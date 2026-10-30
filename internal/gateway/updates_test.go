package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeUpdates stands in for the daemon's updater.
type fakeUpdates struct {
	mu       sync.Mutex
	latest   map[string]ReleaseCheck
	checkErr error
	checks   int
	updates  []string
	gate     chan struct{} // when set, Update waits on it
	output   string
	err      error
	panics   bool
	after    func() // what the update changed, run as it finishes
}

func (f *fakeUpdates) Latest(_ context.Context, service string) (ReleaseCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.latest[service], f.checkErr
}

func (f *fakeUpdates) Update(service string) (string, error) {
	f.mu.Lock()
	f.updates = append(f.updates, service)
	gate, after, panics := f.gate, f.after, f.panics
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if panics {
		panic("boom")
	}
	if after != nil {
		after()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.output, f.err
}

func (f *fakeUpdates) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
}

// settled waits for background checks and updates to finish. The test log is
// off limits once a test returns, and they log.
func (h *harness) settled() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := &h.g.updateState
		s.mu.Lock()
		busy := s.checking || len(s.running) > 0
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("updates never settled")
}

// updateHarness has yarr installed from a release, with a newer one out, wiki
// declared by hand, and an admin and a plain user.
func updateHarness(t *testing.T) (*harness, *fakeUpdates) {
	t.Helper()
	h := newHarness(t)
	f := &fakeUpdates{latest: map[string]ReleaseCheck{"yarr": {Latest: "v2.0.0", Newer: true}}}
	h.g.updater = f
	t.Cleanup(h.settled)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Title: "Yarr", Source: "o/yarr", Release: "v1.0.0"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Desired: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

func updateForm(service string) url.Values {
	return url.Values{"action": {"update"}, "service": {service}}
}

func TestUpdatePageOffersWhatsNewerAndUpdates(t *testing.T) {
	h, f := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled() // opening the portal started the first check

	b := body(t, h.get(c, portal+"/"))
	if !strings.Contains(b, "1 update available") {
		t.Errorf("catalog doesn't say an update is waiting:\n%s", b)
	}
	b = body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{"v1.0.0 from o/yarr", "v2.0.0 is available", `name="service" value="yarr"`, "Not installed from a release: wiki"} {
		if !strings.Contains(b, want) {
			t.Errorf("update page lacks %q:\n%s", want, b)
		}
	}

	f.gate = make(chan struct{})
	f.output = "yarr is running v2.0.0 and healthy (was v1.0.0)\n"
	f.after = func() {
		h.withServices(
			supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Title: "Yarr", Source: "o/yarr", Release: "v2.0.0"},
			supervisor.ServiceStatus{Name: "wiki", Status: "running", Desired: "running"},
		)
		f.mu.Lock()
		f.latest["yarr"] = ReleaseCheck{Latest: "v2.0.0"}
		f.mu.Unlock()
	}
	resp := h.post(c, portal+"/updates", portal, updateForm("yarr"))
	b = body(t, resp)
	if resp.Request.URL.Path != "/updates" || !strings.Contains(b, "Updating.") {
		t.Errorf("after asking: %s\n%s", resp.Request.URL, b)
	}
	if !strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the page doesn't refresh while an update runs")
	}
	if strings.Contains(b, `name="service" value="yarr"`) {
		t.Error("the page still offers the update that's running")
	}

	// A second tap while it runs changes nothing.
	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	close(f.gate)
	h.settled()
	if got := f.calls(); len(got) != 1 || got[0] != "yarr" {
		t.Errorf("updates run = %v; want yarr, once", got)
	}

	b = body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{"v2.0.0 from o/yarr", "running v2.0.0 and healthy", "Up to date"} {
		if !strings.Contains(b, want) {
			t.Errorf("after the update, page lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the page keeps refreshing with nothing running")
	}
	if strings.Contains(body(t, h.get(c, portal+"/")), "update available") {
		t.Error("the catalog still counts the update just installed")
	}
}

func TestOnlyAnAdminCanUpdate(t *testing.T) {
	h, f := updateHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")

	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, "/updates") {
		t.Errorf("the catalog links someone who isn't an admin to updates:\n%s", b)
	}
	if resp := h.get(plain, portal+"/updates"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET as a plain user = %d", resp.StatusCode)
	}
	if resp := h.post(plain, portal+"/updates", portal, updateForm("yarr")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}

	anon := h.client(true)
	if resp := h.get(anon, portal+"/updates"); resp.Request.URL.Path != "/login" {
		t.Errorf("GET signed out landed at %s", resp.Request.URL)
	}
	if resp := h.post(anon, portal+"/updates", portal, updateForm("yarr")); resp.Request.URL.Path != "/login" {
		t.Errorf("POST signed out landed at %s", resp.Request.URL)
	}

	// Admin is read fresh on each request, so revoking it counts at once.
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	h.settled()
	h.addUser(auth.User{Username: "root2", Admin: true}) // the last admin can't be demoted
	if err := h.store.SetAdmin("root", false); err != nil {
		t.Fatal(err)
	}
	if resp := h.post(admin, portal+"/updates", portal, updateForm("yarr")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST after losing admin = %d", resp.StatusCode)
	}

	if got := f.calls(); len(got) != 0 {
		t.Errorf("updates ran: %v", got)
	}
}

