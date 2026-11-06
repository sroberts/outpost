package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// The public listener is where a node with Funnel sends what arrives from the
// internet. It is its own loopback listener, with its own handler, and that
// handler has code for one thing: an app's public_paths, for an app with
// funnel on, with no identity. The portal, sign-in, sessions, access tokens,
// MCP and /.outpost/ aren't wired into it, so no bug in them can be reached
// from the internet.

// MaxPublicBody caps a request body from the internet. GitHub's webhooks
// are the largest a sender is known to send, at 25 MB.
const MaxPublicBody = 25 << 20

// publicThrottleFree is how many refusals an address gets before it is
// slowed down. A webhook sender is never refused, so this is for scanners.
const publicThrottleFree = 20

// ServePublic serves the public listener until ctx ends. The daemon binds it
// on loopback and gives its address to the nodes with Funnel; nothing else
// is meant to reach it.
func (g *Gateway) ServePublic(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(g.servePublic),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       time.Minute,
	}
	stop := context.AfterFunc(ctx, func() {
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut) // the listener is closing either way
	})
	defer stop()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (g *Gateway) servePublic(w http.ResponseWriter, r *http.Request) {
	// First, whatever happens next: nobody outside sets who they are.
	stripIdentity(r.Header)
	client := publicClient(r)
	now := g.now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		g.logf("gateway: funnel: %s %s %s%s -> %d", client, r.Method, hostname(r.Host), r.URL.Path, rec.status)
	}()
	refuse := func(status int) {
		g.publicThrottle.fail(now, client)
		// An empty body: nothing about what is or isn't here.
		rec.WriteHeader(status)
	}
	if d := g.publicThrottle.wait(now, client); d > 0 {
		rec.Header().Set("Retry-After", retryAfter(d))
		rec.WriteHeader(http.StatusTooManyRequests)
		return
	}
	// A leaked token must not become a way in from outside, so one is
	// refused rather than passed on as if the request were anonymous.
	if len(presentedAccessTokens(r.Header)) > 0 {
		refuse(http.StatusUnauthorized)
		return
	}
	// The node's own name, as Funnel delivered it: X-Forwarded-Host isn't
	// read, and only an app whose node has that name and funnel on counts.
	host := hostname(r.Host)
	rt, ok := g.routes.forHost(host)
	if !ok || !rt.Funnel || rt.Host == "" || rt.Host != host {
		refuse(http.StatusNotFound)
		return
	}
	if strings.HasPrefix(r.URL.Path, reservedPrefix) || !g.isPublic(r, rt) {
		refuse(http.StatusNotFound)
		return
	}
	if r.Header.Get("Upgrade") != "" {
		refuse(http.StatusBadRequest)
		return
	}
	// A declared length over the cap is refused before the app sees any of
	// it; one sent without a length is cut off where it passes the cap.
	if r.ContentLength > MaxPublicBody {
		rec.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(rec, r.Body, MaxPublicBody)
	removeCookie(r.Header, g.cookieName)
	removeCookie(r.Header, g.stateCookieName())
	ctx := context.WithValue(r.Context(), targetKey{}, target{
		name:   rt.Name,
		port:   rt.Port,
		host:   r.Host,
		client: client,
		public: true,
	})
	g.proxy.ServeHTTP(rec, r.WithContext(ctx))
}

// publicClient is the internet address a request came from. The node sets
// X-Forwarded-For to it alone, from Tailscale's record of the connection,
// and only the node reaches this listener, on loopback; from anywhere else
// the peer is all there is.
func publicClient(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if a, err := netip.ParseAddr(peer); err == nil && a.Unmap().IsLoopback() {
		if fwd, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Forwarded-For"))); err == nil {
			return fwd.Unmap().String()
		}
	}
	return peer
}

// statusRecorder notes the status a handler wrote, for the log line.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Flush passes a flush on, so the proxy can stream.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(d.Round(time.Second)/time.Second)))
}
