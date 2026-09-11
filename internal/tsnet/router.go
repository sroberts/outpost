// Package tsnet gives each service its own Tailscale identity (§7).
//
// One tsnet.Server per service, named by the manifest's tailscale_name, with
// node state under <stateDir>/<service>/ so identities survive an Outpost
// restart instead of re-registering. Each node listens on the tailnet and
// reverse-proxies to the service on loopback, so the supervised process itself
// never binds anything but 127.0.0.1 — nothing is reachable except through
// Tailscale, which is the point.
//
// Bringing a node up is slow: it does network I/O and, on a first run without
// an auth key, waits for someone to visit a login URL. Attach is called from
// the supervisor's reconcile loop, which must not stall, so Attach only records
// intent — a per-service goroutine does the work and retries until it succeeds
// or the service is detached.
package tsnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/tailnet"
)

const (
	// dialTimeout bounds a single proxied connection attempt to the service.
	dialTimeout = 5 * time.Second
	// Retry pacing for a node that will not come up.
	defaultRetryInitial = 2 * time.Second
	defaultRetryMax     = 60 * time.Second

	// defaultShutdownGrace bounds how long Detach waits for an attach
	// goroutine to notice it was cancelled.
	defaultShutdownGrace = 10 * time.Second
)

// Options configures a Router.
type Options struct {
	// StateDir holds per-service node state (~/.outpost/tsnet).
	StateDir string
	// AuthKey registers new nodes unattended. Empty is valid: tsnet then logs a
	// one-time login URL, which is the normal first-run flow.
	AuthKey string
	// Logf receives operator-facing lines.
	Logf func(format string, args ...any)
	// Verbose passes tsnet's own (noisy) logging through to Logf.
	Verbose bool
}

// tailnetNode is one live Tailscale node. The real implementation wraps
// tsnet.Server; tests substitute a fake so the router's lifecycle can be
// exercised without credentials or a tailnet.
type tailnetNode interface {
	Listener() net.Listener
	Close() error
}

// nodeAddrs is an optional extra a tailnetNode may implement: what the node
// ended up with on the tailnet, which only the real backend knows. Keeping it
// off tailnetNode means the test fakes need not grow a method they have
// nothing to say through.
type nodeAddrs interface {
	TailnetAddrs() (addr, dnsName string)
}

// nodeConfig is everything needed to bring one node up.
type nodeConfig struct {
	Service  string
	Hostname string
	Dir      string
	AuthKey  string
	Verbose  bool
	// OnLoginURL receives a login URL parsed out of the backend's own logging
	// and reports whether it is worth a log line. It is bound to the specific
	// node this config was built for, so a server left running by an earlier
	// attach cannot write onto the node that replaced it. Nil in tests, which
	// swap out startNode entirely.
	OnLoginURL func(url string) bool
}

// Router owns one tsnet node per attached service.
type Router struct {
	stateDir string
	authKey  string
	logf     func(string, ...any)
	verbose  bool

	// startNode brings a node up. Swapped out in tests.
	startNode func(ctx context.Context, cfg nodeConfig) (tailnetNode, error)

	retryInitial  time.Duration
	retryMax      time.Duration
	shutdownGrace time.Duration

	// now is time.Now, swapped out in tests so the login-reminder throttle can
	// be exercised without sleeping through it.
	now func() time.Time

	mu    sync.Mutex
	nodes map[string]*node
}

// node tracks one service's Tailscale presence, including one that is still
// being brought up.
type node struct {
	name     string
	hostname string
	port     int

	// cancel stops the attach goroutine; done closes when it has exited.
	cancel context.CancelFunc
	done   chan struct{}

	// mu guards the fields below, which the attach goroutine fills in.
	mu   sync.Mutex
	live tailnetNode
	http *http.Server

	// state and friends are what the router has observed, as opposed to what
	// was configured: the node's own account of itself, for NodeStatus.
	state       tailnet.State
	loginURL    string
	loginLogged time.Time // when loginURL was last written to the log
	detail      string
	addr        string
	dnsName     string
	since       time.Time
}

