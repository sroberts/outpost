package tsnet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/tailnet"
)

// stubClock drives r.now. The reminder interval is measured in minutes, and no
// test is going to wait for them.
type stubClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStubClock() *stubClock {
	return &stubClock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
}

func (c *stubClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stubClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// addressedNode is a node that knows what the tailnet gave it, which the fakes
// deliberately do not. It stands in for the real backend's optional interface.
type addressedNode struct {
	*fakeNode
	addr    string
	dnsName string
}

func (n *addressedNode) TailnetAddrs() (string, string) { return n.addr, n.dnsName }

// waitingRouter attaches a service whose node is still coming up. Sending a
// node on the returned channel completes the attach; the clock is the router's.
func waitingRouter(t *testing.T, name string, port int) (*Router, *stubClock, chan<- tailnetNode) {
	t.Helper()
	clock := newStubClock()
	deliver := make(chan tailnetNode, 1)
	entered := make(chan struct{})
	var enterOnce sync.Once
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		enterOnce.Do(func() { close(entered) })
		select {
		case n := <-deliver:
			return n, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	r.now = clock.now
	if err := r.Attach(context.Background(), svc(name, port)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// Wait for the attach goroutine to be inside startNode, where tsnet's own
	// logging happens: a login URL cannot arrive before then. The node reads as
	// starting from the moment Attach returns, so its state cannot say this.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the attach never reached startNode")
	}
	return r, clock, deliver
}

func nodeState(t *testing.T, r *Router, service string) tailnet.State {
	t.Helper()
	st, ok := r.NodeStatus(service)
	if !ok {
		return tailnet.StateUnknown
	}
	return st.State
}

// tsnet reoffers the same login URL every five seconds for as long as nobody
// authorises the node. One unauthorised service that way produced 54 identical
// log lines, which is the noise this throttle exists to stop.
func TestNoteLoginURL_LogsOnceWhileTheURLIsUnchanged(t *testing.T) {
	r, _, _ := waitingRouter(t, "unauthorised", 9301)
	const url = "https://login.tailscale.com/a/first"

	if !r.noteLoginURL("unauthorised", url) {
		t.Fatal("first sighting of a login URL was not worth logging; it is the only way anyone learns of it")
	}
	for i := 0; i < 12; i++ {
		if r.noteLoginURL("unauthorised", url) {
			t.Fatalf("repeat %d asked to be logged again", i+1)
		}
	}

	st, ok := r.NodeStatus("unauthorised")
	if !ok {
		t.Fatal("NodeStatus: no node")
	}
	if st.State != tailnet.StateNeedsLogin {
		t.Errorf("State = %q; want %q", st.State, tailnet.StateNeedsLogin)
	}
	if st.LoginURL != url {
		t.Errorf("LoginURL = %q; want %q", st.LoginURL, url)
	}
}

// A new URL is news even inside the throttle window: the old one no longer
// authorises anything.
func TestNoteLoginURL_LogsAgainWhenTheURLChanges(t *testing.T) {
	r, _, _ := waitingRouter(t, "rotating", 9302)

	if !r.noteLoginURL("rotating", "https://login.tailscale.com/a/one") {
		t.Fatal("first URL: want it logged")
	}
	if !r.noteLoginURL("rotating", "https://login.tailscale.com/a/two") {
		t.Error("a different URL was throttled; the one already logged is spent")
	}

	st, _ := r.NodeStatus("rotating")
	if st.LoginURL != "https://login.tailscale.com/a/two" {
		t.Errorf("LoginURL = %q; want the newest one", st.LoginURL)
	}
}

// A node nobody has authorised is still worth an occasional reminder.
func TestNoteLoginURL_RemindsAfterTheInterval(t *testing.T) {
	r, clock, _ := waitingRouter(t, "patient", 9303)
	const url = "https://login.tailscale.com/a/waiting"

	if !r.noteLoginURL("patient", url) {
		t.Fatal("first sighting: want it logged")
	}
	clock.advance(loginReminderInterval - time.Second)
	if r.noteLoginURL("patient", url) {
		t.Error("logged again before the reminder interval had passed")
	}
	clock.advance(2 * time.Second)
	if !r.noteLoginURL("patient", url) {
		t.Error("no reminder after the interval; a node awaiting authorisation goes silent")
	}
}

// The whole point of the state: an attach in flight, a node awaiting
// authorisation and a node genuinely on the tailnet are three different things.
func TestNodeStatus_StartingThenNeedsLoginThenAttached(t *testing.T) {
	r, _, deliver := waitingRouter(t, "arriving", 9304)
	if got := nodeState(t, r, "arriving"); got != tailnet.StateStarting {
		t.Fatalf("State = %q while the attach is in flight; want %q", got, tailnet.StateStarting)
	}

	const url = "https://login.tailscale.com/a/arriving"
	r.noteLoginURL("arriving", url)
	if got := nodeState(t, r, "arriving"); got != tailnet.StateNeedsLogin {
		t.Fatalf("State = %q; want %q", got, tailnet.StateNeedsLogin)
	}

	deliver <- newFakeNode(t)
	waitFor(t, 2*time.Second, "the node to attach", func() bool {
		return nodeState(t, r, "arriving") == tailnet.StateAttached
	})

	st, _ := r.NodeStatus("arriving")
	if st.LoginURL != "" {
		t.Errorf("LoginURL = %q after attaching; an authorised node must not still offer a login link", st.LoginURL)
	}
	if st.Hostname != "arriving" {
		t.Errorf("Hostname = %q; want the configured name", st.Hostname)
	}
	if st.Since.IsZero() {
		t.Error("Since is zero; the state has to say when it began")
	}
}

// The address arrives through the optional interface, so a fake that says
// nothing leaves it empty rather than breaking.
func TestNodeStatus_ReportsWhatTheNodeGotFromTheTailnet(t *testing.T) {
	r, _, deliver := waitingRouter(t, "addressed", 9305)

	deliver <- &addressedNode{
		fakeNode: newFakeNode(t),
		addr:     "100.101.102.103",
		dnsName:  "addressed.tail0c0de.ts.net",
	}
	waitFor(t, 2*time.Second, "the node to attach", func() bool {
		return nodeState(t, r, "addressed") == tailnet.StateAttached
	})

	st, _ := r.NodeStatus("addressed")
	if st.Addr != "100.101.102.103" {
		t.Errorf("Addr = %q; want the tailnet address", st.Addr)
	}
	if st.Hostname != "addressed.tail0c0de.ts.net" {
		t.Errorf("Hostname = %q; want what the node registered as", st.Hostname)
	}
}

// A failed attach must say why: "failed" alone sends an operator to the daemon
// log to find out what everyone already knows.
func TestNodeStatus_FailedCarriesTheReason(t *testing.T) {
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		return nil, errors.New("tailscale coordination server unreachable")
	})
	if err := r.Attach(context.Background(), svc("broken", 9306)); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	waitFor(t, 2*time.Second, "the attach to fail", func() bool {
		return nodeState(t, r, "broken") == tailnet.StateFailed
	})
	st, _ := r.NodeStatus("broken")
	if !strings.Contains(st.Detail, "coordination server unreachable") {
		t.Errorf("Detail = %q; want the error from the failed attempt", st.Detail)
	}
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v; a failed node is not on the tailnet", got)
	}
}

// A detached service has no node, which is different from having one in an
// unknown state.
func TestNodeStatus_NoNodeAfterDetach(t *testing.T) {
	r, _, deliver := waitingRouter(t, "leaving", 9307)
	deliver <- newFakeNode(t)
	waitFor(t, 2*time.Second, "the node to attach", func() bool {
		return nodeState(t, r, "leaving") == tailnet.StateAttached
	})

	if err := r.Detach(context.Background(), "leaving"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, ok := r.NodeStatus("leaving"); ok {
		t.Error("NodeStatus reports a node for a detached service")
	}
	// And a URL arriving late for a node nobody has has nowhere to go.
	if r.noteLoginURL("leaving", "https://login.tailscale.com/a/late") {
		t.Error("a login URL for an unknown service asked to be logged")
	}
}
