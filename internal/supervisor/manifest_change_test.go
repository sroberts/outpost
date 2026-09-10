package supervisor

import (
	"testing"
	"time"
)

// spec.md §6.1: the reconcile loop must "start new, stop removed, restart
// changed". A running service whose declaration changed keeps running the old
// configuration until it is restarted.
func TestReconcile_RestartsServiceWhoseManifestChanged(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `env = { MODE = "before" }`)
	h.apply()
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Fatalf("start count = %d; want 1", got)
	}

	// Change the declaration and re-apply.
	h.writeManifest("alpha", `env = { MODE = "after" }`)
	h.apply()
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("start count = %d; want 2 (a changed manifest must restart the service)", got)
	}
	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Errorf("stop count = %d; want 1", got)
	}
}

// Re-applying an unchanged manifest must not disturb anything.
func TestReconcile_UnchangedManifestDoesNotRestart(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `env = { MODE = "same" }`)
	h.apply()
	h.tick()

	for i := 0; i < 3; i++ {
		h.writeManifest("alpha", `env = { MODE = "same" }`)
		h.apply()
		h.clock.Advance(5 * time.Second)
		h.tick()
	}

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; want 1 (an unchanged manifest must not restart)", got)
	}
	if got := h.rt.stopCount("alpha"); got != 0 {
		t.Errorf("stop count = %d; want 0", got)
	}
}

// A port change must restart the service, so the tailnet proxy is rebuilt
// against the new port rather than left pointing at the old one.
func TestReconcile_PortChangeRestartsAndReattaches(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	attachedBefore, _, _ := tn.counts("alpha")

	// writeManifest derives the port from the name, so set one explicitly.
	h.writeManifestPort("alpha", 9999)
	h.apply()
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Fatalf("start count = %d; want 2 after a port change", got)
	}
	attachedAfter, _, live := tn.counts("alpha")
	if attachedAfter <= attachedBefore {
		t.Errorf("tailnet not re-attached after a port change (%d -> %d)", attachedBefore, attachedAfter)
	}
	if !live {
		t.Error("service is not on the tailnet after the restart")
	}
}

// A service the user stopped must stay stopped even if its manifest changes.
func TestReconcile_ChangedManifestDoesNotResurrectAStoppedService(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `env = { MODE = "before" }`)
	h.apply()
	h.tick()
	if err := h.store.SetDesired("alpha", "stopped"); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	h.tick()

	h.writeManifest("alpha", `env = { MODE = "after" }`)
	h.apply()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("a stopped service was started by a manifest change (starts = %d; want 1)", got)
	}
}
