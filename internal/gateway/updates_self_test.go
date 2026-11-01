package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeSelf stands in for the daemon's updater of Outpost itself.
type fakeSelf struct {
	mu       sync.Mutex
	latest   ReleaseCheck
	refusal  string
	startErr error
	starts   []string
	status   SelfStatus
	has      bool
}

func (f *fakeSelf) Latest(context.Context) (ReleaseCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latest, nil
}

func (f *fakeSelf) Refusal() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusal
}

// Start writes a status as the daemon's does, so a second press sees it.
func (f *fakeSelf) Start(by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, by)
	if f.startErr != nil {
		return f.startErr
	}
	f.status, f.has = SelfStatus{Phase: "starting", By: by}, true
	return nil
}

func (f *fakeSelf) Status() (SelfStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.status
	f.status.Announce = false
	return st, f.has
}

func (f *fakeSelf) set(st SelfStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.has = st, true
}

func (f *fakeSelf) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.starts...)
}

// selfHarness is updateHarness with Outpost able to update itself to v9.0.0.
func selfHarness(t *testing.T) (*harness, *fakeSelf) {
	t.Helper()
	h, _ := updateHarness(t)
	f := &fakeSelf{latest: ReleaseCheck{Latest: "v9.0.0", Newer: true}}
	h.g.selfUpdater = f
	return h, f
}

var selfForm = url.Values{"action": {"self"}}

func TestSelfUpdateIsOfferedAndCounted(t *testing.T) {
	h, _ := selfHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, "2 updates available") {
		t.Errorf("catalog doesn't count Outpost:\n%s", b)
	}
	b := body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{"Outpost", "v9.0.0 is available", `name="action" value="self"`, "Updating Outpost itself restarts every app."} {
		if !strings.Contains(b, want) {
			t.Errorf("update page lacks %q:\n%s", want, b)
		}
	}
}

func TestSelfUpdateStartsOnceOnADoubleClick(t *testing.T) {
	h, f := selfHarness(t)
	feed := h.recording()
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	h.post(c, portal+"/updates", portal, selfForm)
	resp := h.post(c, portal+"/updates", portal, selfForm)
	if got := f.started(); len(got) != 1 || got[0] != "root" {
		t.Errorf("Start calls = %v; want one, by root", got)
	}
	if resp.Request.URL.Path != "/updates" {
		t.Errorf("second press landed on %s", resp.Request.URL)
	}
	got := feed.wait(t, 2)
	if got[1].kind != EventUpdate || !strings.Contains(got[1].message, "root started an update of Outpost") {
		t.Errorf("start = %+v", got[1])
	}
}

func TestSelfUpdateIsAdminOnly(t *testing.T) {
	h, f := selfHarness(t)

	anon := h.client(false)
	if resp := h.post(anon, portal+"/updates", portal, selfForm); resp.StatusCode != http.StatusSeeOther ||
		!strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("anonymous POST = %d to %q; want a redirect to sign in", resp.StatusCode, resp.Header.Get("Location"))
	}

	alice := h.client(true)
	h.login(alice, "alice", "", "/")
	if resp := h.post(alice, portal+"/updates", portal, selfForm); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin POST = %d; want 403", resp.StatusCode)
	}

	root := h.client(true)
	h.login(root, "root", "", "/")
	if resp := h.post(root, portal+"/updates", "https://evil.example", selfForm); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d; want 403", resp.StatusCode)
	}
	h.settled()
	if got := f.started(); len(got) != 0 {
		t.Errorf("Start was called by %v", got)
	}
}

