package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/state"
)

// startStray launches a real process standing in for a service orphaned by an
// unclean daemon shutdown, and returns its PID.
func startStray(t *testing.T, marker string) (*exec.Cmd, int) {
	t.Helper()
	// A distinctive argv so reclaim can recognise it as ours.
	cmd := exec.Command("/bin/sleep", "300")
	cmd.Args = []string{marker, "300"}
	cmd.Path = "/bin/sleep"
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap it as soon as it dies. In production an orphan is not our child and
	// init reaps it; here it is, so without this it lingers as a zombie and
	// still answers signal 0.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd, cmd.Process.Pid
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// After a SIGKILLed daemon, services survive as orphans holding their ports.
// A restarted daemon must clear them before starting replacements.
func TestReclaimOrphans_KillsSurvivorsFromAPreviousRun(t *testing.T) {
	h := newHarness(t)
	h.writeManifestBinary("alpha", "/bin/sleep")
	h.apply()

	_, pid := startStray(t, "/bin/sleep")
	// The state store is what a restarted daemon reads to find them.
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running",
		InstanceID: strconv.Itoa(pid),
	}); err != nil {
		t.Fatal(err)
	}

	reclaimed, err := h.sup.ReclaimOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != "alpha" {
		t.Errorf("reclaimed = %v; want [alpha]", reclaimed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && alive(pid) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Error("orphaned process still running after reclaim")
	}
}

// PIDs are recycled. Killing a stranger because it inherited a recorded PID
// would be far worse than leaving an orphan, so the command must match too.
func TestReclaimOrphans_LeavesUnrelatedProcessesAlone(t *testing.T) {
	h := newHarness(t)
	// The manifest names a binary the stray process is not running.
	h.writeManifestBinary("alpha", "/usr/bin/true")
	h.apply()

	_, pid := startStray(t, "/bin/sleep")
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running",
		InstanceID: strconv.Itoa(pid),
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}

	reclaimed, err := h.sup.ReclaimOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed = %v; want none — the PID belongs to an unrelated process", reclaimed)
	}
	if !alive(pid) {
		t.Fatal("an unrelated process was killed")
	}
}

func TestReclaimOrphans_IgnoresDeadAndEmptyRecords(t *testing.T) {
	h := newHarness(t)
	h.writeManifestBinary("alpha", "/bin/sleep")
	h.writeManifestBinary("beta", "/bin/sleep")
	h.apply()

	// A PID that is not running, and a record with no instance at all.
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running", InstanceID: "999999",
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "beta", Runtime: "process", Status: "stopped", InstanceID: "",
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}

	reclaimed, err := h.sup.ReclaimOrphans(context.Background())
	if err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Errorf("reclaimed = %v; want none", reclaimed)
	}
}

// Reclaiming must clear the stale instance id, so status does not keep
// reporting a PID that is gone.
func TestReclaimOrphans_ClearsStaleInstanceID(t *testing.T) {
	h := newHarness(t)
	h.writeManifestBinary("alpha", "/bin/sleep")
	h.apply()

	_, pid := startStray(t, "/bin/sleep")
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running",
		InstanceID: strconv.Itoa(pid),
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}

	if _, err := h.sup.ReclaimOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := h.store.GetService("alpha")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if rec.InstanceID != "" {
		t.Errorf("InstanceID = %q after reclaim; want it cleared", rec.InstanceID)
	}
}

// The whole point: after reclaiming, the port is free and the service starts.
func TestReclaimOrphans_FreesThePortForARestart(t *testing.T) {
	h := newHarness(t)
	h.writeManifestBinary("alpha", "/bin/sleep")
	h.apply()

	_, pid := startStray(t, "/bin/sleep")
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running",
		InstanceID: strconv.Itoa(pid),
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}

	if _, err := h.sup.ReclaimOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.tick()

	if got := h.rt.startCount("alpha"); got != 1 {
		t.Errorf("start count = %d; want 1 after reclaim", got)
	}
	if got := h.status("alpha"); got != "running" {
		t.Errorf("status = %q; want running", got)
	}
}

// An event should record what happened, so it is visible in `outpost status`.
func TestReclaimOrphans_RecordsAnEvent(t *testing.T) {
	h := newHarness(t)
	h.writeManifestBinary("alpha", "/bin/sleep")
	h.apply()

	_, pid := startStray(t, "/bin/sleep")
	if err := h.store.UpsertService(state.ServiceRecord{
		Name: "alpha", Runtime: "process", Status: "running",
		InstanceID: strconv.Itoa(pid),
	}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	if _, err := h.sup.ReclaimOrphans(context.Background()); err != nil {
		t.Fatalf("ReclaimOrphans: %v", err)
	}

	events, err := h.store.Events("alpha", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if contains(e.Message, "reclaim") || contains(e.Message, "previous") {
			found = true
		}
	}
	if !found {
		t.Errorf("no event describing the reclaim; got %v", events)
	}
}

func TestProcessCommandLine(t *testing.T) {
	// Our own process must be identifiable.
	cmdline, err := processCommandLine(os.Getpid())
	if err != nil {
		t.Fatalf("processCommandLine: %v", err)
	}
	if cmdline == "" {
		t.Fatal("empty command line for the running test process")
	}

	if _, err := processCommandLine(999999); err == nil {
		t.Error("processCommandLine for a dead PID: want error, got nil")
	}
}

// writeManifestBinary writes a manifest naming a specific binary.
func (h *harness) writeManifestBinary(name, binary string) {
	h.t.Helper()
	body := fmt.Sprintf(`
name    = %q
runtime = "process"
binary  = %q
port    = %d
storage = %q
tailscale_name = %q
`, name, binary, 9500+len(name), filepath.Join(h.dir, "data", name), name)
	if err := os.WriteFile(filepath.Join(h.dir, "services", name+".toml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}
