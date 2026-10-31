package gateway

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// sameOrigin is the CSRF check for every POST: the browser's Origin, or its
// Referer when Origin is missing, must be the origin the request was made
// to, port included. With SameSite=Lax cookies this covers login, logout and
// setup without hidden token fields. A request that names neither is
// refused, since every browser sends one or the other on a form POST.
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
	// The port counts: another port on the same name is another origin,
	// and what answers there may not be the gateway's. Something in front
	// on a different port carries the browser's own host, port and all, in
	// X-Forwarded-Host, which rawHost takes from a trusted proxy.
	return origin(u.Scheme, u.Host) == origin(g.scheme, g.rawHost(r))
}

// origin is scheme://host[:port] spelt one way: lower case, no trailing
// dot, and the scheme's default port left out, as a browser writes Origin.
func origin(scheme, hostport string) string {
	port := ""
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		port = p
	}
	host := hostname(hostport)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	scheme = strings.ToLower(scheme)
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}
