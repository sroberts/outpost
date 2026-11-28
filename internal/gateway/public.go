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

	"github.com/sroberts/outpost/internal/auth"
)

// The public listener is where a node with Funnel sends what arrives from the
// internet. It is its own loopback listener, with its own handler, and that
// handler has code for two things: an app's public_paths, for an app with
// funnel on, with no identity; and an app's mcp_path, for an app with
// mcp_funnel on, with an access token made for the internet and that app. The
// portal, sign-in, sessions, every other token, the portal's MCP and
// /.outpost/ aren't wired into it, so no bug in them can be reached from the
// internet. See docs/design/mcp-over-funnel.md.

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
	g.observe(w, r, hostname(r.Host), true, publicClient, g.servePublicRequest)
}

// servePublicRequest is servePublic once the request has its id.
func (g *Gateway) servePublicRequest(w http.ResponseWriter, r *http.Request) {
	client := publicClient(r)
	now := g.now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		g.lg("ip", client, "method", r.Method, "host", hostname(r.Host), "path", r.URL.Path, "status", rec.status, "took", g.now().Sub(now).Round(time.Microsecond)).Infof("gateway: funnel: %s %s %s%s -> %d", client, r.Method, hostname(r.Host), r.URL.Path, rec.status)
	}()
	// Ahead of the throttle, which serveFunnelMCP applies itself: Claude's
	// connectors call from addresses every Anthropic customer shares, so
	// anyone could keep them backed off, and a good token must still work.
	if rt, ok := g.funnelMCPRoute(r); ok {
		noteApp(r, rt.Name)
		g.serveFunnelMCP(rec, r, rt, client)
		return
	}
	// The OAuth addresses beside it, which apply their own throttling for
	// the same reason. See oauth.go.
	if rt, ok := g.funnelOAuthRoute(r); ok {
		noteApp(r, rt.Name)
		g.serveFunnelOAuth(rec, r, rt, client)
		return
	}
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
	noteApp(r, rt.Name)
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

// funnelMCPRoute is the app a request from the internet is for, when it is
// exactly that app's MCP endpoint: the host is the name the app's node
// reports, the app has mcp_funnel on, and the path is its mcp_path, sent
// with nothing escaped, so no %2f, "..", ";" or trailing slash reaches it.
func (g *Gateway) funnelMCPRoute(r *http.Request) (route, bool) {
	host := hostname(r.Host)
	rt, ok := g.routes.forHost(host)
	if !ok || !rt.MCPFunnel || rt.MCPPath == "" || rt.Host == "" || rt.Host != host {
		return route{}, false
	}
	if r.URL.Path != rt.MCPPath || r.URL.EscapedPath() != r.URL.Path {
		return route{}, false
	}
	return rt, true
}

// serveFunnelMCP carries a request from the internet to an app's MCP
// endpoint. It takes one access token, made for the internet and scoped to
// this app alone, and nothing else: no session, no public path, no sign-in.
//
// The token is looked up before either throttle is asked, and a good one goes
// through whatever its address owes. That reverses lookupAccessToken's rule,
// on this path only, because the callers' addresses are shared: a stranger
// sending bad tokens from them would otherwise hold the owner's connector off.
// A token is 256 random bits, so guessing was never what the throttle
// stopped. Everything else is counted against the address, a request with no
// token in publicThrottle as servePublic's refusals are, and a bad token in
// tokenThrottle, and gets an empty 401, or a 429 while the address owes time.
// Refusals stay out of the activity feed, which scanners would flood; the
// funnel log line has each one.
func (g *Gateway) serveFunnelMCP(w http.ResponseWriter, r *http.Request, rt route, client string) {
	presented := presentedAccessTokens(r.Header)
	// Whatever happens next, the token goes no further than here.
	removeAccessTokens(r.Header)
	now := g.now()

	var (
		tok  auth.AccessToken
		user auth.User
		good bool
	)
	if len(presented) == 1 {
		t, u, err := g.auth.LookupAccessToken(presented[0])
		switch {
		case err == nil:
			tok, user = t, u
			good = t.Internet && len(t.Apps) == 1 && t.Apps[0] == rt.Name
		case !errors.Is(err, auth.ErrNoSuchAccessToken):
			g.lg("service", rt.Name, "err", err).Errorf("gateway: funnel mcp %s: access token lookup: %v", rt.Name, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if !good {
		tokenKey := "ip:" + client
		if d := max(g.publicThrottle.wait(now, client), g.tokenThrottle.wait(now, tokenKey)); d > 0 {
			w.Header().Set("Retry-After", retryAfter(d))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if len(presented) == 0 {
			g.publicThrottle.fail(now, client)
		} else {
			g.tokenThrottle.fail(now, tokenKey)
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// The one answer that isn't a 401, as on the portal's MCP route: only
	// someone holding a good token can get it.
	if !allowed(user, rt) {
		http.Error(w, "this account can't open this app", http.StatusForbidden)
		return
	}
	// What servePublic does after its own checks, which this path is taken
	// ahead of. Streamable HTTP never upgrades.
	if r.Header.Get("Upgrade") != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.ContentLength > MaxPublicBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxPublicBody)
	g.touchAccessToken(tok)
	if tok.InternetUsedAt.IsZero() {
		if first, err := g.auth.MarkInternetUsed(tok.ID); err != nil {
			g.lg("service", rt.Name, "err", err).Warnf("gateway: funnel mcp %s: %v", rt.Name, err)
		} else if first {
			g.activity.record(EventTokenInternet, "internet access token %d for %s first used from the internet, from %s, to reach %s",
				tok.ID, user.Username, client, rt.Name)
		}
	}
	// proxyAsTo removes the Outpost cookies and sets identity on the way out.
	g.proxyAsTo(w, r, rt, user, target{host: r.Host, client: client, public: true, funnelMCP: true})
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
