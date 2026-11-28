package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
)

// runFor starts the loop with a tick far enough off that nothing a test sees
// can have come from one, and waits for the first pass to start name.
func runFor(t *testing.T, h *harness, name string) {
	t.Helper()
	h.sup.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.sup.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return h.rt.startCount(name) == 1 }, "the first pass to start "+name)
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A service that dies is recorded as down when it dies, not on the next
// tick: until then the store says running, and the portal shows a service
// crashing on start as a healthy one.
func TestExit_ReapedWhenItHappensNotOnTheNextTick(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.apply()
	runFor(t, h, "alpha")

	h.rt.crash("alpha", 1)
	waitFor(t, func() bool { return h.status("alpha") == string(runtime.StatusFailed) }, "alpha to read failed")

	rec, _, err := h.store.GetService("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rec.RestartCount != 1 {
		t.Errorf("restart count = %d; want the crash counted once", rec.RestartCount)
	}
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("starts = %d; the backoff still holds the restart", got)
	}
}

// An instance stopped on purpose isn't a crash because it exited.
func TestExit_StopOnPurposeIsNotReapedAsACrash(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	runFor(t, h, "alpha")

	if err := h.sup.RestartService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.rt.stopCount("alpha") == 1 }, "alpha to be stopped")
	// Give a stray exit notice time to land.
	time.Sleep(50 * time.Millisecond)

	rec, _, err := h.store.GetService("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if rec.RestartCount != 0 || rec.Status == string(runtime.StatusFailed) {
		t.Errorf("after a requested restart: status %q, restart count %d; want no crash recorded", rec.Status, rec.RestartCount)
	}
}
