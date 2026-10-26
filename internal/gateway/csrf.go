package gateway

import (
	"net/http"
	"net/url"
)

// sameOrigin is the CSRF check for every POST: the browser's Origin, or its
// Referer when Origin is missing, must be this host on the public scheme.
// With SameSite=Lax cookies this covers login, logout and setup without
// hidden token fields. A request that names neither is refused, since every
// browser sends one or the other on a form POST.
func (g *Gateway) sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" || src == "null" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	// Ports are left out: what's in front may listen on a different one
	// than the gateway, and another port on the same host is the same site.
	return u.Scheme == g.scheme && hostname(u.Host) == g.hostOf(r)
}
