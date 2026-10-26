package gateway

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// trusted reports whether the request came straight from a trusted proxy.
func (g *Gateway) trusted(r *http.Request) bool {
	a, ok := remoteAddr(r)
	return ok && g.isProxy(a)
}

func (g *Gateway) isProxy(a netip.Addr) bool {
	for _, p := range g.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// rawHost is the host the client asked for, port included: Host, or the
// first X-Forwarded-Host when a trusted proxy sent the request.
func (g *Gateway) rawHost(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" && g.trusted(r) {
		first, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(first)
	}
	return r.Host
}

// hostOf is the requested host name: lower case, without port or a trailing
// dot.
func (g *Gateway) hostOf(r *http.Request) string {
	return hostname(g.rawHost(r))
}

func hostname(raw string) string {
	h := raw
	if host, _, err := net.SplitHostPort(raw); err == nil {
		h = host
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// clientIP is the address of whoever is on the other end: the peer, or,
// when the peer is a trusted proxy, the nearest untrusted address in
// X-Forwarded-For. Throttling keys on it, so a client can't pick its own.
func (g *Gateway) clientIP(r *http.Request) string {
	a, ok := remoteAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if !g.isProxy(a) {
		return a.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		hop = hop.Unmap()
		if !g.isProxy(hop) {
			return hop.String()
		}
		a = hop
	}
	return a.String()
}

// isLocalHost reports whether a request names the gateway by address rather
// than by a name it serves, as a service on this machine fetching the JWKS
// does.
func isLocalHost(h string) bool {
	if h == "localhost" {
		return true
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}
