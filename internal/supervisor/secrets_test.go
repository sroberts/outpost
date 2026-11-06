package supervisor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
)

// fakeSecrets hands out a fixed set of variables per service, with a
// revision bumped by every change, as secrets.Store does.
type fakeSecrets struct {
	mu   sync.Mutex
	vars map[string]map[string]string
	revs map[string]int
}

func (f *fakeSecrets) Resolve(service string) (map[string]string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vars[service]
	if len(v) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out, fmt.Sprintf("r%d", f.revs[service])
}

func (f *fakeSecrets) set(service, k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vars == nil {
		f.vars, f.revs = map[string]map[string]string{}, map[string]int{}
	}
	if f.vars[service] == nil {
		f.vars[service] = map[string]string{}
	}
	f.vars[service][k] = v
	f.revs[service]++
}

// envRuntime records the env each service was last started with.
type envRuntime struct {
	*fakeRuntime
	mu   sync.Mutex
	envs map[string]map[string]string
}

func (r *envRuntime) Start(ctx context.Context, svc *manifest.Service) (runtime.Handle, error) {
	r.mu.Lock()
	if r.envs == nil {
		r.envs = map[string]map[string]string{}
	}
	r.envs[svc.Name] = svc.Env
	r.mu.Unlock()
	return r.fakeRuntime.Start(ctx, svc)
}

func (r *envRuntime) env(name string) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.envs[name]
}

func secretsHarness(t *testing.T) (*harness, *fakeSecrets, *envRuntime) {
	h := newHarness(t)
	sec := &fakeSecrets{}
	rt := &envRuntime{fakeRuntime: h.rt}
	h.sup.secrets = sec
	h.sup.procRT = rt
	return h, sec, rt
}

func TestSecrets_GivenAtStartOverManifestEnv(t *testing.T) {
	h, sec, rt := secretsHarness(t)
	sec.set("alpha", "API_KEY", "from-secret")
	h.writeManifest("alpha", `env = { API_KEY = "from-manifest", MODE = "x" }`)
	h.apply()
	h.tick()

	env := rt.env("alpha")
	if env["API_KEY"] != "from-secret" || env["MODE"] != "x" {
		t.Errorf("started with %v; want the secret's API_KEY and the manifest's MODE", env)
	}
	// The applied manifest is untouched, so it alone decides a manifest change.
	if got := h.sup.desired["alpha"].Env["API_KEY"]; got != "from-manifest" {
		t.Errorf("desired manifest's API_KEY = %q; the secret leaked into it", got)
	}
}

func TestSecrets_RotationRestartsOnlyTheServiceThatHasIt(t *testing.T) {
	h, sec, rt := secretsHarness(t)
	sec.set("alpha", "API_KEY", "one")
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	h.tick()

	// Steady: nothing restarts while the secret stays the same.
	for i := 0; i < 3; i++ {
		h.clock.Advance(5 * time.Second)
		h.tick()
	}
	if h.rt.startCount("alpha") != 1 {
		t.Fatalf("alpha started %d times with no change", h.rt.startCount("alpha"))
	}

	sec.set("alpha", "API_KEY", "two")
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("alpha start count = %d; want 2 after a rotation", got)
	}
	if got := rt.env("alpha")["API_KEY"]; got != "two" {
		t.Errorf("alpha restarted with API_KEY = %q; want the new value", got)
	}
	if got := h.rt.startCount("beta"); got != 1 {
		t.Errorf("beta start count = %d; a rotation of alpha's secret restarted it", got)
	}
}

func TestSecrets_BindingToARunningServiceRestartsIt(t *testing.T) {
	h, sec, rt := secretsHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	if _, has := rt.env("alpha")["API_KEY"]; has {
		t.Fatal("started with a secret nobody bound")
	}

	sec.set("alpha", "API_KEY", "new")
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()
	if got := rt.env("alpha")["API_KEY"]; got != "new" || h.rt.startCount("alpha") != 2 {
		t.Errorf("after binding: API_KEY = %q, starts = %d", got, h.rt.startCount("alpha"))
	}
}

func TestSecrets_ChangeLeavesAStoppedServiceStopped(t *testing.T) {
	h, sec, _ := secretsHarness(t)
	sec.set("alpha", "API_KEY", "one")
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	if err := h.sup.StopService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	h.tick()

	sec.set("alpha", "API_KEY", "two")
	for i := 0; i < 3; i++ {
		h.clock.Advance(10 * time.Second)
		h.tick()
	}
	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; rotating a stopped service's secret started it", got)
	}
}
