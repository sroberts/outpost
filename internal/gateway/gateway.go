// Package gateway is Outpost's login front door: one HTTP server in the
// daemon that serves a portal, where people log in, and each service on its
// own subdomain behind that login, telling the service who is calling.
//
// It reads the service list only through the supervisor's public API, as the
// CLI does, and keeps people, sessions and keys in internal/auth.
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/supervisor"
)

// Services is what the gateway needs from the supervisor.
type Services interface {
	List(ctx context.Context) ([]supervisor.ServiceStatus, error)
}

// Options configure a Gateway.
type Options struct {
	Config   config.Gateway
	Auth     *auth.Store
	Services Services
	Logf     func(format string, v ...any)
	// TailnetHosts, when set, maps services to their own Tailscale DNS
	// names. Each such service is served at https://<that name>, behind the
	// same login, and the portal links there.
	TailnetHosts func() map[string]string
	// Events, when set, receives sign-ins, failures, denials and account
	// changes for the activity feed.
	Events Events
	// Updates, when set, gives admins a page for updating apps.
	Updates Updates
	// SelfUpdates, when set alongside Updates, puts Outpost itself on that
	// page.
	SelfUpdates SelfUpdates
	// Control, when set, gives admins a menu on each app in the catalog to
	// start, stop, restart or back it up.
	Control Control
	// Stats, when set, is where each app's usage is read from.
	Stats Stats
}

// Gateway is the HTTP front door. It is an http.Handler, so tests can serve
// it with httptest; the daemon uses Listen and Serve.
type Gateway struct {
	cfg      config.Gateway
	auth     *auth.Store
	services Services
	logf     func(format string, v ...any)
	now      func() time.Time

	tailnetHosts func() map[string]string
	updater      Updates
	selfUpdater  SelfUpdates
	updateState  updateState
	control      Control
	ops          appOps
	// claim serialises the busy checks of the catalog's actions and the
	// update page against each other; each side's own lock covers only itself.
	claim   sync.Mutex
	stats   Stats
	traffic trafficMeter

	scheme       string // public_url's scheme
	portalHost   string // public_url's host name, lower case
	portalOrigin string // scheme://host[:port], no trailing slash
	port         string // public_url's port, "" for the scheme's default
	domain       string // app hosts are <name>.<domain>
	proxies      []netip.Prefix
	secure       bool
	cookieName   string

	routes   routeTable
	codes    codeStore
	throttle throttle
	// codeThrottle limits callbacks per address. A code is 256 random bits,
	// so this is about load, not guessing, and it lets a few stale links
	// through before backing off.
	codeThrottle throttle
	tokens       tokenCache
	setup        setupState
	proxy        *httputil.ReverseProxy
	pages        pages
	checks       chan struct{} // slots for concurrent password hashing
	activity     *activity

	// tokenThrottle limits rejected access tokens per address. It is its own
	// table so a broken cron job can't lock its machine out of the sign-in
	// page, and token traffic never touches the record of password guesses.
	tokenThrottle throttle

	ln  net.Listener
	srv *http.Server
}

// New builds a gateway from validated settings.
func New(opts Options) (*Gateway, error) {
	if err := (config.Config{Gateway: opts.Config}).Validate(); err != nil {
		return nil, err
	}
	if !opts.Config.Enabled {
		return nil, errors.New("gateway: not enabled")
	}
	if opts.Auth == nil || opts.Services == nil {
		return nil, errors.New("gateway: Auth and Services are required")
	}
	u, err := url.Parse(opts.Config.PublicURL)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		cfg:          opts.Config,
		auth:         opts.Auth,
		services:     opts.Services,
		logf:         opts.Logf,
		now:          time.Now,
		tailnetHosts: opts.TailnetHosts,
		updater:      opts.Updates,
		selfUpdater:  opts.SelfUpdates,
		control:      opts.Control,
		stats:        opts.Stats,
		scheme:       u.Scheme,
		portalHost:   strings.ToLower(u.Hostname()),
		portalOrigin: u.Scheme + "://" + strings.ToLower(u.Host),
		port:         u.Port(),
		domain:       opts.Config.AppDomain(),
		proxies:      opts.Config.Proxies(),
		secure:       u.Scheme == "https",
		checks:       make(chan struct{}, passwordChecks),
		codeThrottle: throttle{free: 5},

		// As for codes: 256 random bits can't be guessed, so this is about
		// load, and it leaves room for a script with a stale token to be told so.
		tokenThrottle: throttle{free: 5},
	}
	if g.logf == nil {
		g.logf = func(string, ...any) {}
	}
	g.activity = newActivity(opts.Events, g.logf, func() time.Time { return g.now() })
	// A __Host- cookie must be Secure, and a browser drops a Secure cookie
	// set over plain http, so http setups use a plain name.
	g.cookieName = "outpost"
	if g.secure {
		g.cookieName = "__Host-outpost"
	}
	g.proxy = g.newProxy()
	if g.pages, err = loadPages(); err != nil {
		return nil, err
	}
	// Publish a key before any app asks for the JWKS.
	if _, err := g.auth.CurrentKey(); err != nil {
		return nil, err
	}
	if err := g.setup.open(g.auth); err != nil {
		return nil, err
	}
	return g, nil
}

