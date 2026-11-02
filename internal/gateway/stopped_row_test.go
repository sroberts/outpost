package gateway

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// Stopped on purpose isn't an alarm: it has its own state, nothing to go and
// inspect, and Start in the row rather than behind the menu.
func TestStoppedOnPurposeIsNotDown(t *testing.T) {
	h, _ := actionHarness(t)
	b := body(t, h.login(h.client(true), "root", "", "/"))

	row := rowOf(t, b, "notes")
	if !strings.Contains(b, `<li class="stop stopped managed" id="app-notes"`) {
		t.Errorf("notes isn't drawn as stopped:\n%s", row)
	}
	if strings.Contains(row, `class="state">Down`) || strings.Contains(row, "outpost status") {
		t.Errorf("a stopped app reads as a fault:\n%s", row)
	}
	form, manage := strings.Index(row, `class="quick"`), strings.Index(row, `class="acts"`)
	if form < 0 || manage < 0 || form > manage {
		t.Errorf("Start should come before Manage, in the row:\n%s", row)
	}
	if strings.Count(row, `name="action" value="start"`) != 1 {
		t.Errorf("Start should be offered once:\n%s", row)
	}
	if !strings.Contains(b, "· 1 stopped") {
		t.Errorf("the tally should count it apart from anything down:\n%s", b)
	}

	// One that's crashed is still down, and still says how to look closer.
	if row := rowOf(t, b, "atlas"); !strings.Contains(row, `class="state">Down`) || !strings.Contains(row, "outpost status atlas") {
		t.Errorf("a halted app should read as down:\n%s", row)
	}
}

// The sampler lags a stop by up to half a minute; the row shouldn't.
func TestStoppedAppShowsNoLiveFigures(t *testing.T) {
	h, _ := actionHarness(t)
	h.g.stats = fakeStats{"notes": busyUsage(h.g.now())}
	row := rowOf(t, body(t, h.login(h.client(true), "root", "", "/")), "notes")
	if strings.Contains(row, "% CPU ·") || strings.Contains(row, "memory ·") {
		t.Errorf("a stopped app shows live figures:\n%s", row)
	}
	if !strings.Contains(row, "stored") {
		t.Errorf("a stopped app still has its storage:\n%s", row)
	}
}

func TestSigningOutOfOtherDevicesAsksFirst(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")
	b := body(t, h.get(c, portal+"/account"))
	i := strings.Index(b, `value="everywhere"`)
	if i < 0 || strings.LastIndex(b[:i], `<details class="confirm">`) < strings.LastIndex(b[:i], `</details>`) {
		t.Errorf("Sign out everywhere isn't behind a confirmation:\n%s", b)
	}
}

func TestAccountLinksUpdatesForAnAdminOnly(t *testing.T) {
	h, _ := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/account")); !strings.Contains(b, `href="/updates"`) {
		t.Errorf("an admin's account page has no link to updates:\n%s", b)
	}
	c = h.client(true)
	h.login(c, "alice", "", "/")
	if b := body(t, h.get(c, portal+"/account")); strings.Contains(b, `href="/updates"`) {
		t.Errorf("a plain user's account page links to updates:\n%s", b)
	}
}

func TestUpdatingAnAppAsksFirstOnTheUpdatesPage(t *testing.T) {
	h, f := updateHarness(t)
	f.latest["yarr"] = ReleaseCheck{Latest: "v2.0.0", Newer: true}
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	b := body(t, h.get(c, portal+"/updates"))
	i := strings.Index(b, `value="update"`)
	if i < 0 || strings.LastIndex(b[:i], `<details class="confirm">`) < 0 {
		t.Errorf("Update isn't behind a confirmation:\n%s", b)
	}
}

func TestWaitWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		400 * time.Millisecond:  "1 second",
		time.Second:             "1 second",
		1500 * time.Millisecond: "2 seconds",
		60 * time.Second:        "60 seconds",
	} {
		if got := waitWords(d); got != want {
			t.Errorf("waitWords(%v) = %q; want %q", d, got, want)
		}
	}
}

// A stopped app's address can't open it, so it doesn't offer to; an admin who
// reaches it anyway is sent to where Start is, not to a terminal.
func TestStoppedAppIsNotADeadEnd(t *testing.T) {
	h, _ := actionHarness(t)
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))
	if row := rowOf(t, b, "notes"); strings.Contains(row, "open anyway") {
		t.Errorf("a stopped app offers to open anyway:\n%s", row)
	}
	page := body(t, h.get(c, strings.Replace(yarr, "yarr", "notes", 1)+"/"))
	if strings.Contains(page, "outpost status") || !strings.Contains(page, "#app-notes") {
		t.Errorf("the stopped app's page should lead to its row:\n%s", page)
	}
}

// A script a page loads has to be there: an asset missing from the embed list
// still gets a route and a tag, and only fails in the browser.
func TestEverySignedInPageCanLoadItsMenus(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	b := body(t, h.login(c, "alice", "", "/"))
	if !strings.Contains(b, menusURL) || !strings.Contains(menusURL, "?v=") {
		t.Fatalf("the catalog doesn't load %s:\n%s", menusURL, b)
	}
	if resp := h.get(c, portal+menusURL); resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d", menusURL, resp.StatusCode)
	}
}

// withNothingComingBack drops the harness's starting app, which would otherwise
// keep the catalog refreshing.
func withNothingComingBack(h *harness) {
	h.withServices(slices.DeleteFunc(slices.Clone(actionServices), func(s supervisor.ServiceStatus) bool {
		return s.Status == "starting"
	})...)
}

// A crash is nobody's action, so nothing else would refresh the row while the
// app comes back, and that holds for anyone who can see it.
func TestCatalogPollsWhileAnAppIsComingBack(t *testing.T) {
	h, _ := actionHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "failed", Desired: "running", NextAttempt: time.Now().Add(time.Minute)})
	for _, who := range []string{"root", "alice"} {
		if b := body(t, h.login(h.client(true), who, "", "/")); !strings.Contains(b, pollPath) {
			t.Errorf("%s: a restarting app doesn't refresh the catalog:\n%s", who, b)
		}
	}
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running"})
	if b := body(t, h.login(h.client(true), "root", "", "/")); strings.Contains(b, pollPath) {
		t.Errorf("an idle catalog is polling:\n%s", b)
	}
}

// A crashed app has no process, whatever the sampler last read.
func TestRestartingAppShowsNoLiveFigures(t *testing.T) {
	h, _ := actionHarness(t)
	h.g.stats = fakeStats{"yarr": busyUsage(h.g.now())}
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "failed", Desired: "running", NextAttempt: time.Now().Add(time.Minute)})
	row := rowOf(t, body(t, h.login(h.client(true), "root", "", "/")), "yarr")
	if !strings.Contains(row, "Restarting") || strings.Contains(row, "% CPU ·") || strings.Contains(row, "memory ·") {
		t.Errorf("a restarting app shows live figures:\n%s", row)
	}
}
