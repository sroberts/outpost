package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/exposure"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// exposureFixture has one declared service, tasking, with a tailnet name,
// and the store and supervisor the page's adapter works through.
func exposureFixture(t *testing.T, opts manifest.Options) (exposureWeb, *exposure.Store, *supervisor.Supervisor) {
	t.Helper()
	home := t.TempDir()
	a := &app{home: home}
	if err := os.MkdirAll(a.servicesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name = \"tasking\"\nruntime = \"process\"\nbinary = \"/usr/bin/true\"\nport = 19411\n" +
		"storage = \"" + filepath.Join(home, "data", "tasking") + "\"\ntailscale_name = \"tasking\"\n"
	if err := os.WriteFile(filepath.Join(a.servicesDir(), "tasking.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	store, err := exposure.Open(a.exposurePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir: a.servicesDir(), Store: st, ManifestOptions: opts, Exposure: store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return exposureWeb{a: a, store: store, sup: sup, opts: opts}, store, sup
}

func taskingStatus(t *testing.T, sup *supervisor.Supervisor) supervisor.ServiceStatus {
	t.Helper()
	st, err := sup.Status(context.Background(), "tasking")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A setting from the page is checked by the manifest's rules, stored, and in
// force when Set returns; Reset puts the manifest's back.
func TestExposureWeb_SetAppliesAndResetReverts(t *testing.T) {
	e, store, sup := exposureFixture(t, manifest.Options{RequireTailscale: true, Gateway: true})
	ctx := context.Background()
	if err := e.Set(ctx, "tasking", true, []string{"/webhooks/github"}, "root"); err != nil {
		t.Fatal(err)
	}
	if st := taskingStatus(t, sup); !st.Funnel || !st.FromPortal || st.PublicPaths[0] != "/webhooks/github" {
		t.Errorf("after Set: %+v", st)
	}
	if err := e.Reset(ctx, "tasking", "root"); err != nil {
		t.Fatal(err)
	}
	if st := taskingStatus(t, sup); st.Funnel || st.FromPortal || len(st.PublicPaths) != 0 {
		t.Errorf("after Reset: %+v", st)
	}
	if _, ok := store.Get("tasking"); ok {
		t.Error("the setting outlived Reset")
	}
}

func TestExposureWeb_RefusesWhatTheManifestWouldNotTake(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		opts   manifest.Options
		funnel bool
		paths  []string
		want   string
	}{
		"funnel with no paths":    {manifest.Options{RequireTailscale: true, Gateway: true}, true, nil, "there are none"},
		"funnel with no gateway":  {manifest.Options{RequireTailscale: true}, true, []string{"/hook"}, "needs the gateway"},
		"a path the rules refuse": {manifest.Options{RequireTailscale: true, Gateway: true}, false, []string{"/*"}, "/*"},
	} {
		e, store, _ := exposureFixture(t, tc.opts)
		err := e.Set(ctx, "tasking", tc.funnel, tc.paths, "root")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, ok := store.Get("tasking"); ok {
			t.Errorf("%s: a refused setting was stored", name)
		}
	}
	e, _, _ := exposureFixture(t, manifest.Options{RequireTailscale: true, Gateway: true})
	if err := e.Set(ctx, "nobody", false, nil, "root"); err == nil {
		t.Error("set for a service with no manifest")
	}
}
