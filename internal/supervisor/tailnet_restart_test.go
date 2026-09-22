package supervisor

import (
	"context"
	"fmt"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/tailnet"
)

// restartingTailnet behaves like the real router across a restart: Detach
// removes the node, and Attach builds a fresh one that reads as starting.
// Attach on a node that already exists is a no-op, as it is for the real
// router, which is what lets the healthy-tick re-attach leave state alone.
type restartingTailnet struct {
	*reportingTailnet
}

func (r *restartingTailnet) Attach(_ context.Context, svc *manifest.Service) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.status[svc.Name]; !ok {
		r.status[svc.Name] = tailnet.NodeStatus{State: tailnet.StateStarting, Hostname: svc.TailscaleName}
	}
	return nil
}

func (r *restartingTailnet) Detach(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.status, name)
	return nil
}

func countEvents(t *testing.T, store *state.Store, kind string) int {
	t.Helper()
	events, err := store.Events("alpha", 200)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// restartUnauthorised restarts alpha and lets its fresh node settle into
// needing authorisation under a new URL, then ticks so the healthy service
// re-attaches and reports.
func restartUnauthorised(t *testing.T, sup *Supervisor, router *restartingTailnet, url string) {
	t.Helper()
	ctx := context.Background()
	if err := sup.RestartService(ctx, "alpha"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	sup.Tick(ctx)
	router.set("alpha", tailnet.NodeStatus{State: tailnet.StateNeedsLogin, Hostname: "alpha", LoginURL: url})
	sup.Tick(ctx)
}

// An unauthorised service in a restart loop must write one row, not one per
// cycle. Each restart builds a fresh node that passes through starting and may
// be issued a new URL; neither is a change worth an operator's attention.
func TestTailnetEvent_NotRepeatedAcrossRestarts(t *testing.T) {
	router := &restartingTailnet{newReportingTailnet()}
	sup, store := supWithTailnet(t, router)

	router.set("alpha", tailnet.NodeStatus{
		State: tailnet.StateNeedsLogin, Hostname: "alpha",
		LoginURL: "https://login.tailscale.com/a/first",
	})
	sup.Tick(context.Background())

	for i := 0; i < 3; i++ {
		restartUnauthorised(t, sup, router, fmt.Sprintf("https://login.tailscale.com/a/again%d", i))
	}

	// Without this the test could pass by never restarting at all.
	if started := countEvents(t, store, state.EventStarted); started < 4 {
		t.Fatalf("service started %d times; the test needs it to have restarted three times", started)
	}
	if got := countEvents(t, store, state.EventTailnet); got != 1 {
		t.Errorf("recorded %d tailnet events across three restarts; want exactly 1", got)
	}
}

// Suppressing restarts must not suppress a genuine change: a node that
// attached and then needs authorisation again is news.
func TestTailnetEvent_RecordedAgainAfterAttaching(t *testing.T) {
	router := &restartingTailnet{newReportingTailnet()}
	sup, store := supWithTailnet(t, router)
	ctx := context.Background()

	router.set("alpha", tailnet.NodeStatus{
		State: tailnet.StateNeedsLogin, Hostname: "alpha",
		LoginURL: "https://login.tailscale.com/a/first",
	})
	sup.Tick(ctx)
	router.set("alpha", tailnet.NodeStatus{State: tailnet.StateAttached, Hostname: "alpha", Addr: "100.1.2.3"})
	sup.Tick(ctx)

	restartUnauthorised(t, sup, router, "https://login.tailscale.com/a/second")

	if got := countEvents(t, store, state.EventTailnet); got != 2 {
		t.Errorf("recorded %d tailnet events; want 2 — one per time the node needed authorisation", got)
	}
}
