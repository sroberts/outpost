package tsnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// fakeNode stands in for a live Tailscale node.
type fakeNode struct {
	ln     net.Listener
	closed atomic.Bool
}

func (n *fakeNode) Listener() net.Listener { return n.ln }

func (n *fakeNode) Close() error {
	n.closed.Store(true)
	return n.ln.Close()
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeNode{ln: ln}
}

// testRouter builds a Router whose node startup is controlled by the test.
func testRouter(t *testing.T, start func(ctx context.Context, cfg nodeConfig) (tailnetNode, error)) *Router {
	t.Helper()
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r.startNode = start
	r.retryInitial = 5 * time.Millisecond
	r.retryMax = 20 * time.Millisecond
	t.Cleanup(func() { r.Close() })
	return r
}

func svc(name string, port int) *manifest.Service {
	return &manifest.Service{Name: name, TailscaleName: name, Port: port}
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// Registering a node does network I/O and, on a first run, waits on the
// tailnet. Attach is called from the reconcile loop, so it must not block it.
func TestAttach_DoesNotBlockOnNodeRegistration(t *testing.T) {
	release := make(chan struct{})
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		<-release
		return newFakeNode(t), nil
	})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	if err := r.Attach(context.Background(), svc("slow", 9001)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("Attach blocked for %s while the node registered; it must return promptly", elapsed)
	}
}

// A transient failure must not leave a healthy service off the tailnet forever.
func TestAttach_RetriesAfterFailure(t *testing.T) {
	var attempts atomic.Int32
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("tailscale coordination server unreachable")
		}
		return newFakeNode(t), nil
	})

	if err := r.Attach(context.Background(), svc("flaky", 9002)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 3*time.Second, "the node to come up after retries", func() bool {
		return len(r.Attached()) == 1
	})
	if got := attempts.Load(); got < 3 {
		t.Errorf("attempts = %d; want at least 3", got)
	}
}

// Concurrent attaches for one service must not each build a node: two servers
// sharing one state directory contend over a single Tailscale identity, and the
// loser is leaked.
func TestAttach_ConcurrentCallsStartOneNode(t *testing.T) {
	var started atomic.Int32
	var nodes sync.Map
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		started.Add(1)
		n := newFakeNode(t)
		nodes.Store(n, true)
		time.Sleep(10 * time.Millisecond) // widen the window
		return n, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Attach(context.Background(), svc("contended", 9003))
		}()
	}
	wg.Wait()
	waitFor(t, 2*time.Second, "the node to attach", func() bool { return len(r.Attached()) == 1 })

	if got := started.Load(); got != 1 {
		t.Errorf("startNode called %d times for one service; want 1", got)
	}
	// Nothing may be left running that the router has forgotten about.
	nodes.Range(func(k, _ any) bool {
		n := k.(*fakeNode)
		if n.closed.Load() {
			return true
		}
		if len(r.Attached()) == 0 {
			t.Error("a node is live but the router has no record of it")
		}
		return true
	})
}

// Detaching while an attach is in flight must close the node that arrives late.
func TestDetach_DuringInFlightAttachDoesNotLeak(t *testing.T) {
	release := make(chan struct{})
	created := make(chan *fakeNode, 1)
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		<-release
		n := newFakeNode(t)
		created <- n
		return n, nil
	})

	if err := r.Attach(context.Background(), svc("racy", 9004)); err != nil {
		t.Fatal(err)
	}
	if err := r.Detach(context.Background(), "racy"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	close(release)

	select {
	case n := <-created:
		waitFor(t, 2*time.Second, "the late node to be closed", func() bool { return n.closed.Load() })
	case <-time.After(time.Second):
		// The attach was cancelled before building a node, which is also fine.
	}
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v after Detach; want empty", got)
	}
}

// Retries must stop once the service is detached.
func TestDetach_StopsRetrying(t *testing.T) {
	var attempts atomic.Int32
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		attempts.Add(1)
		return nil, errors.New("still failing")
	})

	if err := r.Attach(context.Background(), svc("doomed", 9005)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 2*time.Second, "at least one attempt", func() bool { return attempts.Load() >= 1 })
	if err := r.Detach(context.Background(), "doomed"); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	settled := attempts.Load()
	time.Sleep(200 * time.Millisecond)
	if grew := attempts.Load() - settled; grew > 1 {
		t.Errorf("retries continued after Detach (%d more attempts)", grew)
	}
}

// Attaching an already-attached service is a no-op, so the reconcile loop can
// call it on every tick.
func TestAttach_IdempotentForUnchangedService(t *testing.T) {
	var started atomic.Int32
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		started.Add(1)
		return newFakeNode(t), nil
	})

	for i := 0; i < 5; i++ {
		if err := r.Attach(context.Background(), svc("steady", 9006)); err != nil {
			t.Fatal(err)
		}
		waitFor(t, time.Second, "attach", func() bool { return len(r.Attached()) == 1 })
	}
	if got := started.Load(); got != 1 {
		t.Errorf("startNode called %d times; want 1 (Attach must be idempotent)", got)
	}
}

// A changed port must rebuild the node, so the proxy targets the new port.
func TestAttach_ConfigChangeReplacesNode(t *testing.T) {
	var mu sync.Mutex
	var all []*fakeNode
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		n := newFakeNode(t)
		mu.Lock()
		all = append(all, n)
		mu.Unlock()
		return n, nil
	})

	if err := r.Attach(context.Background(), svc("mover", 9007)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, time.Second, "first attach", func() bool { return len(r.Attached()) == 1 })

	if err := r.Attach(context.Background(), svc("mover", 9008)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 2*time.Second, "the replacement node", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(all) == 2
	})

	mu.Lock()
	first := all[0]
	mu.Unlock()
	waitFor(t, 2*time.Second, "the old node to be closed", func() bool { return first.closed.Load() })
}

func TestClose_ClosesEveryNode(t *testing.T) {
	var mu sync.Mutex
	var all []*fakeNode
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r.startNode = func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		n := newFakeNode(t)
		mu.Lock()
		all = append(all, n)
		mu.Unlock()
		return n, nil
	}
	r.retryInitial, r.retryMax = 5*time.Millisecond, 20*time.Millisecond

	for i := 0; i < 3; i++ {
		if err := r.Attach(context.Background(), svc(fmt.Sprintf("svc%d", i), 9100+i)); err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}
	waitFor(t, 2*time.Second, "all nodes to attach", func() bool { return len(r.Attached()) == 3 })

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, n := range all {
		if !n.closed.Load() {
			t.Errorf("node %d not closed by Close()", i)
		}
	}
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v after Close", got)
	}
}
