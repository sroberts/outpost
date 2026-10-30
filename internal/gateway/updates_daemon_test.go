package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

// A release the daemon's daily check noticed reaches the page and the catalog
// without an admin's visit having to ask, and without waiting out the hour the
// page's own check is good for.
func TestDaemonCheckReachesThePage(t *testing.T) {
	h, f := updateHarness(t)
	f.latest["yarr"] = ReleaseCheck{Latest: "v1.0.0"} // up to date, as far as a page asks
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	if strings.Contains(body(t, h.get(c, portal+"/")), "update available") {
		t.Fatal("nothing should be waiting yet")
	}
	asked := f.checks

	at := time.Now()
	h.withServices(
		supervisor.ServiceStatus{
			Name: "yarr", Status: "running", Desired: "running", Title: "Yarr", Source: "o/yarr", Release: "v1.0.0",
			LatestRelease: "v3.0.0", UpdateAvailable: true, ReleaseCheckedAt: at,
		},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Desired: "running"},
	)

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, "1 update available") {
		t.Errorf("catalog doesn't count it:\n%s", b)
	}
	if b := body(t, h.get(c, portal+"/updates")); !strings.Contains(b, "v3.0.0 is available") || !strings.Contains(b, `name="service" value="yarr"`) {
		t.Errorf("page doesn't offer it:\n%s", b)
	}
	h.settled()
	if f.checks != asked {
		t.Errorf("%d more checks; the page should have used the daemon's answer", f.checks-asked)
	}
}

// The daemon keeps its last answer through a failed check. That old answer
// coming round again mustn't replace the failure the page's own check found.
func TestDaemonCheckDoesNotHideAFailure(t *testing.T) {
	h, f := updateHarness(t)
	at := time.Now()
	list := []supervisor.ServiceStatus{{
		Name: "yarr", Status: "running", Desired: "running", Source: "o/yarr", Release: "v1.0.0",
		LatestRelease: "v3.0.0", UpdateAvailable: true, ReleaseCheckedAt: at,
	}}
	h.withServices(list...)

	f.checkErr = errors.New("rate limited")
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	h.withServices(list...) // what the next route refresh sees
	if b := body(t, h.get(c, portal+"/updates")); !strings.Contains(b, "Couldn&#39;t check: rate limited") {
		t.Errorf("the failure is gone:\n%s", b)
	}
}

// An answer about a release that has since changed isn't an offer, wherever
// it came from.
func TestDaemonCheckForAnOldReleaseIsNotOffered(t *testing.T) {
	h, _ := updateHarness(t)
	h.g.updateState.takeDaemonChecks([]supervisor.ServiceStatus{{
		Name: "yarr", Source: "o/yarr", Release: "v0.9.0",
		LatestRelease: "v3.0.0", UpdateAvailable: true, ReleaseCheckedAt: time.Now(),
	}})
	if n := h.g.waiting(); n != 0 {
		t.Errorf("waiting = %d", n)
	}
}
