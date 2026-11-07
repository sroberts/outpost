package gateway

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeExposure records what the page asked for, and refuses what has no
// public path with funnel on, as the manifest's rules do.
type fakeExposure struct {
	mu     sync.Mutex
	set    []string
	resets []string
}

func (f *fakeExposure) Set(_ context.Context, service string, funnel bool, paths []string, by string) error {
	if funnel && len(paths) == 0 {
		return errors.New("funnel = true answers only public_paths from the internet, and there are none")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = append(f.set, service+" "+strings.Join(paths, ",")+" "+map[bool]string{true: "on", false: "off"}[funnel]+" by "+by)
	return nil
}

func (f *fakeExposure) Reset(_ context.Context, service, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets = append(f.resets, service+" by "+by)
	return nil
}

func exposureHarness(t *testing.T) (*harness, *fakeExposure) {
	h := newHarness(t)
	f := &fakeExposure{}
	h.g.exposure = f
	h.g.tailnetHosts = func() map[string]string { return map[string]string{"tasking": "tasking.tail.ts.net"} }
	h.withServices(
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/webhooks/github"}, FromPortal: true, ManifestPublicPaths: []string{"/old"},
			Tailnet: &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", TailscaleName: "yarr",
			Tailnet: &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelUnavailable}},
		supervisor.ServiceStatus{Name: "notes", Status: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

// What the internet reaches of an app is under that app's Manage menu, and
// the catalog's badge for a public app opens it there.
func TestAppSettings_ShowWhatTheInternetReaches(t *testing.T) {
	h, _ := exposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.get(c, portal+"/"))
	if !strings.Contains(b, `href="#set-public-tasking"`) || !strings.Contains(b, `id="set-public-tasking"`) {
		t.Errorf("the catalog doesn't badge tasking, or give it an internet access panel:\n%s", b)
	}
	tasking := b[strings.Index(b, `id="set-public-tasking"`):]
	tasking = tasking[:strings.Index(tasking, "</details>")]
	for _, want := range []string{
		"https://tasking.tail.ts.net/webhooks/github",
		"On: the internet reaches these paths",
		"Set here, over the manifest, which has Funnel off and public paths /old.",
		"Use the manifest's setting for Tasking",
	} {
		if !strings.Contains(tasking, want) {
			t.Errorf("tasking's panel lacks %q:\n%s", want, tasking)
		}
	}
	if notes := b[strings.Index(b, `id="set-public-notes"`):]; !strings.Contains(notes, "Funnel needs the app on the tailnet") {
		t.Error("an app with no node isn't told what Funnel needs")
	}
}

func TestAppSettings_SavesAndResetsWhatTheInternetReaches(t *testing.T) {
	h, f := exposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.post(c, portal+"/settings", portal, url.Values{
		"action": {"public-save"}, "app": {"yarr"}, "paths": {"/hooks/*\n\n/hooks/*\n /stripe \n"}, "funnel": {"1"},
	}))
	if !strings.Contains(b, "Saved.") || !strings.Contains(b, `id="set-public-yarr" open`) {
		t.Errorf("after saving, the catalog isn't open on yarr's panel with the outcome:\n%s", b)
	}
	if strings.Join(f.set, "|") != "yarr /hooks/*,/stripe on by root" {
		t.Errorf("set = %v", f.set)
	}

	resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"public-save"}, "app": {"yarr"}, "paths": {""}, "funnel": {"1"}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "there are none") || !strings.Contains(b, `id="set-public-yarr" open`) {
		t.Errorf("a refused setting: %d\n%s", resp.StatusCode, b)
	}

	if b := body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"public-reset"}, "app": {"tasking"}})); !strings.Contains(b, "Back to what the manifest says.") {
		t.Errorf("after resetting:\n%s", b)
	}
	if strings.Join(f.resets, "|") != "tasking by root" {
		t.Errorf("resets = %v", f.resets)
	}
}

func TestAppSettings_InternetAccessOnlyForAnAdmin(t *testing.T) {
	h, f := exposureHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, "set-public-") || strings.Contains(b, `action="/settings"`) {
		t.Error("a plain user is shown an app's internet access, or the badge")
	}
	save := url.Values{"action": {"public-save"}, "app": {"yarr"}, "paths": {"/x"}, "funnel": {"1"}}
	if resp := h.post(plain, portal+"/settings", portal, save); resp.StatusCode != 403 {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/settings", "http://evil.test", save); resp.StatusCode != 403 {
		t.Errorf("POST from another site = %d", resp.StatusCode)
	}
	if len(f.set) != 0 {
		t.Errorf("a refused request was saved: %v", f.set)
	}
}

func TestNoInternetAccessWithoutAStore(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running"})
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/")); strings.Contains(b, "set-public-") {
		t.Error("an app offers internet access with nothing to set it")
	}
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"public-save"}, "app": {"yarr"}}); resp.StatusCode != 400 {
		t.Errorf("a save with nothing to set it = %d", resp.StatusCode)
	}
}
