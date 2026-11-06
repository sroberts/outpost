package supervisor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// fakeExposure is the portal's settings, by service.
type fakeExposure struct {
	mu sync.Mutex
	m  map[string]manifestExposure
}

func (f *fakeExposure) Override(service string) (bool, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[service]
	return e.funnel, e.publicPaths, ok
}

func (f *fakeExposure) set(service string, funnel bool, paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]manifestExposure{}
	}
	f.m[service] = manifestExposure{funnel: funnel, publicPaths: paths}
}

func exposureStatus(t *testing.T, h *harness, name string) ServiceStatus {
	t.Helper()
	list, err := h.sup.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range list {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("%s isn't listed", name)
	return ServiceStatus{}
}

// The portal's setting wins over the manifest's, and status says both.
func TestExposure_PortalWinsOverTheManifest(t *testing.T) {
	h := newHarness(t)
	ex := &fakeExposure{}
	h.sup.exposure = ex
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true, Gateway: true}
	h.writeManifest("alpha", `public_paths = ["/old"]`)
	h.writeManifest("beta", `public_paths = ["/hook"]`)
	ex.set("alpha", true, "/webhooks/github")
	h.apply()

	st := exposureStatus(t, h, "alpha")
	if !st.Funnel || strings.Join(st.PublicPaths, ",") != "/webhooks/github" || !st.FromPortal ||
		st.ManifestFunnel || strings.Join(st.ManifestPublicPaths, ",") != "/old" {
		t.Errorf("alpha = funnel %v paths %v from portal %v (manifest %v %v)",
			st.Funnel, st.PublicPaths, st.FromPortal, st.ManifestFunnel, st.ManifestPublicPaths)
	}
	if got := h.sup.desired["alpha"]; !got.Funnel || got.PublicPaths[0] != "/webhooks/github" {
		t.Errorf("what alpha runs with: %+v", got)
	}
	if st := exposureStatus(t, h, "beta"); st.FromPortal || st.Funnel || st.PublicPaths[0] != "/hook" {
		t.Errorf("beta, which the portal didn't set: %+v", st)
	}

	// Forgetting the setting puts the manifest's back on the next apply.
	ex.mu.Lock()
	delete(ex.m, "alpha")
	ex.mu.Unlock()
	h.apply()
	if st := exposureStatus(t, h, "alpha"); st.FromPortal || st.Funnel || st.PublicPaths[0] != "/old" {
		t.Errorf("after a reset: %+v", st)
	}
}

// A setting that no longer passes the manifest's rules, such as Funnel once
// the gateway is off, is left out, and the rest still apply.
func TestExposure_ASettingThatNoLongerAppliesIsLeftOut(t *testing.T) {
	h := newHarness(t)
	ex := &fakeExposure{}
	h.sup.exposure = ex
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true}
	var logs []string
	h.sup.logf = func(format string, args ...any) { logs = append(logs, format) }
	h.writeManifest("alpha", `public_paths = ["/old"]`)
	h.writeManifest("beta", "")
	ex.set("alpha", true, "/webhooks/github")
	h.apply()

	if st := exposureStatus(t, h, "alpha"); st.Funnel || st.FromPortal || st.PublicPaths[0] != "/old" {
		t.Errorf("alpha took a setting the rules refuse: %+v", st)
	}
	if _, ok := h.sup.desired["beta"]; !ok {
		t.Error("beta wasn't applied")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "doesn't apply") {
		t.Errorf("not logged: %v", logs)
	}
}
