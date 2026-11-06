// Package node runs one Tailscale node: the body of the hidden subcommand the
// daemon starts for each service on the tailnet (see internal/tsnet).
//
// It lives apart from the router so that the daemon process never links a
// tsnet.Server into its own goroutines; a panic in here ends this process
// and nothing else.
package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"

	"github.com/sroberts/outpost/internal/manifest"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// Config is one node.
type Config struct {
	Service  string
	Hostname string
	Dir      string
	AuthKey  string
	Port     int
	Verbose  bool
	// Upstream, when set, is the login gateway's URL. The node then serves
	// HTTPS and sends everything there rather than to the service, so the
	// tailnet address is behind the same login as the portal.
	Upstream           string
	UpstreamServerName string
	// TCP is the service's tcp forwards, served on this node.
	TCP []manifest.TCPForward
	// AccessSecret lets the node ask the gateway about each forwarded
	// connection. Required for forwards when Upstream is set.
	AccessSecret string
	// Funnel also answers the public internet, through Tailscale Funnel, on
	// a listener of its own that sends everything to PublicUpstream: the
	// gateway's public listener, which serves only public paths. Needs
	// Upstream.
	Funnel         bool
	PublicUpstream string
}

// Run brings the node up, prints outposttsnet.ReadyLine to ready once it is
// serving, and proxies tailnet traffic to the service until ctx ends. Log
// lines, including the one-time login URL, go to logs.
func Run(ctx context.Context, cfg Config, ready, logs io.Writer) error {
	logf := func(format string, args ...any) { fmt.Fprintf(logs, format+"\n", args...) }
	quiet := func(string, ...any) {}

	server := &tsnet.Server{
		Dir:       cfg.Dir,
		Hostname:  cfg.Hostname,
		AuthKey:   cfg.AuthKey,
		Ephemeral: false,
		// tsnet's own logging is noisy; only the login URL and other
		// user-facing lines come through unless verbose was asked for.
		Logf:     quiet,
		UserLogf: onceEach(logf),
	}
	if cfg.Verbose {
		server.Logf = logf
	}
	// The process is exiting either way; a failure to close is not actionable.
	defer func() { _ = server.Close() }()

	if err := server.Start(); err != nil {
		return fmt.Errorf("start tsnet node %q: %w", cfg.Hostname, err)
	}
	// Block until the node is actually usable, so a node that never registers
	// is retried by the daemon rather than reported as attached. Meanwhile any
	// login URL goes to the daemon too, which shows it in `outpost status`.
	upCtx, stopWatching := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		reportLogin(upCtx, server, ready)
	}()
	status, err := server.Up(ctx)
	// Stopped before anything else is written to ready, which this goroutine
	// also writes to; Fprintln's single write keeps lines whole regardless.
	stopWatching()
	select {
	case <-watched:
	case <-time.After(5 * time.Second):
		// Cancelling should end the bus read at once. If it ever doesn't,
		// coming up matters more than the report; a late login line only
		// names a URL the daemon no longer shows for a node that is on.
	}
	if err != nil {
		return fmt.Errorf("tsnet node %q did not come up: %w", cfg.Hostname, err)
	}
	ownName := ""
	if status.Self != nil && status.Self.DNSName != "" {
		ownName = status.Self.DNSName
		if _, err := fmt.Fprintln(ready, outposttsnet.HostLinePrefix+status.Self.DNSName); err != nil {
			return fmt.Errorf("report host: %w", err)
		}
	}

	var servers []*http.Server
	served := make(chan error, 2+len(cfg.TCP))
	serve := func(ln net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		servers = append(servers, srv)
		go func() { served <- srv.Serve(ln) }()
	}

	if cfg.Upstream == "" {
		ln, err := server.Listen("tcp", ":80")
		if err != nil {
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		serve(ln, outposttsnet.NewProxy(cfg.Port))
	} else {
		target, err := url.Parse(cfg.Upstream)
		if err != nil {
			return fmt.Errorf("upstream %q: %w", cfg.Upstream, err)
		}
		// The gateway's session cookie is Secure, so the app has to be
		// served over HTTPS; the certificate is the tailnet's own.
		tlsLn, err := server.ListenTLS("tcp", ":443")
		if err != nil {
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		ln, err := server.Listen("tcp", ":80")
		if err != nil {
			_ = tlsLn.Close() // failing anyway
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		serve(tlsLn, onlyHost(ownName, outposttsnet.NewUpstreamProxy(target, cfg.UpstreamServerName)))
		serve(ln, onlyHost(ownName, http.HandlerFunc(toHTTPS)))
	}

	if err := forwardTCP(ctx, server, cfg, logf, served); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(ready, outposttsnet.ReadyLine); err != nil {
		return fmt.Errorf("report ready: %w", err)
	}
	// After ready, so a tailnet that doesn't allow Funnel still gets the
	// tailnet side, which nothing here can stop.
	if cfg.Funnel {
		target, err := url.Parse(cfg.PublicUpstream)
		if err != nil || cfg.Upstream == "" {
			return fmt.Errorf("funnel needs the gateway's two listeners; public upstream %q", cfg.PublicUpstream)
		}
		go runFunnel(ctx, server, outposttsnet.NewFunnelHandler(ownName, target, cfg.UpstreamServerName), ready, logf)
	}

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("tailnet listener stopped: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, srv := range servers {
			// Leaving anyway; in-flight requests that do not finish are cut off.
			_ = srv.Shutdown(shutdown)
		}
		return nil
	}
}

// Funnel retry pacing: Tailscale refuses Funnel until the tailnet policy
// grants the node's "funnel" attribute, which someone may do at any time.
var (
	funnelRetryInitial = 30 * time.Second
	funnelRetryMax     = 5 * time.Minute
)

// runFunnel opens the node's Funnel listener and serves it until ctx ends,
// reporting each time it comes up or can't, and trying again after a
// failure. The tailnet listeners are untouched by any of it.
func runFunnel(ctx context.Context, server *tsnet.Server, h http.Handler, report io.Writer, logf func(string, ...any)) {
	say := func(state, why string) {
		line := outposttsnet.FunnelLinePrefix + state
		if why != "" {
			line += " " + strings.Join(strings.Fields(why), " ")
		}
		_, _ = fmt.Fprintln(report, line) // the daemon gone means ctx is ending too
	}
	backoff := funnelRetryInitial
	for ctx.Err() == nil {
		// FunnelOnly: no tailnet connection can arrive on this listener, and
		// none from the internet on the tailnet's, which is the line the
		// gateway's two listeners rest on.
		ln, err := server.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
		if err != nil {
			say(outposttsnet.FunnelUnavailable, err.Error())
		} else {
			say(outposttsnet.FunnelOn, "")
			backoff = funnelRetryInitial
			srv := &http.Server{
				Handler:           h,
				ReadHeaderTimeout: outposttsnet.FunnelReadHeaderTimeout,
				ReadTimeout:       outposttsnet.FunnelReadTimeout,
				WriteTimeout:      outposttsnet.FunnelWriteTimeout,
				IdleTimeout:       time.Minute,
				ConnContext:       funnelClient,
			}
			stop := context.AfterFunc(ctx, func() {
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdown) // leaving anyway
			})
			err := srv.Serve(ln)
			stop()
			if ctx.Err() != nil {
				return
			}
			say(outposttsnet.FunnelUnavailable, "the Funnel listener stopped: "+err.Error())
			logf("funnel: listener stopped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > funnelRetryMax {
			backoff = funnelRetryMax
		}
	}
}

// funnelClient records the client a Funnel connection is from. The
// connection's own peer is the Funnel relay; Tailscale keeps the client's
// address on the connection it hands over, under the TLS layer.
func funnelClient(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if fc, ok := c.(*ipn.FunnelConn); ok && fc.Src.IsValid() {
		return outposttsnet.WithFunnelClient(ctx, fc.Src.Addr().String())
	}
	return ctx
}

// reportLogin prints outposttsnet.LoginLinePrefix and the URL to out each
// time the node is given a new login URL, until ctx ends. tsnet logs the URL
// too, but as prose meant for a person; this is the line the daemon reads.
// The node comes up without it, so a failure here only costs the report.
func reportLogin(ctx context.Context, server *tsnet.Server, out io.Writer) {
	lc, err := server.LocalClient()
	if err != nil {
		return
	}
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return
	}
	defer func() { _ = watcher.Close() }() // finished with it either way
	last := ""
	for {
		n, err := watcher.Next()
		if err != nil {
			return
		}
		if n.BrowseToURL == nil || *n.BrowseToURL == "" || *n.BrowseToURL == last {
			continue
		}
		last = *n.BrowseToURL
		if _, err := fmt.Fprintln(out, outposttsnet.LoginLinePrefix+last); err != nil {
			return
		}
	}
}