// New builds a Router. It does not contact Tailscale until Attach is called.
func New(opts Options) (*Router, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("tsnet: StateDir is required")
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnet: create state dir: %w", err)
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	r := &Router{
		stateDir:      opts.StateDir,
		authKey:       opts.AuthKey,
		logf:          logf,
		verbose:       opts.Verbose,
		retryInitial:  defaultRetryInitial,
		retryMax:      defaultRetryMax,
		shutdownGrace: defaultShutdownGrace,
		now:           time.Now,
		nodes:         map[string]*node{},
	}
	r.startNode = r.startTsnetNode
	return r, nil
}

// StateDirFor is where one service's node identity is persisted.
func (r *Router) StateDirFor(service string) string {
	return filepath.Join(r.stateDir, service)
}

// Attached reports the services currently live on the tailnet, sorted. A
// service still being brought up is not yet attached.
func (r *Router) Attached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.nodes))
	for name, n := range r.nodes {
		n.mu.Lock()
		live := n.state == tailnet.StateAttached
		n.mu.Unlock()
		if live {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Attach brings a service onto the tailnet, proxying tailnet traffic to
// 127.0.0.1:<port>.
//
// It returns as soon as the intent is recorded — the node comes up in the
// background and is retried until it succeeds or the service is detached.
// Attaching an already-attached service with the same configuration is a no-op,
// so the reconcile loop may call it on every tick.
func (r *Router) Attach(ctx context.Context, svc *manifest.Service) error {
	if svc.Funnel {
		// Belt and braces: manifest validation rejects this too (§7).
		return fmt.Errorf("%s: funnel is not supported in v1", svc.Name)
	}
	hostname := svc.TailscaleName
	if hostname == "" {
		return fmt.Errorf("%s: tailscale_name is required to join the tailnet", svc.Name)
	}

	r.mu.Lock()
	if existing, ok := r.nodes[svc.Name]; ok {
		if existing.port == svc.Port && existing.hostname == hostname {
			// Already attached, or on its way. Nothing to do.
			r.mu.Unlock()
			return nil
		}
		// A changed port or hostname needs a fresh node. Claim the slot first
		// so a concurrent Attach cannot start a second one, then tear the old
		// one down outside the lock.
		delete(r.nodes, svc.Name)
		r.mu.Unlock()
		_ = r.shutdownNode(existing)
		r.mu.Lock()
	}

	// The entry is registered before any slow work starts, so concurrent
	// callers see it and return rather than building a second node against the
	// same state directory.
	nodeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &node{
		name:     svc.Name,
		hostname: hostname,
		port:     svc.Port,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	r.nodes[svc.Name] = n
	r.mu.Unlock()

	go r.bringUp(nodeCtx, n)
	return nil
}

// bringUp starts the node, retrying with backoff until it succeeds or the
// node's context is cancelled by Detach or Close.
func (r *Router) bringUp(ctx context.Context, n *node) {
	defer close(n.done)

	n.mu.Lock()
	n.state, n.since = tailnet.StateStarting, r.now()
	n.mu.Unlock()

	// Every input here is fixed for the life of the node: the router's auth key
	// and verbosity are set once in New, and the node's name and hostname never
	// change (a changed hostname gets a fresh node). OnLoginURL is bound to this
	// node rather than to its name, so a backend still logging after the node
	// was replaced cannot report onto its successor.
	cfg := nodeConfig{
		Service:    n.name,
		Hostname:   n.hostname,
		Dir:        r.StateDirFor(n.name),
		AuthKey:    r.authKey,
		Verbose:    r.verbose,
		OnLoginURL: func(url string) bool { return r.noteLoginURLFor(n, url) },
	}

	backoff := r.retryInitial
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}

		live, err := r.startNode(ctx, cfg)
		if err == nil {
			if r.serve(ctx, n, live) {
				return
			}
			// Detached while the node was coming up.
			live.Close()
			return
		}

		if ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		n.state, n.detail, n.since = tailnet.StateFailed, err.Error(), r.now()
		// The attempt that was waiting on authorisation is over, so its URL is
		// spent; offering a stale login link is worse than offering none. The
		// next attempt reinstates one if the node is still unauthorised.
		n.loginURL, n.loginLogged = "", time.Time{}
		n.mu.Unlock()

		// A service that cannot reach the tailnet keeps running on loopback;
		// losing tailnet exposure is not a reason to take it down (§7).
		r.logf("%s: tailnet attach failed (attempt %d): %v; retrying in %s", n.name, attempt, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > r.retryMax {
			backoff = r.retryMax
		}
	}
}

// serve installs a freshly started node and begins proxying. It reports false
// if the node was detached while it was being brought up, in which case the
// caller must close it rather than leak it.
func (r *Router) serve(ctx context.Context, n *node, live tailnetNode) bool {
	if ctx.Err() != nil {
		return false
	}

	// Only adopt the node if this entry is still the router's current one.
	r.mu.Lock()
	current, ok := r.nodes[n.name]
	r.mu.Unlock()
	if !ok || current != n {
		return false
	}

	srv := &http.Server{
		Handler:           newProxy(n.port),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Only the real backend knows what the tailnet handed the node; a fake
	// that says nothing simply leaves the address unknown.
	var addr, dnsName string
	if a, ok := live.(nodeAddrs); ok {
		addr, dnsName = a.TailnetAddrs()
	}

	n.mu.Lock()
	n.live = live
	n.http = srv
	n.state, n.since = tailnet.StateAttached, r.now()
	n.addr, n.dnsName = addr, dnsName
	// Authorisation is done with, so the URL and its throttle go: offering a
	// spent login link is worse than offering none.
	n.loginURL, n.detail = "", ""
	n.loginLogged = time.Time{}
	n.mu.Unlock()

	r.logf("%s: on the tailnet as %s -> 127.0.0.1:%d", n.name, n.hostname, n.port)

	go func() {
		if err := srv.Serve(live.Listener()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if ctx.Err() == nil {
				r.logf("%s: tailnet listener stopped: %v", n.name, err)
			}
		}
	}()
	return true
}

// Detach removes a service from the tailnet, including one whose attach is
// still in flight. Node state is left on disk so the same identity is reused
// when the service comes back.
func (r *Router) Detach(ctx context.Context, name string) error {
	r.mu.Lock()
	n, ok := r.nodes[name]
	delete(r.nodes, name)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return r.shutdownNode(n)
}

// Close detaches every service.
func (r *Router) Close() error {
	r.mu.Lock()
	nodes := make([]*node, 0, len(r.nodes))
	for _, n := range r.nodes {
		nodes = append(nodes, n)
	}
	r.nodes = map[string]*node{}
	r.mu.Unlock()

	var failures []string
	for _, n := range nodes {
		if err := r.shutdownNode(n); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", n.name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("tsnet close: %s", strings.Join(failures, "; "))
	}
	return nil
}

// shutdownNode stops a node's attach goroutine and closes whatever it built.
func (r *Router) shutdownNode(n *node) error {
	n.cancel()
	// Wait for the attach goroutine, so a node that arrives late is closed by
	// bringUp rather than leaked -- but not forever. Detach runs on the
	// reconcile loop, and a startNode that does not honour cancellation would
	// otherwise wedge it indefinitely: the same hazard the non-blocking Attach
	// exists to avoid. Giving up on the wait costs ordering, not the leak
	// guarantee, because bringUp closes a late node whether or not anyone is
	// still listening.
	select {
	case <-n.done:
	case <-time.After(r.shutdownGrace):
		r.logf("%s: tailnet attach did not stop within %s; detaching anyway", n.name, r.shutdownGrace)
	}

	n.mu.Lock()
	srv, live := n.http, n.live
	n.http, n.live = nil, nil
	n.state, n.addr, n.dnsName = tailnet.StateOffline, "", ""
	n.loginURL, n.loginLogged, n.detail = "", time.Time{}, ""
	n.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	if live != nil {
		// http.Server.Shutdown has already closed the listener it was serving,
		// so a second close on the same listener is expected, not a failure.
		if err := live.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

// startTsnetNode is the real backend: a tsnet.Server listening on the tailnet.
func (r *Router) startTsnetNode(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create tsnet state dir: %w", err)
	}

	server := &tsnet.Server{
		Dir:       cfg.Dir,
		Hostname:  cfg.Hostname,
		AuthKey:   cfg.AuthKey,
		Ephemeral: false,
		Logf:      r.tsnetLogf(cfg.Service),
		UserLogf: func(format string, args ...any) {
			// Formatted here and passed as a value: tsnet's text is not ours,
			// so it must never be spliced into a format string.
			line := fmt.Sprintf(format, args...)
			url, isLogin := parseLoginURL(line)
			if !isLogin {
				r.logf("%s: %s", cfg.Service, line)
				return
			}
			// tsnet repeats this every five seconds for as long as nobody
			// authorises the node; noteLoginURL decides when that is news.
			note := cfg.OnLoginURL
			if note == nil {
				note = func(u string) bool { return r.noteLoginURL(cfg.Service, u) }
			}
			if note(url) {
				r.logf("%s: waiting to be authorised onto the tailnet: approve it at %s, or set a Tailscale auth key and restart", cfg.Service, url)
			}
		},
	}

	if err := server.Start(); err != nil {
		server.Close()
		return nil, fmt.Errorf("start tsnet node %q: %w", cfg.Hostname, err)
	}
	// Block until the node is actually usable, so a node that never registers
	// is retried rather than reported as attached.
	st, err := server.Up(ctx)
	if err != nil {
		server.Close()
		return nil, fmt.Errorf("tsnet node %q did not come up: %w", cfg.Hostname, err)
	}

	ln, err := server.Listen("tcp", ":80")
	if err != nil {
		server.Close()
		return nil, fmt.Errorf("listen on tailnet: %w", err)
	}
	addr, dnsName := tailnetAddrs(st)
	return &tsnetNode{server: server, ln: ln, addr: addr, dnsName: dnsName}, nil
}

// tailnetAddrs reads the address and registered name out of the status Up
// already returned, which is where they arrive for free. Either may be empty:
// a node can be up before control has told it everything.
func tailnetAddrs(st *ipnstate.Status) (addr, dnsName string) {
	if st == nil {
		return "", ""
	}
	if len(st.TailscaleIPs) > 0 {
		addr = st.TailscaleIPs[0].String()
	}
	if st.Self != nil {
		// DNSName is an FQDN and ends with a dot; operators do not read it
		// that way.
		dnsName = strings.TrimSuffix(st.Self.DNSName, ".")
	}
	return addr, dnsName
}

// tsnetNode adapts tsnet.Server to tailnetNode.
type tsnetNode struct {
	server  *tsnet.Server
	ln      net.Listener
	addr    string
	dnsName string
}

func (n *tsnetNode) Listener() net.Listener { return n.ln }

// TailnetAddrs satisfies nodeAddrs: values captured when the node came up, so
// reading them does no I/O.
func (n *tsnetNode) TailnetAddrs() (string, string) { return n.addr, n.dnsName }

func (n *tsnetNode) Close() error {
	n.ln.Close()
	return n.server.Close()
}

// tsnetLogf keeps tsnet's internal chatter out of the daemon's output unless
// verbose logging was asked for; a first run still surfaces the login URL,
// which arrives via UserLogf.
func (r *Router) tsnetLogf(service string) func(string, ...any) {
	if !r.verbose {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		// Formatted here rather than spliced into the format string: the text
		// comes from tsnet, and a stray %s in it would otherwise be read as a
		// verb. Same reason UserLogf does it this way.
		r.logf("%s: %s", service, fmt.Sprintf(format, args...))
	}
}
