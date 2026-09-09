package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
)

// fakeClock makes backoff assertions deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	sup   *Supervisor
	rt    *fakeRuntime
	store *state.Store
	clock *fakeClock
	dir   string
	t     *testing.T
}

// writeManifest drops a service TOML into the harness's services dir.
func (h *harness) writeManifest(name string, extra string) {
	h.t.Helper()
	body := fmt.Sprintf(`
name    = %q
runtime = "process"
binary  = "/usr/bin/true"
port    = %d
storage = %q
tailscale_name = %q
%s
`, name, 9000+len(name), filepath.Join(h.dir, "data", name), name, extra)
	if err := os.WriteFile(filepath.Join(h.dir, "services", name+".toml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// writeManifestPort writes a manifest with an explicit port.
func (h *harness) writeManifestPort(name string, port int) {
	h.t.Helper()
	body := fmt.Sprintf(`
name    = %q
runtime = "process"
binary  = "/usr/bin/true"
port    = %d
storage = %q
tailscale_name = %q
`, name, port, filepath.Join(h.dir, "data", name), name)
	if err := os.WriteFile(filepath.Join(h.dir, "services", name+".toml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) removeManifest(name string) {
	h.t.Helper()
	if err := os.Remove(filepath.Join(h.dir, "services", name+".toml")); err != nil {
		h.t.Fatal(err)
	}
}

// apply reloads manifests; tick runs one reconcile pass.
func (h *harness) apply() {
	h.t.Helper()
	if err := h.sup.Apply(context.Background()); err != nil {
		h.t.Fatalf("Apply: %v", err)
	}
}

func (h *harness) tick() {
	h.t.Helper()
	h.sup.Tick(context.Background())
}

func (h *harness) status(name string) string {
	h.t.Helper()
	rec, ok, err := h.store.GetService(name)
	if err != nil {
		h.t.Fatal(err)
	}
	if !ok {
		return "<absent>"
	}
	return rec.Status
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	rt := newFakeRuntime()
	clock := newClock()
	sup, err := New(Options{
		ServicesDir:         filepath.Join(dir, "services"),
		Store:               store,
		ProcessRuntime:      rt,
		HealthFailThreshold: 3,
		Now:                 clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{sup: sup, rt: rt, store: store, clock: clock, dir: dir, t: t}
}

func TestApply_StartsDeclaredService(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Fatalf("start count = %d; want 1", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusRunning) {
		t.Errorf("status = %q; want running", got)
	}

	// A second tick must not start a second copy.
	h.tick()
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count after idle tick = %d; want 1", got)
	}
}

func TestApply_RejectsInvalidManifestWithoutHalfApplying(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	// Two services claiming the same port is a §5 violation.
	h.writeManifest("beta", "")
	if err := os.WriteFile(filepath.Join(h.dir, "services", "beta.toml"), []byte(`
name = "beta"
runtime = "process"
binary = "/usr/bin/true"
port = 9005
storage = "/tmp/beta"
tailscale_name = "alpha"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.sup.Apply(context.Background()); err == nil {
		t.Fatal("Apply with a duplicate tailscale_name: want error, got nil")
	}
	h.tick()
	if got := h.rt.startCount("beta"); got != 0 {
		t.Errorf("beta was started despite a failed Apply (count = %d)", got)
	}
	if got := h.rt.stopCount("alpha"); got != 0 {
		t.Errorf("alpha was disturbed by a failed Apply (stops = %d)", got)
	}
}

func TestApply_StopsServiceRemovedFromManifest(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	h.removeManifest("alpha")
	h.apply()
	h.tick()

	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Errorf("stop count = %d; want 1", got)
	}
	if _, ok, _ := h.store.GetService("alpha"); ok {
		t.Error("state row survived removal from the manifest")
	}
}

func TestRestart_OnFailureRestartsAfterBackoff(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.apply()
	h.tick()

	h.rt.crash("alpha", 1)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("restarted immediately (starts = %d); want the backoff to be respected", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusFailed) {
		t.Errorf("status after crash = %q; want failed", got)
	}

	h.clock.Advance(6 * time.Second)
	h.tick()
	if got := h.rt.startCount("alpha"); got != 2 {
		t.Fatalf("start count after backoff elapsed = %d; want 2", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusRunning) {
		t.Errorf("status after restart = %q; want running", got)
	}
}

func TestRestart_BackoffDoublesAndCaps(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.apply()
	h.tick()

	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300}
	for i, delay := range want {
		h.rt.crash("alpha", 1)
		h.tick()

		// Just short of the backoff: no restart yet.
		h.clock.Advance(delay*time.Second - time.Second)
		h.tick()
		if got := h.rt.startCount("alpha"); got != i+1 {
			t.Fatalf("attempt %d: restarted early (starts = %d, want %d) with backoff %s", i+1, got, i+1, delay*time.Second)
		}

		h.clock.Advance(2 * time.Second)
		h.tick()
		if got := h.rt.startCount("alpha"); got != i+2 {
			t.Fatalf("attempt %d: no restart after %s (starts = %d, want %d)", i+1, delay*time.Second, got, i+2)
		}
	}
}

func TestRestart_BackoffResetsAfterServiceStaysHealthy(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.apply()
	h.tick()

	// Crash twice so the backoff has grown to 10s.
	h.rt.crash("alpha", 1)
	h.tick()
	h.clock.Advance(6 * time.Second)
	h.tick()
	h.rt.crash("alpha", 1)
	h.tick()
	h.clock.Advance(11 * time.Second)
	h.tick()

	// Stay healthy long enough to be considered stable, then crash again: the
	// backoff should be back to its initial value.
	h.clock.Advance(2 * time.Minute)
	h.tick()

	h.rt.crash("alpha", 1)
	h.tick()
	starts := h.rt.startCount("alpha")
	h.clock.Advance(6 * time.Second)
	h.tick()
	if got := h.rt.startCount("alpha"); got != starts+1 {
		t.Errorf("backoff did not reset after a healthy run: no restart 6s after crash (starts %d -> %d)", starts, got)
	}
}

func TestRestart_NeverPolicyLeavesServiceFailed(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `restart_policy = "never"`)
	h.apply()
	h.tick()

	h.rt.crash("alpha", 1)
	h.tick()
	h.clock.Advance(10 * time.Minute)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; want 1 (restart_policy = never)", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusFailed) {
		t.Errorf("status = %q; want failed", got)
	}
}

func TestRestart_OnFailureIgnoresCleanExitButAlwaysRestartsIt(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `restart_policy = "on-failure"`)
	h.writeManifest("beta", `restart_policy = "always"`)
	h.apply()
	h.tick()

	h.rt.exitClean("alpha")
	h.rt.exitClean("beta")
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("on-failure service restarted after a zero exit (starts = %d; want 1)", got)
	}
	if got := h.rt.startCount("beta"); got != 2 {
		t.Errorf("always service not restarted after a zero exit (starts = %d; want 2)", got)
	}
}

func TestHealth_RestartsAfterConsecutiveFailures(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	// Let it pass one healthcheck first, which ends its startup grace window.
	// A service that has never been healthy is still starting, and its failures
	// deliberately do not count — see TestHealth_StartupGraceBeforeFirstFailureCounts.
	h.tick()

	h.rt.setHealthy("alpha", false)
	// Threshold is 3: the first two failures mark it unhealthy but do not restart.
	h.tick()
	h.tick()
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Fatalf("restarted before the failure threshold (starts = %d)", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusUnhealthy) {
		t.Errorf("status = %q; want unhealthy", got)
	}

	h.tick() // third consecutive failure
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("start count = %d; want 2 after the healthcheck threshold was crossed", got)
	}
}

func TestHealth_TransientFailureDoesNotAccumulate(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	for i := 0; i < 5; i++ {
		h.rt.setHealthy("alpha", false)
		h.tick()
		h.rt.setHealthy("alpha", true)
		h.tick()
	}
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; want 1 (non-consecutive failures must not trigger a restart)", got)
	}
}

func TestDesiredStopped_StopsAndStaysStopped(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if err := h.store.SetDesired("alpha", state.DesiredStopped); err != nil {
		t.Fatal(err)
	}
	h.tick()

	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Fatalf("stop count = %d; want 1", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusStopped) {
		t.Errorf("status = %q; want stopped", got)
	}

	h.clock.Advance(10 * time.Minute)
	h.tick()
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("a user-stopped service was restarted (starts = %d; want 1)", got)
	}
}

func TestDesiredRunning_StartsAStoppedService(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.store.SetDesired("alpha", state.DesiredStopped)
	h.tick()

	h.store.SetDesired("alpha", state.DesiredRunning)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("start count = %d; want 2 after the service was asked to run again", got)
	}
}

func TestRestartRequest_StopsAndStartsOnce(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if err := h.store.RequestRestart("alpha"); err != nil {
		t.Fatal(err)
	}
	h.tick()

	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Errorf("stop count = %d; want 1", got)
	}
	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("start count = %d; want 2", got)
	}

	// The epoch is consumed: further ticks must not keep restarting.
	h.tick()
	h.tick()
	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("restart request was replayed (starts = %d; want 2)", got)
	}
}

func TestStart_FailureIsRecordedAndRetried(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.rt.failStart("alpha", errors.New("boom"))
	h.apply()
	h.tick()

	if got := h.status("alpha"); got != string(runtime.StatusFailed) {
		t.Errorf("status = %q; want failed", got)
	}

	h.rt.failStart("alpha", nil)
	h.clock.Advance(6 * time.Second)
	h.tick()
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; want 1 after the failing start was retried", got)
	}
}

// §8: the state DB is a cache. Deleting it and re-applying must converge.
func TestStateRecovery_ReapplyConvergesAfterStateLoss(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	h.tick()
	h.sup.Shutdown(context.Background())

	dbPath := filepath.Join(h.dir, "state.db")
	h.store.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbPath + suffix)
	}

	store2, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	rt2 := newFakeRuntime()
	sup2, err := New(Options{
		ServicesDir:         filepath.Join(h.dir, "services"),
		Store:               store2,
		ProcessRuntime:      rt2,
		HealthFailThreshold: 3,
		Now:                 h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup2.Apply(context.Background()); err != nil {
		t.Fatalf("Apply after state loss: %v", err)
	}
	sup2.Tick(context.Background())

	for _, name := range []string{"alpha", "beta"} {
		if got := rt2.startCount(name); got != 1 {
			t.Errorf("%s: start count = %d; want 1 after state loss", name, got)
		}
		rec, ok, _ := store2.GetService(name)
		if !ok || rec.Status != string(runtime.StatusRunning) {
			t.Errorf("%s: status = %q (present=%v); want running", name, rec.Status, ok)
		}
	}
}

func TestShutdown_StopsEverythingItStarted(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	h.tick()

	if err := h.sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if got := h.rt.stopCount(name); got != 1 {
			t.Errorf("%s: stop count = %d; want 1", name, got)
		}
	}
}

func TestEvents_RecordedForLifecycleTransitions(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.rt.crash("alpha", 7)
	h.tick()

	evs, err := h.store.Events("alpha", 50)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range evs {
		kinds[e.Kind] = true
	}
	for _, want := range []string{state.EventStarted, state.EventExited} {
		if !kinds[want] {
			t.Errorf("no %q event recorded; got %v", want, kinds)
		}
	}
}

func TestRun_ReconcilesUntilContextCancelled(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.sup.Interval = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- h.sup.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.rt.startCount("alpha") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Fatalf("Run did not start the service (starts = %d)", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestReload_PicksUpManifestChanges(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	// No edit: nothing to do.
	changed, err := h.sup.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if changed {
		t.Error("Reload reported a change with no manifest edit")
	}

	// Adding a service is a change.
	h.writeManifest("beta", "")
	changed, err = h.sup.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload after adding a manifest: %v", err)
	}
	if !changed {
		t.Fatal("Reload did not notice a new manifest")
	}
	h.tick()
	if got := h.rt.startCount("beta"); got != 1 {
		t.Errorf("beta start count = %d; want 1", got)
	}
}

// A broken edit must leave the previous manifest set in force, not tear down
// running services.
func TestReload_InvalidEditKeepsPreviousSet(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if err := os.WriteFile(filepath.Join(h.dir, "services", "beta.toml"), []byte("this is not = valid toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sup.Reload(context.Background()); err == nil {
		t.Fatal("Reload with a malformed manifest: want error, got nil")
	}

	h.tick()
	if got := h.rt.stopCount("alpha"); got != 0 {
		t.Errorf("a running service was stopped by a failed reload (stops = %d)", got)
	}
	if got := h.status("alpha"); got != string(runtime.StatusRunning) {
		t.Errorf("alpha status = %q; want running", got)
	}

	// The same broken file must not be re-reported every tick.
	if _, err := h.sup.Reload(context.Background()); err != nil {
		t.Errorf("failed reload was retried for an unchanged file: %v", err)
	}
}
