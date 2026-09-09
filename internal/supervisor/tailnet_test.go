package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// fakeTailnet records attach/detach calls without touching Tailscale.
type fakeTailnet struct {
	mu        sync.Mutex
	attached  map[string]int
	detached  map[string]int
	live      map[string]bool
	attachErr error
}

func newFakeTailnet() *fakeTailnet {
	return &fakeTailnet{attached: map[string]int{}, detached: map[string]int{}, live: map[string]bool{}}
}

func (f *fakeTailnet) Attach(ctx context.Context, svc *manifest.Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached[svc.Name]++
	f.live[svc.Name] = true
	return nil
}

func (f *fakeTailnet) Detach(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached[name]++
	f.live[name] = false
	return nil
}

func (f *fakeTailnet) counts(name string) (int, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached[name], f.detached[name], f.live[name]
}

// tailnetHarness is a harness whose supervisor has a tailnet router attached.
func tailnetHarness(t *testing.T) (*harness, *fakeTailnet) {
	t.Helper()
	h := newHarness(t)
	tn := newFakeTailnet()
	h.sup.tailnet = tn
	return h, tn
}

func TestTailnet_AttachedWhenServiceStarts(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	attached, _, live := tn.counts("alpha")
	if attached != 1 {
		t.Errorf("attach count = %d; want 1", attached)
	}
	if !live {
		t.Error("service is not on the tailnet after starting")
	}
}

func TestTailnet_DetachedWhenServiceStops(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	h.store.SetDesired("alpha", "stopped")
	h.tick()

	_, detached, live := tn.counts("alpha")
	if detached != 1 {
		t.Errorf("detach count = %d; want 1", detached)
	}
	if live {
		t.Error("service still on the tailnet after being stopped")
	}
}

// A crashed service must come off the tailnet, so callers get a clean failure
// instead of a proxy to nothing, and rejoin when it is restarted.
func TestTailnet_DetachedOnCrashAndReattachedOnRestart(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", `backoff_seconds = 5`)
	h.apply()
	h.tick()

	h.rt.crash("alpha", 1)
	h.tick()
	if _, detached, live := tn.counts("alpha"); detached != 1 || live {
		t.Errorf("after crash: detached=%d live=%v; want 1 and false", detached, live)
	}

	h.clock.Advance(6 * time.Second)
	h.tick()
	if attached, _, live := tn.counts("alpha"); attached != 2 || !live {
		t.Errorf("after restart: attached=%d live=%v; want 2 and true", attached, live)
	}
}

func TestTailnet_DetachedWhenRemovedFromManifest(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	h.removeManifest("alpha")
	h.apply()
	h.tick()

	if _, detached, live := tn.counts("alpha"); detached == 0 || live {
		t.Errorf("after removal: detached=%d live=%v; want >0 and false", detached, live)
	}
}

// Losing tailnet exposure is not a reason to take a working service down.
func TestTailnet_AttachFailureLeavesServiceRunning(t *testing.T) {
	h, tn := tailnetHarness(t)
	tn.attachErr = errors.New("tailscale is unreachable")
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Fatalf("start count = %d; want 1", got)
	}
	if got := h.status("alpha"); got != "running" {
		t.Errorf("status = %q; want running despite the tailnet failure", got)
	}

	// The failure should still be visible in the event log.
	events, err := h.store.Events("alpha", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "error" && contains(e.Message, "tailnet") {
			found = true
		}
	}
	if !found {
		t.Error("tailnet attach failure was not recorded as an event")
	}
}

func TestTailnet_ShutdownDetachesEverything(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	h.tick()

	if err := h.sup.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, detached, live := tn.counts(name); detached == 0 || live {
			t.Errorf("%s: detached=%d live=%v after shutdown", name, detached, live)
		}
	}
}

// A supervisor with no tailnet configured must behave exactly as before.
func TestTailnet_OptionalWhenNotConfigured(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	if got := h.status("alpha"); got != "running" {
		t.Errorf("status = %q; want running with no tailnet configured", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
