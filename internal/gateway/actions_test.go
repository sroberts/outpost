package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeControl stands in for the supervisor's start, stop, restart and backup.
type fakeControl struct {
	mu    sync.Mutex
	calls []string      // "stop yarr"
	gate  chan struct{} // when set, Backup waits on it
	err   error
}

func (f *fakeControl) did(what, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what+" "+name)
	return f.err
}

func (f *fakeControl) StartService(_ context.Context, name string) error {
	return f.did("start", name)
}

func (f *fakeControl) StopService(_ context.Context, name string) error {
	return f.did("stop", name)
}

func (f *fakeControl) RestartService(_ context.Context, name string) error {
	return f.did("restart", name)
}

func (f *fakeControl) Backup(_ context.Context, name string) (backup.Archive, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return backup.Archive{Service: name, Path: "/backups/" + name + ".tar.gz"}, f.did("backup", name)
}

func (f *fakeControl) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// opsSettled waits for actions from the portal to finish. The test log is off
// limits once a test returns, and they log.
func (h *harness) opsSettled() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.g.ops.mu.Lock()
		busy := len(h.g.ops.busy) > 0
		h.g.ops.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("actions never settled")
}

// actionServices is one app in each state the menu tells apart.
var actionServices = []supervisor.ServiceStatus{
	{Name: "yarr", Status: "running", Desired: "running", Title: "Yarr"},
	{Name: "boot", Status: "starting", Desired: "running"},
	{Name: "notes", Status: "stopped", Desired: "stopped"},
	{Name: "atlas", Status: "stopped", Desired: "running", Halted: true, RestartCount: 3},
}

