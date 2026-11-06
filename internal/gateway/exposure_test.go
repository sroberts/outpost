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

func TestPublicPage_ShowsWhatTheInternetReaches(t *testing.T) {
	h, _ := exposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.get(c, portal+"/"))
	if !strings.Contains(b, `href="/public"`) || !strings.Contains(b, `href="/public#p-tasking"`) {
		t.Errorf("the catalog doesn't link an admin to internet access, or badge tasking:\n%s", b)
	}
	b = body(t, h.get(c, portal+"/public"))
	for _, want := range []string{
		"https://tasking.tail.ts.net/webhooks/github",
		"On: the internet reaches these paths",
		"Set here, over the manifest, which has Funnel off and public paths /old.",
		"Use the manifest's setting for Tasking",
		"Funnel needs the app on the tailnet",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q:\n%s", want, b)
		}
	}
}

func TestPublicPage_SavesAndResets(t *testing.T) {
	h, f := exposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.post(c, portal+"/public", portal, url.Values{
		"action": {"save"}, "app": {"yarr"}, "paths": {"/hooks/*\n\n/hooks/*\n /stripe \n"}, "funnel": {"1"},
	}))
	if !strings.Contains(b, "Saved.") {
		t.Errorf("after saving:\n%s", b)
	}
	if strings.Join(f.set, "|") != "yarr /hooks/*,/stripe on by root" {
		t.Errorf("set = %v", f.set)
	}

	resp := h.post(c, portal+"/public", portal, url.Values{"action": {"save"}, "app": {"yarr"}, "funnel": {"1"}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "there are none") {
		t.Errorf("a refused setting: %d\n%s", resp.StatusCode, b)
	}

	if b := body(t, h.post(c, portal+"/public", portal, url.Values{"action": {"reset"}, "app": {"tasking"}})); !strings.Contains(b, "Back to what the manifest says.") {
		t.Errorf("after resetting:\n%s", b)
	}
	if strings.Join(f.resets, "|") != "tasking by root" {
		t.Errorf("resets = %v", f.resets)
	}
}

func TestPublicPage_OnlyAnAdmin(t *testing.T) {
	h, f := exposureHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, `href="/public`) {
		t.Error("a plain user is linked to internet access, or shown the badge")
	}
	if resp := h.get(plain, portal+"/public"); resp.StatusCode != 403 {
		t.Errorf("GET as a plain user = %d", resp.StatusCode)
	}
	save := url.Values{"action": {"save"}, "app": {"yarr"}, "paths": {"/x"}, "funnel": {"1"}}
	if resp := h.post(plain, portal+"/public", portal, save); resp.StatusCode != 403 {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/public", "http://evil.test", save); resp.StatusCode != 403 {
		t.Errorf("POST from another site = %d", resp.StatusCode)
	}
	if len(f.set) != 0 {
		t.Errorf("a refused request was saved: %v", f.set)
	}
}

func TestNoPublicPageWithoutAStore(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	if resp := h.get(c, portal+"/public"); resp.StatusCode != 404 {
		t.Errorf("GET /public with no store = %d", resp.StatusCode)
	}
}
