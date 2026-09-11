package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/tailnet"
)

// reportingTailnet is a router that can also say what it observed — the
// optional TailnetReporter half. The existing fakeTailnet deliberately does
// not implement it, which is what keeps every other tailnet test on the
// "router cannot report" path.
type reportingTailnet struct {
	mu     sync.Mutex
	status map[string]tailnet.NodeStatus
}

func newReportingTailnet() *reportingTailnet {
	return &reportingTailnet{status: map[string]tailnet.NodeStatus{}}
}

func (r *reportingTailnet) Attach(context.Context, *manifest.Service) error { return nil }
func (r *reportingTailnet) Detach(context.Context, string) error            { return nil }

func (r *reportingTailnet) NodeStatus(service string) (tailnet.NodeStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, ok := r.status[service]
	return ns, ok
}

func (r *reportingTailnet) set(service string, ns tailnet.NodeStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[service] = ns
}

// supWithTailnet builds a supervisor with one declared service and the given
// router, and applies it.
func supWithTailnet(t *testing.T, router TailnetRouter) (*Supervisor, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	svcDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name = \"alpha\"\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nargs = [\"3600\"]\n" +
		"port = 19400\nstorage = " + strconvQuote(filepath.Join(dir, "data")) + "\ntailscale_name = \"alpha\"\n"
	if err := os.WriteFile(filepath.Join(svcDir, "alpha.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	sup, err := New(Options{
		ServicesDir:    svcDir,
		Store:          store,
		ProcessRuntime: newFakeRuntime(),
		Tailnet:        router,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return sup, store
}

func strconvQuote(s string) string { return "\"" + s + "\"" }

// The point of the whole change: status reports what the router observed, not
// what the manifest asked for.
func TestStatus_TailnetStateComesFromTheRouter(t *testing.T) {
	router := newReportingTailnet()
	router.set("alpha", tailnet.NodeStatus{
		State: tailnet.StateNeedsLogin, Hostname: "alpha",
		LoginURL: "https://login.tailscale.com/a/abc123",
	})
	sup, _ := supWithTailnet(t, router)

	st, err := sup.Status(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Tailnet.State != tailnet.StateNeedsLogin {
		t.Errorf("state = %q; want needs-login", st.Tailnet.State)
	}
	if st.Tailnet.LoginURL == "" {
		t.Error("the login URL did not reach the status")
	}
	// The configured name is still reported, unchanged — it is the other half.
	if st.TailscaleName != "alpha" {
		t.Errorf("TailscaleName = %q; want the manifest's value", st.TailscaleName)
	}
}

// A router that supervises but cannot report leaves the state unknown, so
// every renderer falls back to what it printed before this change.
func TestStatus_RouterWithoutReportingLeavesStateUnknown(t *testing.T) {
	sup, _ := supWithTailnet(t, newFakeTailnet())

	st, err := sup.Status(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Tailnet.State != tailnet.StateUnknown {
		t.Errorf("state = %q; want unknown for a non-reporting router", st.Tailnet.State)
	}
}

// Declared, but the router holds no node for it.
func TestStatus_NoNodeReadsOffline(t *testing.T) {
	sup, _ := supWithTailnet(t, newReportingTailnet())

	st, err := sup.Status(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Tailnet.State != tailnet.StateOffline {
		t.Errorf("state = %q; want offline", st.Tailnet.State)
	}
}

// With no router at all the service is not asking for a tailnet identity.
func TestStatus_NoRouterReadsDisabled(t *testing.T) {
	sup, _ := supWithTailnet(t, nil)

	st, err := sup.Status(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Tailnet.State != tailnet.StateDisabled {
		t.Errorf("state = %q; want disabled", st.Tailnet.State)
	}
}

// The login URL should reach the activity feed, because that is where an
// operator already looks — but once, not on every reconcile tick.
func TestTailnetEvent_RecordedOnceForNeedsLogin(t *testing.T) {
	router := newReportingTailnet()
	router.set("alpha", tailnet.NodeStatus{
		State: tailnet.StateNeedsLogin, Hostname: "alpha",
		LoginURL: "https://login.tailscale.com/a/abc123",
	})
	sup, store := supWithTailnet(t, router)

	for i := 0; i < 5; i++ {
		sup.Tick(context.Background())
	}

	events, err := store.Events("alpha", 50)
	if err != nil {
		t.Fatal(err)
	}
	var tailnetEvents int
	var sawURL bool
	for _, e := range events {
		if e.Kind == state.EventTailnet {
			tailnetEvents++
			if strings.Contains(e.Message, "abc123") {
				sawURL = true
			}
		}
	}
	if tailnetEvents != 1 {
		t.Errorf("recorded %d tailnet events across five ticks; want exactly 1", tailnetEvents)
	}
	if !sawURL {
		t.Error("the event does not carry the login URL, which is the reason for recording it")
	}
}

// An attached node is deliberately silent: a crash-looping service already
// writes started and exited rows, and two more per restart would drown them.
func TestTailnetEvent_AttachedIsNotRecorded(t *testing.T) {
	router := newReportingTailnet()
	router.set("alpha", tailnet.NodeStatus{
		State: tailnet.StateAttached, Hostname: "alpha", Addr: "100.1.2.3",
	})
	sup, store := supWithTailnet(t, router)

	for i := 0; i < 3; i++ {
		sup.Tick(context.Background())
	}

	events, err := store.Events("alpha", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == state.EventTailnet {
			t.Errorf("an attached node recorded an event: %q", e.Message)
		}
	}
}