// actionHarness can control apps, and has an admin and a plain user.
func actionHarness(t *testing.T) (*harness, *fakeControl) {
	t.Helper()
	h := newHarness(t)
	f := &fakeControl{}
	h.g.control = f
	t.Cleanup(h.opsSettled)
	h.withServices(actionServices...)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

func actForm(action, service string) url.Values {
	return url.Values{"action": {action}, "service": {service}}
}

// rowOf is one app's row of the catalog.
func rowOf(t *testing.T, page, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<li [^>]*id="app-` + regexp.QuoteMeta(name) + `".*?</li>`).FindString(page)
	if m == "" {
		t.Fatalf("no row for %s:\n%s", name, page)
	}
	return m
}

func offersAction(row, action string) bool {
	return strings.Contains(row, `name="action" value="`+action+`"`)
}

func TestAdminSeesManageWithWhatEachStateAllows(t *testing.T) {
	h, _ := actionHarness(t)
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))

	for name, want := range map[string][]string{
		"yarr":  {"restart", "backup", "stop"},
		"boot":  {"restart", "stop"},
		"notes": {"start", "backup"},
		"atlas": {"start", "stop"},
	} {
		row := rowOf(t, b, name)
		if !strings.Contains(row, `<summary class="pill"><span>Manage<span class="sr"> `) {
			t.Errorf("%s has no Manage menu:\n%s", name, row)
		}
		for _, a := range []string{"start", "stop", "restart", "backup"} {
			offered := false
			for _, w := range want {
				offered = offered || w == a
			}
			if got := offersAction(row, a); got != offered {
				t.Errorf("%s offers %s = %v; want %v", name, a, got, offered)
			}
		}
		if strings.Contains(row, "/updates") {
			t.Errorf("%s offers an update with none waiting:\n%s", name, row)
		}
	}

	// Stop, Restart and Back up ask first; Start doesn't.
	yarrRow := rowOf(t, b, "yarr")
	for _, want := range []string{
		`<details class="confirm" name="confirm-yarr"><summary class="pill danger"><span>Stop<span class="sr"> Yarr</span>…</span></summary>`,
		"Stop Yarr? It stays stopped until someone starts it.",
		"Restart Yarr? It&#39;s briefly unavailable",
		"Back up Yarr now? Yarr is unavailable while its storage is archived",
		`><span>Stop Yarr</span></button>`,
	} {
		if !strings.Contains(yarrRow, want) {
			t.Errorf("yarr's menu lacks %q:\n%s", want, yarrRow)
		}
	}
	notesRow := rowOf(t, b, "notes")
	if strings.Contains(notesRow, `<summary class="pill"><span>Start`) || !strings.Contains(notesRow, `><span>Start<span class="sr"> notes</span></span></button>`) {
		t.Errorf("Start should be one step:\n%s", notesRow)
	}
	if !strings.Contains(notesRow, "notes stays stopped while its storage is archived") {
		t.Errorf("a stopped app's backup doesn't say it stays stopped:\n%s", notesRow)
	}

	// The whole of a running app's row still opens it, and nothing to press
	// sits inside that link.
	if !strings.Contains(yarrRow, `<a class="name go" href="http://yarr.outpost.test/">Yarr</a>`) {
		t.Errorf("yarr's name isn't its link:\n%s", yarrRow)
	}
	for _, m := range regexp.MustCompile(`(?s)<a [^>]*>(.*?)</a>`).FindAllStringSubmatch(b, -1) {
		if strings.Contains(m[1], "<form") || strings.Contains(m[1], "<details") || strings.Contains(m[1], "<button") {
			t.Errorf("a control inside a link: %s", m[0])
		}
	}
	if strings.Contains(b, "style=") || strings.Contains(b, "<script>") {
		t.Error("the page has inline style or script, which its CSP refuses")
	}
}

func TestPlainUserAndNoControlMeanNoMenu(t *testing.T) {
	h, _ := actionHarness(t)
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if strings.Contains(b, `action="/apps"`) || strings.Contains(b, "Manage") {
		t.Errorf("a plain user is offered the menu:\n%s", b)
	}

	bare := newHarness(t)
	bare.addUser(auth.User{Username: "root", Admin: true})
	c := bare.client(true)
	if b := body(t, bare.login(c, "root", "", "/")); strings.Contains(b, `action="/apps"`) || strings.Contains(b, "Manage") {
		t.Errorf("a menu without anything to drive it:\n%s", b)
	}
	if resp := bare.post(c, portal+"/apps", portal, actForm("stop", "yarr")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /apps without control = %d; want 404", resp.StatusCode)
	}
}

func TestEachActionCallsControlOnceAndComesBack(t *testing.T) {
	for _, tc := range []struct{ action, service string }{
		{"start", "notes"},
		{"stop", "yarr"},
		{"restart", "yarr"},
		{"backup", "yarr"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			h, f := actionHarness(t)
			c := h.client(false)
			h.login(c, "root", "", "/")
			resp := h.post(c, portal+"/apps", portal, actForm(tc.action, tc.service))
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/#note-"+tc.service {
				t.Errorf("= %d to %q; want 303 to /#note-%s", resp.StatusCode, resp.Header.Get("Location"), tc.service)
			}
			h.opsSettled()
			if got := f.called(); len(got) != 1 || got[0] != tc.action+" "+tc.service {
				t.Errorf("calls = %v", got)
			}
		})
	}
}

func TestActionRefusals(t *testing.T) {
	h, f := actionHarness(t)
	admin := h.client(false)
	h.login(admin, "root", "", "/")

	for name, tc := range map[string]struct {
		origin, action, service string
		want                    int
	}{
		"another site's form": {"http://evil.test", "stop", "yarr", http.StatusForbidden},
		"no origin at all":    {"", "stop", "yarr", http.StatusForbidden},
		"an app's own origin": {yarr, "stop", "yarr", http.StatusForbidden},
		"no such service":     {portal, "stop", "nope", http.StatusNotFound},
		"a path":              {portal, "stop", "../x", http.StatusNotFound},
		"a flag":              {portal, "stop", "-x", http.StatusNotFound},
		"nothing":             {portal, "stop", "", http.StatusNotFound},
		"no such action":      {portal, "explode", "yarr", http.StatusBadRequest},
		"no action":           {portal, "", "yarr", http.StatusBadRequest},
		"not for its state":   {portal, "start", "yarr", http.StatusBadRequest},
		"backup while down":   {portal, "backup", "atlas", http.StatusBadRequest},
	} {
		if resp := h.post(admin, portal+"/apps", tc.origin, actForm(tc.action, tc.service)); resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d; want %d", name, resp.StatusCode, tc.want)
		}
	}

	plain := h.client(false)
	h.login(plain, "alice", "", "/")
	if resp := h.post(plain, portal+"/apps", portal, actForm("stop", "yarr")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a plain user = %d; want 403", resp.StatusCode)
	}

	anon := h.client(false)
	resp := h.post(anon, portal+"/apps", portal, actForm("stop", "yarr"))
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Errorf("signed out = %d to %q; want the sign-in", resp.StatusCode, loc)
	}

	if resp := h.get(admin, portal+"/apps"); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Errorf("GET /apps = %d; want 405", resp.StatusCode)
	}

	// Admin is read fresh on each request, so revoking it counts at once.
	h.addUser(auth.User{Username: "root2", Admin: true}) // the last admin can't be demoted
	if err := h.store.SetAdmin("root", false); err != nil {
		t.Fatal(err)
	}
	if resp := h.post(admin, portal+"/apps", portal, actForm("stop", "yarr")); resp.StatusCode != http.StatusForbidden {
		t.Errorf("after losing admin = %d; want 403", resp.StatusCode)
	}

	// On an app's host the path is the app's, not the gateway's.
	h.post(admin, yarr+"/apps", yarr, actForm("stop", "yarr"))

	h.opsSettled()
	if got := f.called(); len(got) != 0 {
		t.Errorf("control was called: %v", got)
	}
}

func TestBusyAppShowsProgressThenTheOutcome(t *testing.T) {
	h, f := actionHarness(t)
	withNothingComingBack(h)
	// Set once, before any request, so moving it later races nothing.
	var ahead atomic.Int64
	h.g.now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	c := h.client(true)
	h.login(c, "root", "", "/")

	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	b := body(t, h.post(c, portal+"/apps", portal, actForm("backup", "yarr")))
	row := rowOf(t, b, "yarr")
	if !strings.Contains(row, `<p class="act-note" id="note-yarr" tabindex="-1" role="status">Backing up…</p>`) || strings.Contains(row, "Manage") {
		t.Errorf("a busy row should say what's under way and offer nothing:\n%s", row)
	}
	if !strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the catalog doesn't refresh while an action runs")
	}
	if !strings.Contains(rowOf(t, b, "notes"), "Manage") {
		t.Error("another app lost its menu while yarr is busy")
	}

	// A second tap while it runs changes nothing.
	h.post(c, portal+"/apps", portal, actForm("backup", "yarr"))
	close(gate)
	h.opsSettled()
	if got := f.called(); len(got) != 1 {
		t.Errorf("calls = %v; want one backup", got)
	}

	b = body(t, h.get(c, portal+"/"))
	row = rowOf(t, b, "yarr")
	if !strings.Contains(row, "Backed up by root · just now") || !strings.Contains(row, "Manage") {
		t.Errorf("after the backup, row:\n%s", row)
	}
	if strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the catalog keeps refreshing with nothing running")
	}

	f.mu.Lock()
	f.err = errors.New("disk <full>")
	f.mu.Unlock()
	h.post(c, portal+"/apps", portal, actForm("stop", "yarr"))
	h.opsSettled()
	row = rowOf(t, body(t, h.get(c, portal+"/")), "yarr")
	if !strings.Contains(row, `role="alert"`) || !strings.Contains(row, "Couldn&#39;t stop when root asked just now: disk &lt;full&gt;") {
		t.Errorf("a failure isn't flagged, escaped:\n%s", row)
	}

	// The catalog reloads while anything is busy; an old failure stays on
	// the row but isn't announced again on every reload.
	ahead.Store(int64(10 * time.Second))
	if row := rowOf(t, body(t, h.get(c, portal+"/")), "yarr"); strings.Contains(row, `role="alert"`) || !strings.Contains(row, "Couldn&#39;t stop") {
		t.Errorf("an old failure is still an alert, or gone:\n%s", row)
	}

	// The outcome is for whoever's still looking, not for good.
	ahead.Store(int64(6 * time.Minute))
	if row := rowOf(t, body(t, h.get(c, portal+"/")), "yarr"); strings.Contains(row, "act-note") {
		t.Errorf("an old outcome is still shown:\n%s", row)
	}
}

func TestPanicInAnActionDoesNotKillTheDaemon(t *testing.T) {
	h, _ := actionHarness(t)
	h.g.control = &panicky{}
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.post(c, portal+"/apps", portal, actForm("restart", "yarr"))
	h.opsSettled()
	if row := rowOf(t, body(t, h.get(c, portal+"/")), "yarr"); !strings.Contains(row, "stopped unexpectedly") {
		t.Errorf("row doesn't say it failed:\n%s", row)
	}
}

type panicky struct{ fakeControl }

func (*panicky) RestartService(context.Context, string) error { panic("boom") }

func TestUpdateIsOfferedOnlyWhenOneIsWaiting(t *testing.T) {
	h, f := updateHarness(t)
	h.g.control = &fakeControl{}
	t.Cleanup(h.opsSettled)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	b := body(t, h.get(c, portal+"/"))
	row := rowOf(t, b, "yarr")
	for _, want := range []string{
		`<summary class="pill"><span>Update<span class="sr"> Yarr</span> to v2.0.0…</span></summary>`,
		"Update Yarr to v2.0.0? Outpost backs it up, installs v2.0.0 and restarts it",
		`action="/updates"`,
		`name="action" value="update"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("yarr's menu lacks %q:\n%s", want, row)
		}
	}
	if strings.Contains(rowOf(t, b, "wiki"), "/updates") {
		t.Error("an app not from a release is offered an update")
	}

	// While the update runs, the row says so and the menu can't act on it.
	f.gate = make(chan struct{})
	h.post(c, portal+"/updates", portal, updateForm("yarr"))
	b = body(t, h.get(c, portal+"/"))
	if row := rowOf(t, b, "yarr"); !strings.Contains(row, "Updating…") || strings.Contains(row, "Manage") {
		t.Errorf("row during an update:\n%s", row)
	}
	if resp := h.post(c, portal+"/apps", portal, actForm("stop", "yarr")); resp.StatusCode != http.StatusConflict {
		t.Errorf("stop during an update = %d; want 409", resp.StatusCode)
	}
	close(f.gate)
	h.settled()

	f.mu.Lock()
	f.latest["yarr"] = ReleaseCheck{Latest: "v1.0.0"}
	f.mu.Unlock()
	h.post(c, portal+"/updates", portal, url.Values{"action": {"check"}})
	h.settled()
	if b := body(t, h.get(c, portal+"/")); strings.Contains(b, "available</a>") || strings.Contains(b, `value="update"`) {
		t.Errorf("the catalog offers an update with none waiting:\n%s", b)
	}
}

