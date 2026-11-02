package tsnet

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// serviceHeaderTimeout bounds how long a node that proxies straight to a
// service waits for its response headers. A variable so a test can shorten it.
var serviceHeaderTimeout = 60 * time.Second

// newProxy builds the reverse proxy that carries tailnet requests to the
// service on loopback.
//
// This is deliberately separate from the tsnet node so it can be tested
// against an ordinary local server, without a tailnet (§13).
func newProxy(port int) *httputil.ReverseProxy {
	return newProxyWith(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}, "", serviceHeaderTimeout)
}

// newProxyTo builds the proxy a node serves in front of the login gateway.
// serverName is the name to verify an https upstream's certificate against,
// since it is dialled by address.
//
// It sets no header timeout: the hop is loopback, and the gateway already
// decides how long to wait for the app behind it. A second, shorter limit
// here cut off anything the app took over a minute to answer, such as a long
// MCP tool call, with a 502 the gateway never chose.
func newProxyTo(target *url.URL, serverName string) *httputil.ReverseProxy {
	return newProxyWith(target, serverName, 0)
}

// newProxyWith is the proxy both build. headerTimeout of 0 waits for response
// headers for as long as the request lasts.
func newProxyWith(target *url.URL, serverName string, headerTimeout time.Duration) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	if serverName != "" {
		transport.TLSClientConfig = &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Preserve the tailnet hostname the client used, so services that
			// generate absolute URLs produce reachable ones, and so the
			// gateway can tell which app was asked for.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		// Flush as data arrives, so server-sent events stream.
		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A service that is down or restarting is the common case here, so
			// say so plainly rather than returning a bare 500.
			http.Error(w, fmt.Sprintf("outpost: service on %s is not reachable: %v", target.Host, err),
				http.StatusBadGateway)
		},
	}
	return proxy
}
