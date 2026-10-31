// Package node runs one Tailscale node: the body of the hidden subcommand the
// daemon starts for each service on the tailnet (see internal/tsnet).
//
// It lives apart from the router so that the daemon process never links a
// tsnet.Server into its own goroutines; a panic in here ends this process
// and nothing else.
package node

import (
	"context"
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
	served := make(chan error, 2)
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

	if _, err := fmt.Fprintln(ready, outposttsnet.ReadyLine); err != nil {
		return fmt.Errorf("report ready: %w", err)
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
