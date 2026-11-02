package tsnet

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hostedFake is a node that serves the tailnet itself, as the child process
// does, and reports the DNS name it was given.
type hostedFake struct {
	host   string
	closed atomic.Bool
}

func (n *hostedFake) Listener() net.Listener { return nil }
func (n *hostedFake) Host() string           { return n.host }
func (n *hostedFake) Close() error           { n.closed.Store(true); return nil }

// portalRouter is a Router in front of a gateway, whose nodes are started by
// start, logging to logs.
func portalRouter(t *testing.T, logs *logSink, start func(ctx context.Context, cfg nodeConfig) (tailnetNode, error)) *Router {
	t.Helper()
	r, err := New(Options{StateDir: t.TempDir(), Upstream: "http://127.0.0.1:8443", Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	r.startNode = start
	r.retryInitial = 5 * time.Millisecond
	r.retryMax = 20 * time.Millisecond
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestAttachPortal_StartsANodeInFrontOfTheGateway(t *testing.T) {
	var mu sync.Mutex
	var got []nodeConfig
	r := portalRouter(t, &logSink{}, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		mu.Lock()
		got = append(got, cfg)
		mu.Unlock()
		return &hostedFake{host: "outpost.example.ts.net"}, nil
	})

	if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err != nil {
		t.Fatalf("AttachPortal: %v", err)
	}
	waitFor(t, 3*time.Second, "the portal's node to come up", func() bool {
		ns, ok := r.NodeStatus(PortalNode)
		return ok && ns.State == NodeOn
	})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("started %d nodes; want 1", len(got))
	}
	cfg := got[0]
	if cfg.Hostname != "outpost" {
		t.Errorf("Hostname = %q; want outpost", cfg.Hostname)
	}
	if cfg.Port != 0 {
		t.Errorf("Port = %d; the portal has no service port", cfg.Port)
	}
	if cfg.Upstream != "http://127.0.0.1:8443" {
		t.Errorf("Upstream = %q; want the gateway", cfg.Upstream)
	}
	if want := filepath.Join(r.stateDir, "_portal"); cfg.Dir != want {
		t.Errorf("Dir = %q; want %q", cfg.Dir, want)
	}
	if ns, _ := r.NodeStatus(PortalNode); ns.Host != "outpost.example.ts.net" {
		t.Errorf("NodeStatus host = %q", ns.Host)
	}
	// It is a node, not a service: nothing that lists services sees it.
	if a := r.Attached(); len(a) != 0 {
		t.Errorf("Attached = %v; the portal is not a service", a)
	}
	if h := r.Hosts(); len(h) != 0 {
		t.Errorf("Hosts = %v; the portal is not a service", h)
	}
}

func TestAttachPortal_DoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	r := portalRouter(t, &logSink{}, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &hostedFake{}, nil
	})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("AttachPortal blocked for %s", elapsed)
	}
	if ns, ok := r.NodeStatus(PortalNode); !ok || ns.State != NodeStarting {
		t.Errorf("NodeStatus = %+v, %v; want starting", ns, ok)
	}
}

func TestAttachPortal_IsIdempotent(t *testing.T) {
	var starts atomic.Int32
	r := portalRouter(t, &logSink{}, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		starts.Add(1)
		return &hostedFake{host: "outpost.example.ts.net"}, nil
	})
	for range 3 {
		if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 3*time.Second, "the portal's node to come up", func() bool {
		ns, _ := r.NodeStatus(PortalNode)
		return ns.State == NodeOn
	})
	if n := starts.Load(); n != 1 {
		t.Errorf("started %d nodes; want 1", n)
	}
}

func TestAttachPortal_CloseStopsIt(t *testing.T) {
	live := &hostedFake{host: "outpost.example.ts.net"}
	r := portalRouter(t, &logSink{}, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		return live, nil
	})
	if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the portal's node to come up", func() bool {
		ns, _ := r.NodeStatus(PortalNode)
		return ns.State == NodeOn
	})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !live.closed.Load() {
		t.Error("Close left the portal's node running")
	}
	if _, ok := r.NodeStatus(PortalNode); ok {
		t.Error("the portal's node is still reported after Close")
	}
}

// Tailscale renames a node whose name is taken. The portal's address was fixed
// when the gateway started, so the daemon says so rather than following it.
func TestAttachPortal_LogsARenamedNode(t *testing.T) {
	logs := &logSink{}
	r := portalRouter(t, logs, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		return &hostedFake{host: "outpost-1.example.ts.net"}, nil
	})
	if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the rename to be logged", func() bool {
		return logs.has("outpost-1.example.ts.net") && logs.has("https://outpost.example.ts.net") && logs.has("config.toml")
	})
}

func TestAttachPortal_QuietWhenTheNameIsRight(t *testing.T) {
	logs := &logSink{}
	r := portalRouter(t, logs, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		return &hostedFake{host: "outpost.example.ts.net"}, nil
	})
	if err := r.AttachPortal(context.Background(), "outpost", "Outpost.Example.ts.net."); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the node to be logged", func() bool { return logs.has("portal: on the tailnet") })
	if logs.has("WARNING") {
		t.Errorf("warned about a node with the right name: %q", logs.snapshot())
	}
}

// Without the gateway there is nothing for the portal's node to serve.
func TestAttachPortal_NeedsTheGateway(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.AttachPortal(context.Background(), "outpost", "outpost.example.ts.net"); err == nil {
		t.Error("AttachPortal with no gateway upstream: want an error")
	}
}
