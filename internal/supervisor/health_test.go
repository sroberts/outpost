package supervisor

import (
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
)

// A service that takes a while to bind its port must not be restart-looped
// before it has had a chance to come up.
func TestHealth_StartupGraceBeforeFirstFailureCounts(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("slow", `start_grace_seconds = 30`)
	h.apply()
	h.tick()

	// Unhealthy from the moment it starts, as a slow-booting service is.
	h.rt.setHealthy("slow", false)

	// Well past the failure threshold in tick count, but inside the grace window.
	for i := 0; i < 10; i++ {
		h.clock.Advance(2 * time.Second)
		h.tick()
	}
	if got := h.rt.startCount("slow"); got != 1 {
		t.Fatalf("service restarted during its startup grace (starts = %d; want 1)", got)
	}
	if got := h.status("slow"); got != string(runtime.StatusStarting) {
		t.Errorf("status during grace = %q; want %q", got, runtime.StatusStarting)
	}

	// Past the grace window, failures count and it is restarted.
	h.clock.Advance(20 * time.Second)
	for i := 0; i < 4; i++ {
		h.tick()
	}
	h.clock.Advance(30 * time.Second)
	h.tick()
	if got := h.rt.startCount("slow"); got < 2 {
		t.Errorf("service not restarted after the grace window expired (starts = %d)", got)
	}
}

// Becoming healthy inside the grace window ends it immediately.
func TestHealth_GraceEndsOnFirstSuccess(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("slow", `start_grace_seconds = 60`)
	h.apply()
	h.tick()

	h.rt.setHealthy("slow", false)
	h.clock.Advance(2 * time.Second)
	h.tick()
	if got := h.status("slow"); got != string(runtime.StatusStarting) {
		t.Fatalf("status = %q; want starting", got)
	}

	h.rt.setHealthy("slow", true)
	h.tick()
	if got := h.status("slow"); got != string(runtime.StatusRunning) {
		t.Fatalf("status after first healthy check = %q; want running", got)
	}

	// Now that it has been healthy once, failures count normally.
	h.rt.setHealthy("slow", false)
	for i := 0; i < 3; i++ {
		h.tick()
	}
	h.clock.Advance(10 * time.Second)
	h.tick()
	if got := h.rt.startCount("slow"); got != 2 {
		t.Errorf("start count = %d; want 2 (failures should count once the service has been healthy)", got)
	}
}

// The default grace must be short enough not to mask a genuinely broken
// service, but non-zero so the common case works.
func TestHealth_DefaultGraceApplies(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("svc", "")
	h.apply()
	h.tick()
	h.rt.setHealthy("svc", false)

	h.clock.Advance(2 * time.Second)
	h.tick()
	h.tick()
	h.tick()
	h.tick()
	if got := h.rt.startCount("svc"); got != 1 {
		t.Errorf("restarted within the default grace window (starts = %d; want 1)", got)
	}

	h.clock.Advance(60 * time.Second)
	for i := 0; i < 4; i++ {
		h.tick()
	}
	h.clock.Advance(30 * time.Second)
	h.tick()
	if got := h.rt.startCount("svc"); got < 2 {
		t.Errorf("never restarted after the default grace expired (starts = %d)", got)
	}
}

// A crash during the grace window is still a crash: the grace covers slow
// healthchecks, not a process that exits.
func TestHealth_GraceDoesNotSuppressCrashRestart(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("svc", `start_grace_seconds = 300`)
	h.apply()
	h.tick()

	h.rt.crash("svc", 1)
	h.tick()
	h.clock.Advance(10 * time.Second)
	h.tick()

	if got := h.rt.startCount("svc"); got != 2 {
		t.Errorf("start count = %d; want 2 (a crash inside the grace window still restarts)", got)
	}
}
