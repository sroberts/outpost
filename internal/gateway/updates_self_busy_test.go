package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestSelfOutcome_IgnoresALeadingV(t *testing.T) {
	for _, c := range []struct {
		name    string
		st      SelfStatus
		running string
		want    string
		alert   bool
	}{
		{"updated, tag without v", SelfStatus{To: "1.1.0", From: "v1.0.0 (a)"}, "v1.1.0 (b)", "Outpost was updated to 1.1.0.", false},
		{"updated, build without v", SelfStatus{To: "v1.1.0", From: "v1.0.0 (a)"}, "1.1.0 (b)", "Outpost was updated to v1.1.0.", false},
		{"rolled back, from without v", SelfStatus{Failed: true, RolledBack: true, To: "v1.1.0", From: "1.0.0 (a)"}, "v1.0.0 (b)",
			"Outpost went back to 1.0.0, which is running.", false},
		{"refused, from with v", SelfStatus{Failed: true, To: "v1.1.0", From: "v1.0.0 (a)", Error: "no checksum"}, "1.0.0",
			"Outpost wasn't updated: no checksum. Nothing was changed.", false},
		{"stopped partway", SelfStatus{Failed: true, To: "1.1.0", From: "1.0.0"}, "v1.1.0 (b)",
			"The update stopped partway. Outpost is running v1.1.0", true},
	} {
		got, alert := selfOutcome(c.st, c.running)
		if !strings.Contains(got, c.want) || alert != c.alert {
			t.Errorf("%s: %q alert=%v; want %q alert=%v", c.name, got, alert, c.want, c.alert)
		}
	}
}

// selfOnly stands in for a daemon whose updater has nothing for the apps.
func selfAndControl(t *testing.T) (*harness, *fakeControl, *fakeSelf) {
	t.Helper()
	h, ctl := actionHarness(t)
	h.g.updater = &fakeUpdates{}
	t.Cleanup(h.settled)
	f := &fakeSelf{latest: ReleaseCheck{Latest: "v9.0.0", Newer: true}}
	h.g.selfUpdater = f
	return h, ctl, f
}

// The catalog's actions wait for Outpost: its restart would cut them short.
func TestCatalogActionsAreRefusedWhileOutpostUpdates(t *testing.T) {
	h, ctl, f := selfAndControl(t)
	f.set(SelfStatus{Phase: "downloading"})
	c := h.client(true)
	h.login(c, "root", "", "/")

	resp := h.post(c, portal+"/apps", portal, actForm("restart", "yarr"))
	if b := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "Outpost is being updated") {
		t.Errorf("restart during Outpost's update = %d:\n%s", resp.StatusCode, b)
	}
	h.opsSettled()
	if got := ctl.called(); len(got) != 0 {
		t.Errorf("the supervisor was asked to %v", got)
	}
}

// And Outpost waits for them.
func TestSelfUpdateIsRefusedWhileACatalogActionRuns(t *testing.T) {
	h, ctl, f := selfAndControl(t)
	ctl.gate = make(chan struct{})
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	h.post(c, portal+"/apps", portal, actForm("backup", "yarr"))
	resp := h.post(c, portal+"/updates", portal, selfForm)
	b := body(t, resp)
	close(ctl.gate)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(b, "Yarr is being backed up") {
		t.Errorf("Outpost's update during a backup = %d:\n%s", resp.StatusCode, b)
	}
	if got := f.started(); len(got) != 0 {
		t.Errorf("Start was called by %v", got)
	}
}

// countingSelf counts how often the daemon is asked how an update stands.
type countingSelf struct {
	*fakeSelf
	mu    sync.Mutex
	reads int
}

func (c *countingSelf) Status() (SelfStatus, bool) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.fakeSelf.Status()
}

func (c *countingSelf) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// The catalog asks once per app; the file is read once a second at most.
func TestSelfStateIsRememberedBriefly(t *testing.T) {
	h, _ := updateHarness(t)
	f := &countingSelf{fakeSelf: &fakeSelf{}}
	h.g.selfUpdater = f
	for range 20 {
		h.g.selfBusy()
	}
	if n := f.count(); n != 1 {
		t.Errorf("read %d times; want 1", n)
	}
	// What an admin acts on is read afresh.
	h.g.selfBusyNow()
	if n := f.count(); n != 2 {
		t.Errorf("read %d times; want 2", n)
	}
}

// expiredSelf has an outcome that is too old to show, never recorded.
type expiredSelf struct {
	*fakeSelf
	mu   sync.Mutex
	told bool
}

func (e *expiredSelf) Status() (SelfStatus, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := SelfStatus{Phase: "done", Final: true, To: "v9.0.0", By: "root", Announce: !e.told}
	e.told = true
	return st, false
}

func TestSelfOutcomeTooOldToShowIsStillRecorded(t *testing.T) {
	h, _ := updateHarness(t)
	feed := h.recording()
	h.g.selfUpdater = &expiredSelf{fakeSelf: &fakeSelf{}}
	h.g.noteSelfUpdate()
	got := feed.wait(t, 1)
	if !strings.Contains(got[0].message, "update of Outpost to v9.0.0, which root started, finished") {
		t.Errorf("outcome = %+v", got[0])
	}
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	if b := body(t, h.get(c, portal+"/updates")); strings.Contains(b, "Outpost was updated") {
		t.Errorf("an outcome over a day old is shown:\n%s", b)
	}
}