func TestActionsAreRecordedWithWhoAsked(t *testing.T) {
	h, f := actionHarness(t)
	feed := h.recording()
	c := h.client(true)
	h.login(c, "root", "", "/")
	feed.wait(t, 1) // the sign-in

	h.post(c, portal+"/apps", portal, actForm("stop", "yarr"))
	h.opsSettled()
	got := feed.wait(t, 2)
	if got[1].kind != EventControl || got[1].message != "root stopped yarr from 127.0.0.1" {
		t.Errorf("stop = %+v", got[1])
	}

	h.post(c, portal+"/apps", portal, actForm("backup", "yarr"))
	h.opsSettled()
	got = feed.wait(t, 4)
	if got[2].message != "root started a backup of yarr from 127.0.0.1" || got[3].message != "backup of yarr finished" {
		t.Errorf("backup = %+v, %+v", got[2], got[3])
	}

	f.mu.Lock()
	f.err = errors.New("no")
	f.mu.Unlock()
	h.post(c, portal+"/apps", portal, actForm("restart", "yarr"))
	h.opsSettled()
	if got := feed.wait(t, 5); got[4].message != "root couldn't restart yarr from 127.0.0.1: no" {
		t.Errorf("failure = %+v", got[4])
	}
}

func TestCrashedAppIsRestartedAndHaltedOneStarted(t *testing.T) {
	h, _ := actionHarness(t)
	h.withServices(append(actionServices,
		supervisor.ServiceStatus{Name: "sick", Status: "unhealthy", Desired: "running"})...)
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))
	// Start only means something once the supervisor has given up; an app
	// it's still trying to run comes back with a restart.
	for name, want := range map[string][]string{
		"atlas": {"start", "stop"},
		"sick":  {"restart", "stop"},
	} {
		row := rowOf(t, b, name)
		for _, a := range []string{"start", "stop", "restart", "backup"} {
			offered := false
			for _, w := range want {
				offered = offered || w == a
			}
			if got := offersAction(row, a); got != offered {
				t.Errorf("%s offers %s = %v; want %v", name, a, got, offered)
			}
		}
	}
	if resp := h.post(c, portal+"/apps", portal, actForm("restart", "atlas")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("restart of a halted app = %d; want 400", resp.StatusCode)
	}
}

