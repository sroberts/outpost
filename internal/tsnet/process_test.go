package tsnet

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperNodeProcess is not a test. It is the fake node the tests below
// start as a child process, in place of a real tsnet node.
func TestHelperNodeProcess(t *testing.T) {
	if os.Getenv("OUTPOST_HELPER_NODE") != "1" {
		return
	}
	if f := os.Getenv("OUTPOST_HELPER_COUNT"); f != "" {
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fh.WriteString("x")
			_ = fh.Close()
		}
	}
	waitForEOF := func() { _, _ = io.Copy(io.Discard, os.Stdin) }

	switch os.Getenv("OUTPOST_HELPER_MODE") {
	case "ready":
		fmt.Println(ReadyLine)
		waitForEOF()
		os.Exit(0)
	case "panic-after-ready":
		fmt.Println(ReadyLine)
		// Up long enough to be adopted, as the real node was before it died.
		time.Sleep(100 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "panic: ipnlocal: watchdog timeout")
		os.Exit(2)
	case "fail":
		fmt.Fprintln(os.Stderr, "start tsnet node: state directory is locked")
		os.Exit(1)
	case "login":
		fmt.Fprintln(os.Stderr, "To authenticate, visit: https://login.tailscale.com/a/abc123")
		waitForEOF()
		os.Exit(0)
	case "ignore-eof":
		fmt.Println(ReadyLine)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "env":
		fmt.Fprintln(os.Stderr, "authkey="+os.Getenv(AuthKeyEnv))
		fmt.Fprintln(os.Stderr, "args="+strings.Join(os.Args[len(os.Args)-8:], " "))
		fmt.Println(ReadyLine)
		waitForEOF()
		os.Exit(0)
	}
	os.Exit(3)
}

// logSink collects the router's log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logSink) has(sub string) bool {
	for _, line := range l.snapshot() {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// processRouter builds a Router whose nodes are real child processes running
// TestHelperNodeProcess in the given mode.
func processRouter(t *testing.T, mode, authKey string) (*Router, *logSink, string) {
	t.Helper()
	logs := &logSink{}
	r, err := New(Options{StateDir: t.TempDir(), AuthKey: authKey, Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(t.TempDir(), "starts")
	r.nodeCmd = func(cfg nodeConfig) (*exec.Cmd, error) {
		args := append([]string{"-test.run=^TestHelperNodeProcess$", "--"}, nodeArgs(cfg)...)
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(),
			"OUTPOST_HELPER_NODE=1", "OUTPOST_HELPER_MODE="+mode, "OUTPOST_HELPER_COUNT="+count)
		return cmd, nil
	}
	r.retryInitial = 10 * time.Millisecond
	r.retryMax = 50 * time.Millisecond
	r.nodeStopGrace = 2 * time.Second
	t.Cleanup(func() { _ = r.Close() })
	return r, logs, count
}

func starts(t *testing.T, count string) int {
	t.Helper()
	b, _ := os.ReadFile(count)
	return len(b)
}

// The panic this exists for: a node that dies must cost one service its
// tailnet exposure, not the daemon, and it must come back on its own.
func TestProcessNode_CrashIsContainedAndRestarted(t *testing.T) {
	r, logs, count := processRouter(t, "panic-after-ready", "")
	if err := r.Attach(context.Background(), svc("crashy", 9201)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "the node to be restarted after crashing", func() bool {
		return starts(t, count) >= 3
	})
	if !logs.has("watchdog timeout") {
		t.Errorf("the node's dying words were not logged: %q", logs.snapshot())
	}
	if !logs.has("node process exited") {
		t.Errorf("the restart was not logged: %q", logs.snapshot())
	}
}

// A ready node is reported attached, and Close stops its process.
func TestProcessNode_ReadyIsAttachedAndCloseStopsIt(t *testing.T) {
	r, _, count := processRouter(t, "ready", "")
	if err := r.Attach(context.Background(), svc("steady", 9202)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "attach", func() bool { return len(r.Attached()) == 1 })

	start := time.Now()
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > r.nodeStopGrace {
		t.Errorf("Close took %s; the node should leave on stdin EOF, well inside the grace period", elapsed)
	}
	if got := starts(t, count); got != 1 {
		t.Errorf("node started %d times; a healthy node must not be restarted", got)
	}
}

// A node that will not leave when asked is killed, not waited on forever.
func TestProcessNode_IgnoringStopIsKilled(t *testing.T) {
	r, _, _ := processRouter(t, "ignore-eof", "")
	r.nodeStopGrace = 200 * time.Millisecond
	if err := r.Attach(context.Background(), svc("stubborn", 9203)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "attach", func() bool { return len(r.Attached()) == 1 })

	done := make(chan error, 1)
	go func() { done <- r.Detach(context.Background(), "stubborn") }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Detach: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Detach waited on a node that ignored the request to stop")
	}
}

// A node that fails before coming up is retried, and the error says why.
func TestProcessNode_FailureBeforeReadyIsRetriedWithItsReason(t *testing.T) {
	r, logs, count := processRouter(t, "fail", "")
	if err := r.Attach(context.Background(), svc("locked", 9204)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "retries", func() bool { return starts(t, count) >= 2 })
	if !logs.has("state directory is locked") {
		t.Errorf("the reason was not reported: %q", logs.snapshot())
	}
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v; a node that never came up is not attached", got)
	}
}

// A first run waits on a login. The URL must reach the operator, and a detach
// while waiting must end the process rather than leave it holding the state
// directory.
func TestProcessNode_LoginURLIsLoggedAndDetachEndsTheWait(t *testing.T) {
	r, logs, _ := processRouter(t, "login", "")
	if err := r.Attach(context.Background(), svc("newbie", 9205)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "the login URL", func() bool {
		return logs.has("newbie: To authenticate, visit: https://login.tailscale.com/a/abc123")
	})
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v while waiting on a login", got)
	}

	start := time.Now()
	if err := r.Detach(context.Background(), "newbie"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if elapsed := time.Since(start); elapsed > r.nodeStopGrace {
		t.Errorf("Detach took %s", elapsed)
	}
}

// The auth key travels in the environment, never on the command line where
// any local user could read it from the process table.
func TestProcessNode_AuthKeyIsNotAnArgument(t *testing.T) {
	r, logs, _ := processRouter(t, "env", "tskey-secret")
	if err := r.Attach(context.Background(), svc("keyed", 9206)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "attach", func() bool { return len(r.Attached()) == 1 })
	// stderr is read on its own goroutine, so the ready line on stdout can
	// attach the node before the lines the helper wrote first are logged.
	// Without the args line the check below would pass by having nothing to
	// look at.
	waitFor(t, 10*time.Second, "the node's environment and arguments to be logged", func() bool {
		return logs.has("authkey=") && logs.has("args=")
	})

	if !logs.has("authkey=tskey-secret") {
		t.Errorf("the node did not receive the auth key: %q", logs.snapshot())
	}
	for _, line := range logs.snapshot() {
		if strings.Contains(line, "args=") && strings.Contains(line, "tskey-secret") {
			t.Errorf("the auth key was passed as an argument: %q", line)
		}
	}
	if !logs.has("--port 9206") {
		t.Errorf("the node was not told the service port: %q", logs.snapshot())
	}
}
