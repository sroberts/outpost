package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

var allForm = url.Values{"action": {"all"}}

// allHarness is updateHarness with a second app, sous, also behind its
// latest release.
func allHarness(t *testing.T) (*harness, *fakeUpdates) {
	t.Helper()
	h, f := updateHarness(t)
	f.latest["sous"] = ReleaseCheck{Latest: "v0.14.0", Newer: true}
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Title: "Yarr", Source: "o/yarr", Release: "v1.0.0"},
		supervisor.ServiceStatus{Name: "sous", Status: "running", Desired: "running", Title: "Sous", Source: "o/sous", Release: "v0.13.0"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Desired: "running"},
	)
	return h, f
}

// queueDone waits for an Update all to work through its queue: between two
// updates nothing is running, so settled alone could return in the gap.
func (h *harness) queueDone() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := &h.g.updateState
		s.mu.Lock()
		busy := s.checking || len(s.running) > 0 || len(s.queue) > 0
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("the queue never emptied")
}

func TestUpdateAllRunsEachInTurn(t *testing.T) {
	h, f := allHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	b := body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{"2 apps have newer releases", `name="action" value="all"`, "Update all 2 apps"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q:\n%s", want, b)
		}
	}

	f.gate = make(chan struct{})
	h.post(c, portal+"/updates", portal, allForm)
	b = body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "Updating.") || !strings.Contains(b, "Waiting its turn for v2.0.0.") || !strings.Contains(b, `http-equiv="refresh"`) {
		t.Errorf("while the first runs, the page doesn't show the second waiting:\n%s", b)
	}
	if strings.Contains(b, `value="all"`) || strings.Contains(b, `name="service"`) {
		t.Errorf("buttons offered while an update all is under way:\n%s", b)
	}
	// A second press while it runs starts nothing more.
	h.post(c, portal+"/updates", portal, allForm)
	close(f.gate)
	h.queueDone()
	if got := f.calls(); strings.Join(got, ",") != "sous,yarr" {
		t.Errorf("updated %v; want sous then yarr, once each", got)
	}
}

func TestUpdateAllStopsAtTheFirstFailure(t *testing.T) {
	h, f := allHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	f.err = errors.New("sous v0.14.0 did not come up healthy; rolled back to v0.13.0")
	h.post(c, portal+"/updates", portal, allForm)
	h.queueDone()
	if got := f.calls(); strings.Join(got, ",") != "sous" {
		t.Errorf("updated %v; want only sous, the run stopping when it failed", got)
	}
	b := body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "rolled back to v0.13.0") || !strings.Contains(b, `role="alert"`) || !strings.Contains(b, `name="service" value="yarr"`) {
		t.Errorf("page doesn't say why and offer yarr on its own:\n%s", b)
	}
}

func TestUpdateAllHoldsOutpostsOwnUpdate(t *testing.T) {
	h, f := allHarness(t)
	self := &fakeSelf{latest: ReleaseCheck{Latest: "v9.0.0", Newer: true}}
	h.g.selfUpdater = self
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	// Outpost isn't one of "all": its restart would cut the rest short.
	if b := body(t, h.get(c, portal+"/updates")); !strings.Contains(b, "Update all 2 apps") {
		t.Errorf("Outpost counted among the apps:\n%s", b)
	}
	f.gate = make(chan struct{})
	h.post(c, portal+"/updates", portal, allForm)
	resp := h.post(c, portal+"/updates", portal, selfForm)
	if resp.StatusCode != http.StatusConflict || len(self.started()) != 0 {
		t.Errorf("Outpost's update started during an update all: %d %v", resp.StatusCode, self.started())
	}
	close(f.gate)
	h.queueDone()
}

func TestUpdateAllWaitsForOutpost(t *testing.T) {
	h, f := allHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	self := &fakeSelf{latest: ReleaseCheck{Latest: "v9.0.0", Newer: true}}
	self.set(SelfStatus{To: "v9.0.0"})
	h.g.selfUpdater = self

	resp := h.post(c, portal+"/updates", portal, allForm)
	if resp.StatusCode != http.StatusConflict || len(f.calls()) != 0 {
		t.Errorf("update all ran while Outpost was updating: %d %v", resp.StatusCode, f.calls())
	}
}