func TestOneMenuAndOneConfirmOpenAtATime(t *testing.T) {
	h, _ := actionHarness(t)
	b := body(t, h.login(h.client(true), "root", "", "/"))
	if n := strings.Count(b, `<details class="acts" name="manage">`); n != 4 {
		t.Errorf("%d Manage menus share the exclusive name; want 4", n)
	}
	row := rowOf(t, b, "yarr")
	if n := strings.Count(row, `<details class="confirm" name="confirm-yarr">`); n != 3 {
		t.Errorf("%d of yarr's confirmations share a name; want 3:\n%s", n, row)
	}
	// Each confirmation says how to back out, which works with no script.
	if !strings.Contains(row, "Tap Stop… again to cancel.") {
		t.Errorf("the stop confirmation has no way out:\n%s", row)
	}
}

func TestCatalogPollsByScriptOnlyWhileBusy(t *testing.T) {
	h, f := actionHarness(t)
	withNothingComingBack(h)
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))
	if strings.Contains(b, pollPath) || strings.Contains(b, `http-equiv="refresh"`) {
		t.Errorf("an idle catalog reloads itself:\n%s", b)
	}

	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	b = body(t, h.post(c, portal+"/apps", portal, actForm("backup", "yarr")))
	if !strings.Contains(b, `<script src="`+pollPath+`?v=`) {
		t.Error("a busy catalog doesn't load the poll script")
	}
	// A meta refresh would close an open menu, so it's only for no script.
	if !strings.Contains(b, `<noscript><meta http-equiv="refresh" content="3"></noscript>`) ||
		strings.Count(b, `http-equiv="refresh"`) != 1 {
		t.Errorf("the meta refresh isn't confined to noscript:\n%s", b)
	}
	if strings.Contains(b, jsPath) {
		t.Error("a short list loads the filter")
	}
	close(gate)
	h.opsSettled()

	resp := h.get(h.client(false), portal+pollPath)
	if js := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") ||
		!strings.Contains(js, "details[open]") {
		t.Errorf("poll script = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// The redirect lands on the outcome, now a status note with that id.
	row := rowOf(t, body(t, h.get(c, portal+"/")), "yarr")
	if !strings.Contains(row, `<p class="act-note" id="note-yarr" tabindex="-1" role="status">Backed up by root`) {
		t.Errorf("the outcome isn't where the redirect lands:\n%s", row)
	}
}