// Listen binds the configured address. It's separate from Serve so that a
// port already in use fails the daemon's start, not a goroutine.
func (g *Gateway) Listen() error {
	ln, err := net.Listen("tcp", g.cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	g.ln = ln
	return nil
}

// Addr is the bound address, once Listen has run.
func (g *Gateway) Addr() string {
	if g.ln == nil {
		return g.cfg.ListenAddr()
	}
	return g.ln.Addr().String()
}

// Serve runs the gateway until ctx ends.
func (g *Gateway) Serve(ctx context.Context) error {
	if g.ln == nil {
		if err := g.Listen(); err != nil {
			return err
		}
	}
	g.refreshRoutes(ctx)
	go g.keepRoutesFresh(ctx)
	// The daemon closes the stores once Serve returns, so the feed's last
	// writes have to be done by then, however Serve ends.
	activityCtx, stopActivity := context.WithCancel(ctx)
	activityDone := make(chan struct{})
	go func() { defer close(activityDone); g.activity.run(activityCtx) }()
	defer func() { stopActivity(); <-activityDone }()

	g.srv = &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: it would cut off server-sent events and long
		// downloads that the proxy carries.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.srv.Shutdown(shutCtx) // the listener is closing either way
	}()

	var err error
	if g.cfg.TLSCert != "" {
		g.srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = g.srv.ServeTLS(g.ln, g.cfg.TLSCert, g.cfg.TLSKey)
	} else {
		err = g.srv.Serve(g.ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// PortalURL is the portal's address as browsers see it.
func (g *Gateway) PortalURL() string { return g.portalOrigin }

// SetupURL is the one-time first-run link, or "" once there are users.
func (g *Gateway) SetupURL() string {
	tok := g.setup.current()
	if tok == "" {
		return ""
	}
	return g.portalOrigin + "/setup?token=" + url.QueryEscape(tok)
}

// ServeHTTP routes a request by its host.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// First, whatever happens next: nobody outside sets who they are.
	stripIdentity(r.Header)

	host := g.hostOf(r)
	switch {
	case host == g.portalHost:
		// Tokens reach apps, not the place accounts are managed from.
		if len(presentedAccessTokens(r.Header)) > 0 {
			g.refuseAccessToken(w, r)
			return
		}
		g.servePortal(w, r)
	case r.URL.Path == jwksPath && isLocalHost(host):
		// Apps fetch keys from loopback, without leaving the machine.
		g.serveJWKS(w, r)
	default:
		if rt, ok := g.appFor(host); ok {
			g.serveApp(w, r, rt)
			return
		}
		// No detail: a probe shouldn't learn which apps exist.
		http.NotFound(w, r)
	}
}

// appFor maps a host to its service: its tailnet address, its hostname, or
// <subdomain>.<domain>.
func (g *Gateway) appFor(host string) (route, bool) {
	return g.routes.forHost(host)
}

// appHost is the host name an app is served at.
func (g *Gateway) appHost(name string) string {
	if rt, ok := g.routes.get(name); ok {
		if rt.Host != "" {
			return rt.Host
		}
		return rt.Addr
	}
	return name + "." + g.domain
}

// appOrigin is an app's origin as browsers see it.
func (g *Gateway) appOrigin(name string) string {
	if rt, ok := g.routes.get(name); ok && rt.Host != "" {
		// The node serves HTTPS on the default port, whatever the portal's.
		return "https://" + rt.Host
	}
	h := g.appHost(name)
	if g.port != "" {
		h = net.JoinHostPort(h, g.port)
	}
	return g.scheme + "://" + h
}

// stripIdentity removes every header an app could read as X-Outpost-*. The
// name is matched with its punctuation taken out: a CGI-style server folds
// "X_Outpost_User" into the same HTTP_X_OUTPOST_USER as the real header,
// and Go passes such names on as sent.
func stripIdentity(h http.Header) {
	for k := range h {
		if isIdentityName(k) {
			delete(h, k)
		}
	}
}

func isIdentityName(name string) bool {
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteRune(c + 'a' - 'A')
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		}
	}
	return strings.HasPrefix(b.String(), "xoutpost")
}

// ServiceEnv is what every service is told about the gateway: that identity
// headers come from it, who issues the token, and where its keys are.
func ServiceEnv(cfg config.Gateway) []string {
	if !cfg.Enabled {
		return nil
	}
	issuer := strings.TrimSuffix(cfg.PublicURL, "/")
	return []string{
		"OUTPOST_AUTH=gateway",
		"OUTPOST_ISSUER=" + issuer,
		"OUTPOST_JWKS_URL=" + jwksURL(cfg, issuer),
	}
}

// jwksURL is where a service on this machine fetches the keys: over
// loopback when the gateway speaks plain http, since TLS ends in front of
// it; through the public address when it serves TLS itself, because a
// loopback address wouldn't match its certificate.
func jwksURL(cfg config.Gateway, issuer string) string {
	if cfg.TLSCert != "" {
		return issuer + jwksPath
	}
	host, port, err := net.SplitHostPort(cfg.ListenAddr())
	if err != nil {
		return issuer + jwksPath
	}
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + jwksPath
}

// LocalUpstream is how a tailnet node on this machine reaches the gateway:
// its listen address, on loopback when it listens everywhere, and, when it
// serves TLS itself, the name its certificate is for.
func LocalUpstream(cfg config.Gateway) (upstream, serverName string) {
	host, port, err := net.SplitHostPort(cfg.ListenAddr())
	if err != nil {
		return "", ""
	}
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)
	if cfg.TLSCert == "" {
		return "http://" + addr, ""
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return "", ""
	}
	return "https://" + addr, u.Hostname()
}

// setupState is the first-run link: a token that lets one admin be created
// while there are no users. It lives in memory, so a restart replaces it.
type setupState struct {
	mu    sync.Mutex
	token string
}

func (s *setupState) open(store *auth.Store) error {
	users, err := store.ListUsers()
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	tok, err := auth.NewToken()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.token = tok
	s.mu.Unlock()
	return nil
}

func (s *setupState) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}
