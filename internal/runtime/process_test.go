package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

var (
	buildOnce sync.Once
	fakeBin   string
	buildErr  error
)

// fakeservice builds the testdata helper binary once per package run.
func fakeservice(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "outpost-fakeservice")
		if err != nil {
			buildErr = err
			return
		}
		fakeBin = filepath.Join(dir, "fakeservice")
		cmd := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakeservice")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build fakeservice: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return fakeBin
}

// freePort returns a port that was free at the time of the call.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testRuntime(t *testing.T) *ProcessRuntime {
	t.Helper()
	return &ProcessRuntime{LogDir: t.TempDir()}
}

func testService(t *testing.T, extraArgs ...string) *manifest.Service {
	t.Helper()
	port := freePort(t)
	return &manifest.Service{
		Name:           "fake",
		Runtime:        "process",
		Binary:         fakeservice(t),
		Args:           append([]string{"-port", strconv.Itoa(port)}, extraArgs...),
		Port:           port,
		Storage:        t.TempDir(),
		TailscaleName:  "fake",
		Healthcheck:    "/up",
		RestartPolicy:  "on-failure",
		BackoffSeconds: 1,
	}
}

// waitHealthy polls until the runtime reports the handle healthy.
func waitHealthy(t *testing.T, r Runtime, h Handle, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		ok, _ := r.Healthy(context.Background(), h)
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("service never became healthy within %s", within)
}

func TestProcessRuntime_StartHealthyStop(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.PID() <= 0 {
		t.Errorf("PID = %d; want > 0", h.PID())
	}
	waitHealthy(t, r, h, 5*time.Second)

	if got := h.Status(); got != StatusRunning {
		t.Errorf("Status = %q; want %q", got, StatusRunning)
	}

	if err := r.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.Status(); got != StatusStopped {
		t.Errorf("Status after Stop = %q; want %q", got, StatusStopped)
	}

	// A stopped process must no longer answer its healthcheck.
	if ok, _ := r.Healthy(context.Background(), h); ok {
		t.Error("Healthy = true after Stop; want false")
	}
}

// A user-initiated Stop is a clean stop, not a crash: the supervisor keys its
// restart decision off this distinction (§6.3).
func TestProcessRuntime_StopIsCleanNotCrash(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHealthy(t, r, h, 5*time.Second)
	if err := r.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed after Stop")
	}
	if !h.CleanStop() {
		t.Error("CleanStop = false after an explicit Stop; want true")
	}
}

func TestProcessRuntime_CrashIsNotCleanStop(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t, "-crash-after", "150ms", "-exit-code", "3")

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	if h.CleanStop() {
		t.Error("CleanStop = true for a crashed process; want false")
	}
	if h.Status() != StatusFailed {
		t.Errorf("Status = %q; want %q", h.Status(), StatusFailed)
	}
	if code := h.ExitCode(); code != 3 {
		t.Errorf("ExitCode = %d; want 3", code)
	}
}

func TestProcessRuntime_StopKillsProcessIgnoringSIGTERM(t *testing.T) {
	r := testRuntime(t)
	r.StopGrace = 300 * time.Millisecond
	svc := testService(t, "-ignore-term")

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHealthy(t, r, h, 5*time.Second)

	start := time.Now()
	if err := r.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Stop took %s; expected escalation to SIGKILL after the grace period", elapsed)
	}
	if alive(h.PID()) {
		t.Error("process still alive after Stop")
	}
}

func TestProcessRuntime_UnhealthyWhenHealthcheckFails(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), h) })
	waitHealthy(t, r, h, 5*time.Second)

	// Flip the service into a failing healthcheck.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/unhealthy", svc.Port))
	if err != nil {
		t.Fatalf("trigger unhealthy: %v", err)
	}
	resp.Body.Close()

	ok, err := r.Healthy(context.Background(), h)
	if err == nil {
		t.Error("Healthy returned nil error for a 500 healthcheck; want an error describing the status")
	}
	if ok {
		t.Error("Healthy = true after the healthcheck started failing; want false")
	}
}

func TestProcessRuntime_CapturesStdoutAndStderr(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHealthy(t, r, h, 5*time.Second)
	if err := r.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	rc, err := r.Logs(h)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	logs := string(body)
	if !strings.Contains(logs, "fakeservice: listening on port") {
		t.Errorf("stdout not captured in logs; got:\n%s", logs)
	}
	if !strings.Contains(logs, "fakeservice: stderr stream open") {
		t.Errorf("stderr not captured in logs; got:\n%s", logs)
	}
}

// The manifest's env and OUTPOST_STORAGE_DIR must reach the process (§2).
func TestProcessRuntime_InjectsEnvAndStorageDir(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)
	svc.Env = map[string]string{"LOG_LEVEL": "debug"}

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), h) })
	waitHealthy(t, r, h, 5*time.Second)

	get := func(key string) string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/env?k=%s", svc.Port, key))
		if err != nil {
			t.Fatalf("read env %s: %v", key, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if got := get("LOG_LEVEL"); got != "debug" {
		t.Errorf("LOG_LEVEL in process = %q; want %q", got, "debug")
	}
	if got := get("OUTPOST_STORAGE_DIR"); got != svc.ExpandStorage() {
		t.Errorf("OUTPOST_STORAGE_DIR = %q; want %q", got, svc.ExpandStorage())
	}
}

// Storage must exist before the process starts; the service is entitled to
// write to it immediately (§5).
func TestProcessRuntime_CreatesStorageBeforeStart(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)
	svc.Storage = filepath.Join(t.TempDir(), "not-yet-there")

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), h) })

	if fi, err := os.Stat(svc.ExpandStorage()); err != nil || !fi.IsDir() {
		t.Fatalf("storage dir not created before start: err=%v", err)
	}
}

func TestProcessRuntime_StartRejectsMissingBinary(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)
	svc.Binary = filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := r.Start(context.Background(), svc); err == nil {
		t.Fatal("Start with a missing binary: want error, got nil")
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes for existence without delivering anything.
	return p.Signal(syscall.Signal(0)) == nil
}
