package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// newHome returns an isolated Outpost root with one declared service.
func newHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "services"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func writeManifest(t *testing.T, home, name string, port int) {
	t.Helper()
	body := fmt.Sprintf(`
name    = %q
runtime = "process"
binary  = "/bin/sleep"
args    = ["3600"]
port    = %d
storage = %q
tailscale_name = %q
`, name, port, filepath.Join(home, "data", name), name)
	if err := os.WriteFile(filepath.Join(home, "services", name+".toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// runCLI invokes a command against an isolated home and returns its output.
func runCLI(t *testing.T, home string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(append([]string{"--home", home}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func TestCLI_ApplyValidatesAndReportsNoDaemon(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))

	out, _, err := runCLI(t, home, "apply")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "1 service(s) valid") {
		t.Errorf("apply output missing the validation summary:\n%s", out)
	}
	if !strings.Contains(out, "No daemon is running") {
		t.Errorf("apply did not report the absent daemon:\n%s", out)
	}
	// Storage must have been created as a side effect of a successful apply.
	if fi, err := os.Stat(filepath.Join(home, "data", "alpha")); err != nil || !fi.IsDir() {
		t.Errorf("storage dir not created by apply: %v", err)
	}
}

func TestCLI_ApplyFailsLoudlyOnInvalidManifest(t *testing.T) {
	home := newHome(t)
	port := freePort(t)
	writeManifest(t, home, "alpha", port)
	// A second service on the same port violates §5.
	writeManifest(t, home, "beta", port)

	_, _, err := runCLI(t, home, "apply")
	if err == nil {
		t.Fatal("apply with colliding ports: want error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate port") {
		t.Errorf("error does not name the collision: %v", err)
	}
}

func TestCLI_ListShowsDeclaredServicesBeforeAnythingRuns(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))

	out, _, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("list omitted a declared service:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("list should show an unobserved service as unknown:\n%s", out)
	}
}

func TestCLI_ListEmptyHome(t *testing.T) {
	home := newHome(t)
	out, _, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "No services declared") {
		t.Errorf("unexpected output for an empty home:\n%s", out)
	}
}

// Control commands go through the daemon (§12), so without one they must fail
// clearly rather than half-acting.
func TestCLI_StopWithoutDaemonIsAnError(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))

	for _, verb := range []string{"start", "stop", "restart", "status"} {
		_, _, err := runCLI(t, home, verb, "alpha")
		if err == nil {
			t.Errorf("%s with no daemon: want error, got nil", verb)
			continue
		}
		if !strings.Contains(err.Error(), "outpost serve") {
			t.Errorf("%s error does not say how to fix it: %v", verb, err)
		}
	}
}

func TestCLI_CommandsRejectUnknownService(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))

	// With no daemon these fail on the missing daemon; the daemon itself
	// rejects unknown service names (see internal/ipc).
	for _, verb := range []string{"start", "stop", "restart"} {
		if _, _, err := runCLI(t, home, verb, "ghost"); err == nil {
			t.Errorf("%s ghost: want error, got nil", verb)
		}
	}
}

func TestCLI_RequiresServiceName(t *testing.T) {
	home := newHome(t)
	for _, verb := range []string{"start", "stop", "restart", "status", "logs"} {
		if _, _, err := runCLI(t, home, verb); err == nil {
			t.Errorf("%s without a service name: want error, got nil", verb)
		}
	}
}

func TestCLI_LogsMissingFileIsAClearError(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))

	_, _, err := runCLI(t, home, "logs", "alpha")
	if err == nil {
		t.Fatal("logs before anything ran: want error, got nil")
	}
	if !strings.Contains(err.Error(), "no logs for alpha") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestCLI_UnknownCommand(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "frobnicate"); err == nil {
		t.Fatal("unknown command: want error, got nil")
	}
}

func TestCLI_HomeFlagOverridesEnv(t *testing.T) {
	t.Setenv("OUTPOST_HOME", filepath.Join(t.TempDir(), "from-env"))
	explicit := newHome(t)

	home, rest, err := parseGlobals([]string{"list", "--home", explicit})
	if err != nil {
		t.Fatal(err)
	}
	if home != explicit {
		t.Errorf("home = %q; want the --home flag value %q", home, explicit)
	}
	if len(rest) != 1 || rest[0] != "list" {
		t.Errorf("rest = %v; want [list]", rest)
	}
}

func TestCLI_HomeFromEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("OUTPOST_HOME", want)

	home, _, err := parseGlobals([]string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	if home != want {
		t.Errorf("home = %q; want %q", home, want)
	}
}

func TestSupervisorPID_StalePidfileReadsAsNotRunning(t *testing.T) {
	home := newHome(t)
	a := &app{home: home, out: os.Stdout, err: os.Stderr}

	// A PID that is almost certainly not a live process.
	if err := os.WriteFile(a.pidPath(), []byte(strconv.Itoa(1<<22)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, running := a.supervisorPID(); running {
		t.Error("stale pidfile reported as a running supervisor")
	}

	if err := os.WriteFile(a.pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, running := a.supervisorPID(); !running || pid != os.Getpid() {
		t.Errorf("live pidfile: pid=%d running=%v; want %d true", pid, running, os.Getpid())
	}
}
