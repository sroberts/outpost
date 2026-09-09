package tsnet

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// newProxy builds the reverse proxy that carries tailnet requests to the
// service on loopback.
//
// This is deliberately separate from the tsnet node so it can be tested
// against an ordinary local server, without a tailnet (§13).
func newProxy(port int) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Preserve the tailnet hostname the client used, so services that
			// generate absolute URLs produce reachable ones.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A service that is down or restarting is the common case here, so
			// say so plainly rather than returning a bare 500.
			http.Error(w, fmt.Sprintf("outpost: service on %s is not reachable: %v", target.Host, err),
				http.StatusBadGateway)
		},
	}
	return proxy
}
