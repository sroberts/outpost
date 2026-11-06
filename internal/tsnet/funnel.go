package tsnet

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Funnel carries requests from the public internet, which reach a node with
// funnel through Tailscale Funnel, to the gateway's public listener. That
// listener answers only the app's public_paths; nothing here decides what is
// public. What this end does is keep the internet's requests apart from the
// tailnet's: they arrive on a listener of their own, and go to an upstream
// of their own.

// FunnelReadHeaderTimeout, FunnelReadTimeout and FunnelWriteTimeout bound a
// request from the internet. A webhook sender gives up well before a minute.
const (
	FunnelReadHeaderTimeout = 10 * time.Second
	FunnelReadTimeout       = 60 * time.Second
	FunnelWriteTimeout      = 75 * time.Second
)

type funnelClientKey struct{}

// WithFunnelClient records the address of the client a Funnel connection
// came from, which the node reads from Tailscale's own record of it; the
// connection's peer is the Funnel relay.
func WithFunnelClient(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, funnelClientKey{}, addr)
}

// NewFunnelHandler is what a node serves on its Funnel listener: requests for
// its own name go to target, the gateway's public listener, with the
// client's address as the only X-Forwarded-For. Upgrades are refused, since
// webhooks don't need them and a held-open connection from the internet is
// all cost.
func NewFunnelHandler(ownName string, target *url.URL, serverName string) http.Handler {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ResponseHeaderTimeout: FunnelReadTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	if serverName != "" {
		transport.TLSClientConfig = &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// Rewrite has already dropped whatever X-Forwarded-* the client
			// sent; this is the only account of who called.
			if client, _ := pr.In.Context().Value(funnelClientKey{}).(string); client != "" {
				pr.Out.Header.Set("X-Forwarded-For", client)
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			// Nothing about the inside for the internet.
			http.Error(w, "unavailable", http.StatusBadGateway)
		},
	}
	own := strings.TrimSuffix(strings.ToLower(ownName), ".")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if own == "" || funnelHost(r.Host) != own {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Upgrade") != "" || headerHasToken(r.Header, "Connection", "upgrade") {
			http.Error(w, "upgrades are not served here", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func funnelHost(raw string) string {
	h := raw
	if host, _, err := net.SplitHostPort(raw); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// headerHasToken reports whether any of name's comma-separated values is
// token, ignoring case.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