// onlyHost passes on requests for this node's own name and refuses the rest.
// The gateway routes on Host and trusts the node as its proxy, so without
// this a peer allowed to reach only this node could name the portal or any
// other app in Host and be sent there. Until the name is known (ownName
// empty) nothing matches, which only refuses requests nobody could have
// addressed to this node by name yet.
func onlyHost(ownName string, next http.Handler) http.Handler {
	own := strings.TrimSuffix(strings.ToLower(ownName), ".")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if own == "" || hostname(r.Host) != own {
			http.Error(w, "outpost: this tailnet node serves only its own name", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostname is the host part of a Host header: lower case, without port or
// a trailing dot.
func hostname(raw string) string {
	h := raw
	if host, _, err := net.SplitHostPort(raw); err == nil {
		h = host
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// toHTTPS sends a plain-http request to the same place over HTTPS.
func toHTTPS(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Scheme = "https"
	u.Host = r.Host
	http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
}

// maxRemembered bounds onceEach's memory. User-facing lines are few and
// mostly identical, so this is only a backstop.
const maxRemembered = 256

// onceEach passes each distinct line through once. tsnet repeats the login
// URL every few seconds until someone visits it, which buried everything
// else in the daemon's log; a restarted node starts afresh and says it again.
func onceEach(logf func(string, ...any)) func(string, ...any) {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mu.Lock()
		if seen[line] {
			mu.Unlock()
			return
		}
		if len(seen) >= maxRemembered {
			seen = map[string]bool{}
		}
		seen[line] = true
		mu.Unlock()
		logf("%s", line)
	}
}

// forwardTCP listens on each of the service's tcp ports and carries what
// arrives to the service. With the gateway on, each connection is first put
// to it, by the caller's tailnet login: the gateway's login can't reach a raw
// stream, and without this a forward would be a way round it.
func forwardTCP(ctx context.Context, server *tsnet.Server, cfg Config, logf func(string, ...any), served chan<- error) error {
	if len(cfg.TCP) == 0 {
		return nil
	}
	var check func(context.Context, string, string) (string, error)
	var identify func(context.Context, string) (outposttsnet.Caller, error)
	if cfg.Upstream != "" {
		checker, err := outposttsnet.NewAccessChecker(cfg.Upstream, cfg.UpstreamServerName, cfg.AccessSecret)
		if err != nil {
			// Refusing to start beats forwarding unchecked.
			return fmt.Errorf("tcp forwards: %w", err)
		}
		check = checker.Check
		lc, err := server.LocalClient()
		if err != nil {
			return fmt.Errorf("tcp forwards: %w", err)
		}
		identify = func(ctx context.Context, remote string) (outposttsnet.Caller, error) {
			who, err := lc.WhoIs(ctx, remote)
			if err != nil {
				return outposttsnet.Caller{}, err
			}
			var c outposttsnet.Caller
			if who.Node != nil {
				c.Tagged = who.Node.IsTagged()
				c.Device = strings.TrimSuffix(who.Node.Name, ".")
			}
			if who.UserProfile != nil {
				c.Login = who.UserProfile.LoginName
			}
			return c, nil
		}
	}
	for _, f := range cfg.TCP {
		ln, err := server.Listen("tcp", fmt.Sprintf(":%d", f.Tailnet))
		if err != nil {
			return fmt.Errorf("listen on tailnet port %d: %w", f.Tailnet, err)
		}
		fw := &outposttsnet.Forwarder{
			Service:  cfg.Service,
			Port:     f.Tailnet,
			Target:   fmt.Sprintf("127.0.0.1:%d", f.Local),
			Identify: identify,
			Check:    check,
			Logf:     logf,
		}
		go func() {
			if err := fw.Serve(ctx, ln); err != nil {
				served <- fmt.Errorf("tcp %d: %w", fw.Port, err)
			}
		}()
	}
	return nil
}
