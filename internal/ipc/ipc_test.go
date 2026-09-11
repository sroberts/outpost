package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// testDaemon spins up a real supervisor behind a real socket.
func testDaemon(t *testing.T, services ...string) (*Client, string) {
	t.Helper()
	home := t.TempDir()
	svcDir := filepath.Join(home, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, name := range services {
		body := fmt.Sprintf(`
name    = %q
runtime = "process"
binary  = "/bin/sleep"
args    = ["3600"]
port    = %d
storage = %q
tailscale_name = %q
`, name, 19100+i, filepath.Join(home, "data", name), name)
		if err := os.WriteFile(filepath.Join(svcDir, name+".toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    svcDir,
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		Interval:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); _ = sup.Run(ctx) }()
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = srv.Serve(ctx) }()

	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-serveDone
		<-loopDone
	})

	return NewClient(sockPath), home
}

func TestServer_PingAndAvailability(t *testing.T) {
	c, _ := testDaemon(t)
	if !c.Available() {
		t.Fatal("Available = false with a daemon listening")
	}
	resp, err := c.do(Request{Command: CmdPing})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if resp.Message != "pong" {
		t.Errorf("ping returned %q; want pong", resp.Message)
	}
}

func TestClient_NoDaemon(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "outpost.sock"))
	if c.Available() {
		t.Error("Available = true with no daemon")
	}
	if _, err := c.List(); err == nil {
		t.Fatal("List with no daemon: want error, got nil")
	}
}

func TestServer_ListReportsDeclaredServices(t *testing.T) {
	c, _ := testDaemon(t, "alpha", "beta")

	// The loop starts services on its first tick. Waiting for both to report
	// "running" would be waiting for two independent blips to coincide: the
	// fixture listens on nothing, so that status lasts only from a start until
	// the first failed healthcheck. Having been started is the durable fact.
	waitFor(t, 10*time.Second, func() bool {
		services, err := c.List()
		if err != nil {
			return false
		}
		started := 0
		for _, s := range services {
			if s.InstanceID != "" {
				started++
			}
		}
		return started == 2
	})

	services, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 {
		t.Fatalf("List returned %d services; want 2", len(services))
	}
	for _, s := range services {
		if s.Port == 0 {
			t.Errorf("%s: manifest detail missing from the status (port = 0)", s.Name)
		}
		if s.InstanceID == "" {
			t.Errorf("%s: no instance id for a running service", s.Name)
		}
	}
}

func TestServer_StopAndStartRoundTrip(t *testing.T) {
	c, _ := testDaemon(t, "alpha")
	// The fixture is /bin/sleep, which listens on nothing, so it never settles
	// on "running" -- that status is a blip between a start and the first
	// failed healthcheck, observed in roughly three samples out of ninety.
	// Waiting for it is racing a window; what this test is actually about is
	// that stop and start take effect, so it asserts on desired state and on
	// the instance being replaced.
	waitFor(t, 10*time.Second, func() bool {
		st, err := c.Status("alpha")
		return err == nil && st.InstanceID != ""
	})
	first, err := c.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Stop("alpha"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err := c.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	// The daemon reconciles before replying, so the effect is visible at once.
	if st.Status != string(runtime.StatusStopped) {
		t.Errorf("status after Stop = %q; want stopped", st.Status)
	}
	if st.Desired != "stopped" {
		t.Errorf("desired after Stop = %q; want stopped", st.Desired)
	}

	if _, err := c.Start("alpha"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err = c.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.Desired != "running" {
		t.Errorf("desired after Start = %q; want running", st.Desired)
	}
	if st.Status == string(runtime.StatusStopped) {
		t.Errorf("status after Start = %q; want it up again", st.Status)
	}
	// A new process, not the one Stop killed.
	if st.InstanceID == "" || st.InstanceID == first.InstanceID {
		t.Errorf("instance after Start = %q; want a new one (was %q)", st.InstanceID, first.InstanceID)
	}
}

func TestServer_RestartReplacesInstance(t *testing.T) {
	c, _ := testDaemon(t, "alpha")
	// Having an instance is durable; "running" is not, for a fixture that
	// serves no healthcheck.
	waitFor(t, 10*time.Second, func() bool {
		st, err := c.Status("alpha")
		return err == nil && st.InstanceID != ""
	})

	before, err := c.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Restart("alpha"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	after, err := c.Status("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if after.InstanceID == before.InstanceID {
		t.Errorf("instance id unchanged after restart (%s)", after.InstanceID)
	}
}

func TestServer_UnknownServiceIsAnError(t *testing.T) {
	c, _ := testDaemon(t, "alpha")
	if _, err := c.Status("ghost"); err == nil {
		t.Error("Status ghost: want error, got nil")
	}
	if _, err := c.Stop("ghost"); err == nil {
		t.Error("Stop ghost: want error, got nil")
	}
}

func TestServer_ApplyPicksUpANewManifest(t *testing.T) {
	c, home := testDaemon(t, "alpha")
	waitFor(t, 3*time.Second, func() bool {
		services, err := c.List()
		return err == nil && len(services) == 1
	})

	body := fmt.Sprintf(`
name    = "gamma"
runtime = "process"
binary  = "/bin/sleep"
args    = ["3600"]
port    = 19199
storage = %q
tailscale_name = "gamma"
`, filepath.Join(home, "data", "gamma"))
	if err := os.WriteFile(filepath.Join(home, "services", "gamma.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	services, err := c.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("Apply reported %d services; want 2", len(services))
	}
}

func TestServer_EventsAreReadable(t *testing.T) {
	c, _ := testDaemon(t, "alpha")
	waitFor(t, 3*time.Second, func() bool {
		events, err := c.Events("alpha", 20)
		return err == nil && len(events) > 0
	})

	events, err := c.Events("alpha", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Kind == state.EventStarted {
			found = true
		}
	}
	if !found {
		t.Errorf("no started event in %v", events)
	}
}

func TestServer_MalformedRequestGetsAnError(t *testing.T) {
	c, _ := testDaemon(t)
	conn, err := net.Dial("unix", c.path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("{not json"))
	_ = conn.(*net.UnixConn).CloseWrite()

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no reply to a malformed request: n=%d err=%v", n, err)
	}
	if !strings.Contains(string(buf[:n]), "malformed request") {
		t.Errorf("unhelpful reply: %s", buf[:n])
	}
}

// A socket left by a dead daemon must not block a new one from starting.
func TestListen_RemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(shortDir(t), "outpost.sock")

	// A plain file standing in for a socket nothing is listening on.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // leaves the file behind on some platforms
	if _, err := os.Stat(sockPath); err != nil {
		if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sup := newIdleSupervisor(t, dir)
	srv, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	srv.Close()
}

func TestListen_RefusesToStealALiveSocket(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	sup := newIdleSupervisor(t, dir)

	first, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = first.Serve(ctx) }()

	if _, err := Listen(sockPath, sup, nil); err == nil {
		t.Fatal("second Listen on a live socket: want error, got nil")
	}
}

// newIdleSupervisor builds a supervisor with no services and no running loop.
func newIdleSupervisor(t *testing.T, dir string) *supervisor.Supervisor {
	t.Helper()
	svcDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    svcDir,
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(dir, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sup
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}

// shortDir returns a directory short enough to hold a unix socket path, which
// t.TempDir() on darwin is not.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "op")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
