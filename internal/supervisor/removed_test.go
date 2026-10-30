package supervisor

import (
	"context"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/state"
)

func listed(t *testing.T, h *harness, name string) (ServiceStatus, bool) {
	t.Helper()
	all, err := h.sup.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range all {
		if st.Name == name {
			return st, true
		}
	}
	return ServiceStatus{}, false
}

// #32: a manifest deleted by hand leaves the service listed as removed, not
// stopped, with its history intact.
func TestRemoved_ManifestDeletedWhileRunning(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	h.removeManifest("alpha")
	h.apply()
	h.tick()

	st, ok := listed(t, h, "alpha")
	if !ok || st.Status != StatusRemoved {
		t.Fatalf("list shows %+v (present %v); want alpha as removed", st, ok)
	}
	detail, err := h.sup.Status(context.Background(), "alpha")
	if err != nil || detail.Status != StatusRemoved {
		t.Errorf("status = %q, %v; want removed", detail.Status, err)
	}
	events, err := h.sup.Events(context.Background(), "alpha", 50)
	if err != nil {
		t.Fatal(err)
	}
	var sawRemoval bool
	for _, e := range events {
		sawRemoval = sawRemoval || e.Message == "removed from manifest"
	}
	if !sawRemoval || len(events) < 2 {
		t.Errorf("history was not kept: %+v", events)
	}
}

// A manifest deleted while the daemon was down leaves a row the loop never
// saw go. It must read as removed too, not as whatever it last was.
func TestRemoved_RowLeftFromAnEarlierRun(t *testing.T) {
	h := newHarness(t)
	if err := h.store.UpsertService(state.ServiceRecord{Name: "ghost", Runtime: "process", Status: "stopped", RestartCount: 2}); err != nil {
		t.Fatal(err)
	}
	h.apply()
	h.tick()

	st, ok := listed(t, h, "ghost")
	if !ok || st.Status != StatusRemoved || st.RestartCount != 2 {
		t.Errorf("list shows %+v (present %v); want ghost as removed with its restarts", st, ok)
	}
}

// Starting a removed service says what to do instead of "no such service"
// for something list is showing.
func TestRemoved_StartExplains(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.removeManifest("alpha")
	h.apply()
	h.tick()

	err := h.sup.StartService(context.Background(), "alpha")
	if err == nil || !strings.Contains(err.Error(), "outpost remove alpha") {
		t.Errorf("start of a removed service: %v", err)
	}
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("a removed service was started again (starts = %d)", got)
	}
	if err := h.sup.StartService(context.Background(), "nobody"); err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Errorf("start of an unknown service: %v", err)
	}
}

// Restoring the manifest brings a removed service back.
func TestRemoved_RestoringTheManifestBringsItBack(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.removeManifest("alpha")
	h.apply()
	h.tick()

	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("starts = %d; want it started again", got)
	}
	if st, _ := listed(t, h, "alpha"); st.Status == StatusRemoved {
		t.Errorf("restored service still listed as removed: %+v", st)
	}
}

// Forget clears a removed service's row and events.
func TestForget_ClearsARemovedService(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.removeManifest("alpha")
	h.apply()
	h.tick()

	if err := h.sup.Forget(context.Background(), "alpha"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, ok, _ := h.store.GetService("alpha"); ok {
		t.Error("row survived Forget")
	}
	if _, ok := listed(t, h, "alpha"); ok {
		t.Error("list still shows alpha")
	}
	if events, _ := h.sup.Events(context.Background(), "alpha", 50); len(events) != 0 {
		t.Errorf("events survived Forget: %+v", events)
	}
}

// `outpost remove` deletes the manifest and forgets at once, before any tick:
// Forget must notice the manifest is gone and stop the service itself.
func TestForget_StopsAServiceWhoseManifestWasJustDeleted(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	h.removeManifest("alpha")
	if err := h.sup.Forget(context.Background(), "alpha"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Errorf("stops = %d; want the running service stopped", got)
	}
	if _, ok, _ := h.store.GetService("alpha"); ok {
		t.Error("row survived Forget")
	}
	h.tick()
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("a forgotten service was started again (starts = %d)", got)
	}
}

// Forget refuses a declared service and reports an unknown one.
func TestForget_RefusesDeclaredAndUnknown(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if err := h.sup.Forget(context.Background(), "alpha"); err == nil || !strings.Contains(err.Error(), "still declared") {
		t.Errorf("Forget of a declared service: %v", err)
	}
	if got := h.rt.stopCount("alpha"); got != 0 {
		t.Errorf("a declared service was stopped by Forget (stops = %d)", got)
	}
	if err := h.sup.Forget(context.Background(), "nobody"); err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Errorf("Forget of an unknown service: %v", err)
	}
}
