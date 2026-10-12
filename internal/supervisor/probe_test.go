package supervisor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
)

// slowRuntime makes every healthcheck take a while, as a probe that runs into
// its timeout does.
type slowRuntime struct {
	*fakeRuntime
	delay time.Duration
}

func (r *slowRuntime) Healthy(ctx context.Context, h runtime.Handle) (bool, error) {
	time.Sleep(r.delay)
	return r.fakeRuntime.Healthy(ctx, h)
}

// Probes ran one after another on the loop, so N slow services cost N
// timeouts per tick and the loop fell behind its interval (#29).
func TestHealth_ProbesRunConcurrently(t *testing.T) {
	h := newHarness(t)
	const n, delay = 6, 200 * time.Millisecond
	for i := 0; i < n; i++ {
		// writeManifest derives the port from the name's length, which
		// these names share.
		h.writeManifestPort(fmt.Sprintf("svc%d", i), 9300+i)
	}
	h.apply()
	h.tick() // starts them
	h.sup.procRT = &slowRuntime{fakeRuntime: h.rt, delay: delay}

	start := time.Now()
	h.tick()
	if elapsed := time.Since(start); elapsed >= n*delay/2 {
		t.Errorf("a tick with %d slow probes took %s; probes are still serial (each takes %s)", n, elapsed, delay)
	}
	for i := 0; i < n; i++ {
		if got := h.status(fmt.Sprintf("svc%d", i)); got != string(runtime.StatusRunning) {
			t.Errorf("svc%d status = %q; the concurrent probe's result was not applied", i, got)
		}
	}
}

// A healthcheck that failed while outpost itself could not run says nothing
// about the service, and must not count toward restarting it (#29).
func TestHealth_FailureDuringADaemonStallIsNotCounted(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("svc", "")
	h.apply()
	h.tick()
	h.clock.Advance(5 * time.Minute) // past any startup grace
	h.tick()
	h.rt.setHealthy("svc", false)

	// The heartbeat is running and has not beaten for seconds: the daemon is
	// stalled right now.
	h.sup.heartbeat.mu.Lock()
	h.sup.heartbeat.last = time.Now().Add(-5 * time.Second)
	h.sup.heartbeat.mu.Unlock()

	for i := 0; i < 10; i++ {
		h.tick()
	}
	if got := h.rt.startCount("svc"); got != 1 {
		t.Errorf("restarted during a daemon stall (starts = %d; want 1)", got)
	}

	// Once the daemon is running normally, failures count again.
	h.sup.heartbeat.mu.Lock()
	h.sup.heartbeat.last = time.Now()
	h.sup.heartbeat.stallLen = 0
	h.sup.heartbeat.mu.Unlock()
	for i := 0; i < 4; i++ {
		h.tick()
	}
	h.clock.Advance(5 * time.Minute) // past the restart backoff
	h.tick()
	if got := h.rt.startCount("svc"); got < 2 {
		t.Errorf("not restarted once the stall was over (starts = %d)", got)
	}
}

func TestHeartbeat_ReportsNothingWhileNotRunning(t *testing.T) {
	hb := newHeartbeat(10*time.Millisecond, 50*time.Millisecond)
	if _, stalled := hb.stalledSince(time.Now().Add(-time.Hour)); stalled {
		t.Error("a heartbeat that never ran reported a stall")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hb.run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	// Long after it stopped, the last beat must not read as a stall.
	time.Sleep(100 * time.Millisecond)
	if _, stalled := hb.stalledSince(time.Now().Add(-time.Hour)); stalled {
		t.Error("a stopped heartbeat reported a stall")
	}
}

func TestHeartbeat_RecordedStallOverlapsOnlyLaterProbes(t *testing.T) {
	hb := newHeartbeat(10*time.Millisecond, 50*time.Millisecond)
	now := time.Now()
	hb.last = now
	hb.stallEnd, hb.stallLen = now.Add(-time.Second), 3*time.Second

	if _, stalled := hb.stalledSince(now.Add(-2 * time.Second)); !stalled {
		t.Error("a probe in flight when the stall ended was not excused")
	}
	if _, stalled := hb.stalledSince(now.Add(-500 * time.Millisecond)); stalled {
		t.Error("a probe that started after the stall ended was excused")
	}
}

func TestHeartbeat_DetectsARealStall(t *testing.T) {
	hb := newHeartbeat(10*time.Millisecond, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.run(ctx)
	time.Sleep(20 * time.Millisecond)

	// Stop the world for long enough to miss several beats: the closest a
	// test can get to the daemon not being scheduled.
	since := time.Now()
	hb.mu.Lock()
	time.Sleep(150 * time.Millisecond)
	hb.mu.Unlock()
	time.Sleep(40 * time.Millisecond) // let the heartbeat wake and record it

	if _, stalled := hb.stalledSince(since); !stalled {
		t.Error("missed a stall of 150ms against a 30ms threshold")
	}
}