func TestUpdateRefusals(t *testing.T) {
	h, f := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	for name, tc := range map[string]struct {
		origin, service string
		want            int
	}{
		"another site's form":  {"http://evil.test", "yarr", http.StatusForbidden},
		"no origin at all":     {"", "yarr", http.StatusForbidden},
		"an app's own origin":  {yarr, "yarr", http.StatusForbidden},
		"no source to use":     {portal, "wiki", http.StatusNotFound},
		"no such service":      {portal, "nope", http.StatusNotFound},
		"not a service at all": {portal, "--force", http.StatusNotFound},
		"a path":               {portal, "../../etc/passwd", http.StatusNotFound},
		"nothing":              {portal, "", http.StatusNotFound},
	} {
		if resp := h.post(c, portal+"/updates", tc.origin, updateForm(tc.service)); resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d; want %d", name, resp.StatusCode, tc.want)
		}
	}
	if got := f.calls(); len(got) != 0 {
		t.Errorf("updates ran: %v", got)
	}

	// On an app's host the path is the app's, not the gateway's.
	h.post(c, yarr+"/updates", yarr, updateForm("yarr"))
	if got := f.calls(); len(got) != 0 {
		t.Errorf("a POST on an app's host ran an update: %v", got)
	}
}

func TestFailedUpdateSaysWhyAndOffersItAgain(t *testing.T) {
	h, f := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	f.output = "downloading…\n"
	f.err = errors.New("yarr v2.0.0 did not come up healthy; rolled back to <v1.0.0>")
	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	h.settled()

	b := body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "rolled back to &lt;v1.0.0&gt;") {
		t.Errorf("page doesn't say why, escaped:\n%s", b)
	}
	if !strings.Contains(b, `role="alert"`) || !strings.Contains(b, `name="service" value="yarr"`) {
		t.Errorf("page doesn't flag the failure and offer it again:\n%s", b)
	}
}

// A panic on a goroutine ends the process, and with it every supervised
// service. That can't be what a bad archive from a release does.
func TestPanicInAnUpdateDoesNotKillTheDaemon(t *testing.T) {
	h, f := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	f.panics = true
	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	h.settled()
	b := body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "The update stopped unexpectedly") {
		t.Errorf("page doesn't say it failed:\n%s", b)
	}
}

func TestCheckFailureIsShownAndChecksAreThrottled(t *testing.T) {
	h, f := updateHarness(t)
	f.checkErr = errors.New("rate limited")
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	if b := body(t, h.get(c, portal+"/updates")); !strings.Contains(b, "Couldn&#39;t check: rate limited") {
		t.Errorf("page doesn't show the failed check:\n%s", b)
	}
	h.settled()
	h.get(c, portal+"/")
	h.get(c, portal+"/updates")
	h.settled()
	f.mu.Lock()
	n := f.checks
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("%d checks; opening pages should reuse the last one for an hour", n)
	}

	h.post(c, portal+"/updates", portal, url.Values{"action": {"check"}})
	h.settled()
	f.mu.Lock()
	n = f.checks
	f.mu.Unlock()
	if n != 2 {
		t.Errorf("%d checks; Check now should ask again", n)
	}
}

// An answer about a release that's no longer installed says nothing, so an
// update from the CLI can't leave the page offering what's already there, and
// the next page asks again rather than waiting out the hour.
func TestCheckIsRedoneWhenTheReleaseChanges(t *testing.T) {
	h, f := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	// Block the fresh check, to see the page before it answers.
	unblock := make(chan struct{})
	f.mu.Lock()
	f.latest["yarr"] = ReleaseCheck{Latest: "v2.0.0"}
	f.mu.Unlock()
	h.g.updater = slowLatest{f, unblock}
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Source: "o/yarr", Release: "v2.0.0"})

	b := body(t, h.get(c, portal+"/updates"))
	close(unblock)
	if strings.Contains(b, `name="service"`) || !strings.Contains(b, "Checking.") {
		t.Errorf("page acts on the old answer:\n%s", b)
	}
	if strings.Contains(body(t, h.get(c, portal+"/")), "update available") {
		t.Error("the catalog counts the old answer")
	}
	h.settled()
	if b := body(t, h.get(c, portal+"/updates")); !strings.Contains(b, "Up to date") {
		t.Errorf("the fresh answer didn't arrive:\n%s", b)
	}
	h.settled()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checks != 2 {
		t.Errorf("%d checks; want the first and one for the change, then no more", f.checks)
	}
}

// slowLatest holds a check until told to answer.
type slowLatest struct {
	*fakeUpdates
	unblock chan struct{}
}

func (s slowLatest) Latest(ctx context.Context, service string) (ReleaseCheck, error) {
	<-s.unblock
	return s.fakeUpdates.Latest(ctx, service)
}

func TestNoUpdatePageWithoutAnUpdater(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))
	if strings.Contains(b, "/updates") {
		t.Errorf("catalog links to a page that isn't there:\n%s", b)
	}
	if resp := h.get(c, portal+"/updates"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET = %d", resp.StatusCode)
	}
}

func TestUpdatesAreRecordedWithWhoAsked(t *testing.T) {
	h, f := updateHarness(t)
	feed := h.recording()
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	feed.wait(t, 1) // the sign-in

	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	h.settled()
	got := feed.wait(t, 3)
	if got[1].kind != EventUpdate || !strings.Contains(got[1].message, "root started an update of yarr") {
		t.Errorf("start = %+v", got[1])
	}
	if !strings.Contains(got[2].message, "update of yarr finished") {
		t.Errorf("end = %+v", got[2])
	}

	f.err = errors.New("no")
	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	h.settled()
	if got := feed.wait(t, 5); !strings.Contains(got[4].message, "update of yarr failed") {
		t.Errorf("failure = %+v", got[4])
	}
}