func TestSelfUpdateStatusIsJSONAndNeverRedirects(t *testing.T) {
	h, f := selfHarness(t)
	f.set(SelfStatus{Phase: "downloading"})

	resp := h.get(h.client(false), portal+"/updates/self")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Location") != "" {
		t.Errorf("anonymous = %d, Location %q; want 401 and no redirect", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("the status allows other origins")
	}

	alice := h.client(false)
	h.login(alice, "alice", "", "/")
	if resp := h.get(alice, portal+"/updates/self"); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("non-admin = %d, Location %q; want 403 and no redirect", resp.StatusCode, resp.Header.Get("Location"))
	}

	root := h.client(false)
	h.login(root, "root", "", "/")
	resp = h.get(root, portal+"/updates/self")
	var got struct {
		Phase string `json:"phase"`
		Final bool   `json:"final"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("admin = %d, %v", resp.StatusCode, err)
	}
	if got.Phase != "downloading" || got.Final {
		t.Errorf("status = %+v", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	f.set(SelfStatus{Phase: "done", Final: true})
	if err := json.NewDecoder(h.get(root, portal+"/updates/self").Body).Decode(&got); err != nil || !got.Final {
		t.Errorf("after: %+v %v", got, err)
	}
}

func TestSelfUpdateRunningWaitsItOutByScript(t *testing.T) {
	h, f := selfHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	f.set(SelfStatus{Phase: "restarting", To: "v9.0.0", Log: "/home/o/.outpost/logs/self-update.log"})

	b := body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{selfJSPath, `<a href="/updates">reload</a>`, "Restarting Outpost on v9.0.0.", `id="self-phase"`, "Every app stops",
		`<noscript><meta http-equiv="refresh"`, "self-update.log"} {
		if !strings.Contains(b, want) {
			t.Errorf("running page lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, `<meta http-equiv="refresh" content="3">`) {
		t.Error("a meta refresh would stop at the browser's error page while Outpost is down")
	}
	if strings.Contains(b, `value="self"`) {
		t.Error("the page offers the update that's running")
	}
	if !strings.Contains(b, " disabled>") {
		t.Errorf("an app's update button isn't disabled:\n%s", b)
	}
	// The app's update is still waiting; Outpost's is under way.
	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, "1 update available") {
		t.Errorf("catalog count while Outpost updates:\n%s", b)
	}

	resp := h.post(c, portal+"/updates", portal, updateForm("yarr"))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("an app update during Outpost's = %d; want 409", resp.StatusCode)
	}

	if resp := h.get(c, portal+selfJSPath); resp.StatusCode != http.StatusOK ||
		!strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Errorf("script = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestSelfUpdateRefusalIsShownInWords(t *testing.T) {
	h, f := selfHarness(t)
	f.refusal = "the daemon wasn't started by its autostart job"
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	b := body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "can't be installed from here: the daemon wasn&#39;t started by its autostart job") {
		t.Errorf("page doesn't give the refusal:\n%s", b)
	}
	if strings.Contains(b, `value="self"`) {
		t.Error("the page offers an update it would refuse")
	}
	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, "1 update available") {
		t.Errorf("catalog counts an update that can't be made from here:\n%s", b)
	}
}

func TestSelfUpdateStartRefusedIsShown(t *testing.T) {
	h, f := selfHarness(t)
	f.startErr = errors.New("an update is running (yarr)")
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	resp := h.post(c, portal+"/updates", portal, selfForm)
	if b := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(b, "an update is running (yarr)") {
		t.Errorf("refused start = %d:\n%s", resp.StatusCode, b)
	}
}

func TestSelfUpdateOutcomeIsRecordedOnce(t *testing.T) {
	h, f := selfHarness(t)
	feed := h.recording()
	f.set(SelfStatus{Phase: "done", Final: true, To: "v9.0.0", By: "root", Output: "Outpost is running v9.0.0", Announce: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	b := body(t, h.get(c, portal+"/updates"))
	if !strings.Contains(b, "Outpost is running v9.0.0") {
		t.Errorf("page doesn't show the outcome:\n%s", b)
	}
	body(t, h.get(c, portal+"/updates"))
	outcomes := 0
	for _, e := range feed.wait(t, 2) {
		if strings.Contains(e.message, "update of Outpost to v9.0.0, which root started, finished") {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Errorf("the outcome was recorded %d times; want once", outcomes)
	}
}

// A daemon that can't update itself gives the page it always did.
func TestNoSelfUpdatesLeavesThePageAsItWas(t *testing.T) {
	h, _ := updateHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	b := body(t, h.get(c, portal+"/updates"))
	for _, unwanted := range []string{"Outpost itself", "sroberts/outpost", `value="self"`, selfJSPath} {
		if strings.Contains(b, unwanted) {
			t.Errorf("page has %q:\n%s", unwanted, b)
		}
	}
	if resp := h.get(c, portal+"/updates/self"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /updates/self = %d; want 404", resp.StatusCode)
	}
	if resp := h.post(c, portal+"/updates", portal, selfForm); resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST action=self = %d; want 404", resp.StatusCode)
	}
	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, "1 update available") {
		t.Errorf("catalog count changed:\n%s", b)
	}
}
