// Package tsnet gives each service its own Tailscale identity (§7).
//
// One tsnet.Server per service, named by the manifest's tailscale_name, with
// node state under <stateDir>/<service>/ so identities survive an Outpost
// restart instead of re-registering. Each node listens on the tailnet and
// reverse-proxies to the service on loopback, so the supervised process itself
// never binds anything but 127.0.0.1 — nothing is reachable except through
// Tailscale, which is the point.
package tsnet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"

	"github.com/sroberts/outpost/internal/manifest"
)

// dialTimeout bounds a single proxied connection attempt to the local service.
const dialTimeout = 5 * time.Second

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

// Router owns one tsnet node per attached service.
type Router struct {
	stateDir string
	authKey  string
	logf     func(string, ...any)
	verbose  bool

	mu    sync.Mutex
	nodes map[string]*node
}

// node is one service's Tailscale presence.
type node struct {
	name     string
	hostname string
	port     int

	server *tsnet.Server
	ln     net.Listener
	http   *http.Server
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
	return &Router{
		stateDir: opts.StateDir,
		authKey:  opts.AuthKey,
		logf:     logf,
		verbose:  opts.Verbose,
		nodes:    map[string]*node{},
	}, nil
}

// StateDirFor is where one service's node identity is persisted.
func (r *Router) StateDirFor(service string) string {
	return filepath.Join(r.stateDir, service)
}

// Attached reports the services currently on the tailnet, sorted.
func (r *Router) Attached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.nodes))
	for name := range r.nodes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Attach brings a service onto the tailnet, proxying tailnet traffic to
// 127.0.0.1:<port>. Attaching an already-attached service is a no-op, so the
// reconcile loop can call it freely.
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
		// A changed port or hostname needs a fresh listener.
		if existing.port == svc.Port && existing.hostname == hostname {
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		if err := r.Detach(ctx, svc.Name); err != nil {
			return err
		}
		r.mu.Lock()
	}
	r.mu.Unlock()

	dir := r.StateDirFor(svc.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: create tsnet state dir: %w", svc.Name, err)
	}

	server := &tsnet.Server{
		Dir:       dir,
		Hostname:  hostname,
		AuthKey:   r.authKey,
		Ephemeral: false,
		Logf:      r.tsnetLogf(svc.Name),
		UserLogf:  func(format string, args ...any) { r.logf("%s: "+format, append([]any{svc.Name}, args...)...) },
	}

	if err := server.Start(); err != nil {
		server.Close()
		return fmt.Errorf("%s: start tsnet node %q: %w", svc.Name, hostname, err)
	}

	ln, err := server.Listen("tcp", ":80")
	if err != nil {
		server.Close()
		return fmt.Errorf("%s: listen on tailnet: %w", svc.Name, err)
	}

	srv := &http.Server{
		Handler:           newProxy(svc.Port),
		ReadHeaderTimeout: 30 * time.Second,
	}
	n := &node{name: svc.Name, hostname: hostname, port: svc.Port, server: server, ln: ln, http: srv}

	r.mu.Lock()
	r.nodes[svc.Name] = n
	r.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			r.logf("%s: tailnet listener stopped: %v", svc.Name, err)
		}
	}()

	r.logf("%s: on the tailnet as %s -> 127.0.0.1:%d", svc.Name, hostname, svc.Port)
	return nil
}

// Detach removes a service from the tailnet. Its node state is left on disk so
// the same identity is reused when the service comes back.
func (r *Router) Detach(ctx context.Context, name string) error {
	r.mu.Lock()
	n, ok := r.nodes[name]
	delete(r.nodes, name)
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return n.close(ctx)
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var failures []string
	for _, n := range nodes {
		if err := n.close(ctx); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", n.name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("tsnet close: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (n *node) close(ctx context.Context) error {
	if n.http != nil {
		n.http.Shutdown(ctx)
	}
	if n.server != nil {
		return n.server.Close()
	}
	return nil
}

// tsnetLogf keeps tsnet's internal chatter out of the daemon's output unless
// verbose logging was asked for; a first run still surfaces the login URL,
// which arrives via UserLogf.
func (r *Router) tsnetLogf(service string) func(string, ...any) {
	if !r.verbose {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		r.logf("%s: "+format, append([]any{service}, args...)...)
	}
}
