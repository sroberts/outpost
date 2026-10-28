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
//
// Each node runs in its own child process (see process.go), so a Tailscale
// panic costs one service its tailnet exposure rather than taking down the
// daemon. A node that dies is brought back up the same way it first came up.
package tsnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
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
	// Upstream, when set, is where nodes send tailnet traffic instead of the
	// service: the login gateway, as a URL. Nodes then serve HTTPS, so that
	// every request on the tailnet passes the gateway's login.
	Upstream string
	// UpstreamServerName verifies an https Upstream's certificate.
	UpstreamServerName string
}

// tailnetNode is one live Tailscale node. The real implementation is a child
// process running tsnet.Server; tests substitute a fake so the router's
// lifecycle can be exercised without credentials or a tailnet.
//
// A node whose Listener is nil serves the tailnet itself, as the child
// process does; otherwise the router serves the proxy on it.
type tailnetNode interface {
	Listener() net.Listener
	Close() error
}

// exitingNode is a node that can stop on its own -- a child process that
// crashed. The router brings such a node back up rather than reporting it
// attached forever.
type exitingNode interface {
	Done() <-chan struct{}
}

// nodeConfig is everything needed to bring one node up.
type nodeConfig struct {
	Service  string
	Hostname string
	Dir      string
	AuthKey  string
	Verbose  bool
	// Port is the service's loopback port, which a node process proxies to.
	Port int
	// Upstream and UpstreamServerName are Options' own.
	Upstream           string
	UpstreamServerName string
}

// Router owns one tsnet node per attached service.
type Router struct {
	stateDir string
	authKey  string
	logf     func(string, ...any)
	verbose  bool

	upstream           string
	upstreamServerName string

	// startNode brings a node up. Swapped out in tests.
	startNode func(ctx context.Context, cfg nodeConfig) (tailnetNode, error)
	// nodeCmd builds the child process for a node. Swapped out in tests.
	nodeCmd       func(cfg nodeConfig) (*exec.Cmd, error)
	nodeStopGrace time.Duration

	retryInitial  time.Duration
	retryMax      time.Duration
	shutdownGrace time.Duration

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
	mu       sync.Mutex
	live     tailnetNode
	http     *http.Server
	attached bool
	// dnsName is what the node reported it was given, "" if it didn't.
	dnsName string
}

// hostedNode is a node that knows its DNS name on the tailnet.
type hostedNode interface {
	Host() string
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
		nodeCmd:       defaultNodeCmd,
		nodeStopGrace: defaultNodeStopGrace,
		nodes:         map[string]*node{},
	}
	r.upstream, r.upstreamServerName = opts.Upstream, opts.UpstreamServerName
	r.startNode = r.startProcessNode
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
		live := n.attached
		n.mu.Unlock()
		if live {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Hosts maps each attached service to the DNS name its node was given, for
// the services whose node reported one.
func (r *Router) Hosts() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := map[string]string{}
	for name, n := range r.nodes {
		n.mu.Lock()
		if n.attached && n.dnsName != "" {
			out[name] = n.dnsName
		}
		n.mu.Unlock()
	}
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

	cfg := nodeConfig{
		Service:  n.name,
		Hostname: n.hostname,
		Dir:      r.StateDirFor(n.name),
		AuthKey:  r.authKey,
		Verbose:  r.verbose,
		Port:     n.port,

		Upstream:           r.upstream,
		UpstreamServerName: r.upstreamServerName,
	}

	backoff := r.retryInitial
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return
		}

		live, err := r.startNode(ctx, cfg)
		if err == nil {
			if !r.serve(ctx, n, live) {
				// Detached while the node was coming up.
				_ = live.Close() // nobody is left to report a close failure to
				return
			}
			exiting, ok := live.(exitingNode)
			if !ok {
				return
			}
			upSince := time.Now()
			select {
			case <-ctx.Done():
				// Detach or Close owns the node from here.
				return
			case <-exiting.Done():
			}
			r.unserve(n, live)
			if ctx.Err() != nil {
				return
			}
			// A node that stayed up a while earns a fresh backoff; one that
			// dies as soon as it starts keeps backing off, so a crash loop
			// does not spin.
			if time.Since(upSince) > r.retryMax {
				backoff = r.retryInitial
			}
			err = errors.New("the node process exited")
		}

		if ctx.Err() != nil {
			return
		}
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

	ln := live.Listener()
	var srv *http.Server
	if ln != nil {
		srv = &http.Server{
			Handler:           newProxy(n.port),
			ReadHeaderTimeout: 30 * time.Second,
		}
	}

	n.mu.Lock()
	n.live = live
	n.http = srv
	n.attached = true
	n.dnsName = ""
	if h, ok := live.(hostedNode); ok {
		n.dnsName = h.Host()
	}
	n.mu.Unlock()

	shown := n.hostname
	if h, ok := live.(hostedNode); ok && h.Host() != "" {
		shown = h.Host()
	}
	if r.upstream != "" {
		r.logf("%s: on the tailnet as https://%s -> gateway -> 127.0.0.1:%d", n.name, shown, n.port)
	} else {
		r.logf("%s: on the tailnet as %s -> 127.0.0.1:%d", n.name, shown, n.port)
	}

	if srv == nil {
		// The node serves the tailnet itself.
		return true
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if ctx.Err() == nil {
				r.logf("%s: tailnet listener stopped: %v", n.name, err)
			}
		}
	}()
	return true
}

// unserve forgets a node that stopped on its own, so the service is no longer
// reported attached while a replacement comes up.
func (r *Router) unserve(n *node, live tailnetNode) {
	n.mu.Lock()
	if n.live == live {
		n.live, n.http, n.attached, n.dnsName = nil, nil, false, ""
	}
	n.mu.Unlock()
	// The process has already exited; Close only reaps what is left.
	_ = live.Close()
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
	n.http, n.live, n.attached = nil, nil, false
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
