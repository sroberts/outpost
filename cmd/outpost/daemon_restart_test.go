package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// testLabel is never the real job's. The seams below already keep these tests
// away from launchd; this is so that a mistake in them still couldn't name it.
const testLabel = "com.example.outpost-restart-test"

// initStub replaces the init system. Everything run or asked is recorded.
type initStub struct {
	mu     sync.Mutex
	loaded bool
	onRun  func(argv []string) error
	ran    [][]string
	asked  [][]string
}

func (s *initStub) commands() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ran
}

func (s *initStub) questions() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func stubInit(t *testing.T, loaded bool) *initStub {
	t.Helper()
	sandboxUnitDir(t)
	s := &initStub{loaded: loaded}
	oldCmd, oldProbe, oldUID, oldPoll := initCommand, initProbe, effectiveUID, daemonPoll
	t.Cleanup(func() { initCommand, initProbe, effectiveUID, daemonPoll = oldCmd, oldProbe, oldUID, oldPoll })
	initCommand = func(_ *app, argv []string) error {
		s.mu.Lock()
		s.ran = append(s.ran, argv)
		onRun := s.onRun
		s.mu.Unlock()
		if onRun != nil {
			return onRun(argv)
		}
		return nil
	}
	initProbe = func(argv []string) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.asked = append(s.asked, argv)
		return s.loaded
	}
	effectiveUID = func() int { return 501 }
	daemonPoll = 5 * time.Millisecond
	return s
}

// shortHome is a home whose socket fits within darwin's path limit.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "op")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, "services"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

// writeJob installs a user-scope unit file running the given home.
func writeJob(t *testing.T, runs string) (autostart.Manager, autostart.Config, string) {
	t.Helper()
	mgr, err := autostart.ForHost()
	if err != nil {
		t.Skip(err)
	}
	cfg := autostart.Config{Label: testLabel, Binary: "/bin/outpost", Home: runs}
	path, err := mgr.UnitPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := mgr.Render(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return mgr, cfg, path
}

// startDaemon runs a real supervisor and socket for home, and returns what
// stops them. Two in a row share a PID, which leaves only the start time to
// tell them apart, the harder case.
func startDaemon(t *testing.T, home string) (stop func()) {
	t.Helper()
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		BackupDir:      filepath.Join(home, "backups"),
		Interval:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ipc.Listen(filepath.Join(home, "outpost.sock"), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loopDone, serveDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(loopDone); _ = sup.Run(ctx) }()
	go func() { defer close(serveDone); _ = srv.Serve(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			srv.Close()
			<-serveDone
			<-loopDone
			store.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}

func restart(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	out, _, err := runCLI(t, home, append([]string{"daemon", "restart", "--label", testLabel}, args...)...)
	return out, err
}

// The restart isn't over when the socket answers, but when a different daemon
// does: the one that answers next may be the old one, not yet stopped.
func TestDaemonRestart_WaitsForTheNewDaemon(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	mgr, cfg, _ := writeJob(t, home)
	stopOld := startDaemon(t, home)
	before, err := ipc.NewClient(filepath.Join(home, "outpost.sock")).Identity()
	if err != nil {
		t.Fatal(err)
	}

	var stopNew func()
	done := make(chan struct{})
	stub.onRun = func([]string) error {
		// Returns before the swap, as kickstart does.
		go func() {
			defer close(done)
			time.Sleep(50 * time.Millisecond) // still the old daemon, briefly
			stopOld()
			time.Sleep(50 * time.Millisecond) // nothing listening
			stopNew = startDaemon(t, home)
		}()
		return nil
	}

	out, err := restart(t, home)
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	<-done
	defer stopNew()

	for _, want := range mgr.RestartCommands(cfg, 501) {
		if !strings.Contains(out, "$ "+strings.Join(want, " ")) {
			t.Errorf("output doesn't show %v:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "The daemon is back") || !strings.Contains(out, "SIGTERM") {
		t.Errorf("output:\n%s", out)
	}
	after, err := ipc.NewClient(filepath.Join(home, "outpost.sock")).Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !after.Started.After(before.Started) {
		t.Errorf("started %v, then %v", before.Started, after.Started)
	}
	if got, want := len(stub.commands()), len(mgr.RestartCommands(cfg, 501)); got != want {
		t.Errorf("ran %d commands; want %d", got, want)
	}
}

// The restart command succeeding proves nothing if the old daemon stays up,
// which is what happens when it was started by hand and the job's copy can't
// bind the socket. That must not be reported as a restart.
func TestDaemonRestart_OldDaemonStayingUpIsAFailure(t *testing.T) {
	stubInit(t, true)
	home := shortHome(t)
	mgr, cfg, _ := writeJob(t, home)
	startDaemon(t, home)

	out, err := restart(t, home, "--wait", "50ms")
	if err == nil {
		t.Fatalf("reported success with the old daemon still up:\n%s", out)
	}
	if !strings.Contains(err.Error(), "still answering") || !strings.Contains(err.Error(), mgr.LogHint(cfg)) {
		t.Errorf("err = %v", err)
	}
}

// Each way of not coming back has a different cause, so they get different
// messages, and every one says where to look.
func TestAwaitNewDaemon_NamesWhatItWasWaitingFor(t *testing.T) {
	stubInit(t, true)
	home := shortHome(t)
	a := &app{home: home, out: &strings.Builder{}, err: &strings.Builder{}}

	// A process that has exited: the old daemon has gone.
	gone := exec.Command("/bin/sh", "-c", "exit 0")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	_, err := a.awaitNewDaemon(ipc.DaemonIdentity{PID: gone.Process.Pid}, 20*time.Millisecond, "the-log")
	if err == nil || !strings.Contains(err.Error(), "no new one answered") || !strings.Contains(err.Error(), "the-log") {
		t.Errorf("old daemon gone: %v", err)
	}

	// A process still alive with its socket closed: stopping its services.
	_, err = a.awaitNewDaemon(ipc.DaemonIdentity{PID: os.Getpid()}, 20*time.Millisecond, "the-log")
	if err == nil || !strings.Contains(err.Error(), "still stopping") || !strings.Contains(err.Error(), "the-log") {
		t.Errorf("old daemon stopping: %v", err)
	}
}

func TestIsSameRun(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(2000, 0)
	cases := []struct {
		name     string
		old, now ipc.DaemonIdentity
		want     bool
	}{
		{"same pid and start", ipc.DaemonIdentity{PID: 7, Started: t0}, ipc.DaemonIdentity{PID: 7, Started: t0}, true},
		{"pid reused", ipc.DaemonIdentity{PID: 7, Started: t0}, ipc.DaemonIdentity{PID: 7, Started: t1}, false},
		{"new pid", ipc.DaemonIdentity{PID: 7, Started: t0}, ipc.DaemonIdentity{PID: 8, Started: t0}, false},
		// The upgrade itself: the old daemon predates reporting its identity,
		// so its PID came from the pidfile.
		{"old can't say, same pid", ipc.DaemonIdentity{PID: 7}, ipc.DaemonIdentity{PID: 7, Started: t1}, true},
		{"old can't say, new pid", ipc.DaemonIdentity{PID: 7}, ipc.DaemonIdentity{PID: 8, Started: t1}, false},
	}
	for _, c := range cases {
		if got := isSameRun(c.old, c.now); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// A daemon running in a terminal is the operator's: no job means nothing here
// may touch it.
func TestDaemonRestart_NoJobLeavesAHandStartedDaemonAlone(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	startDaemon(t, home)

	_, err := restart(t, home)
	if err == nil {
		t.Fatal("restart succeeded with no job")
	}
	for _, want := range []string{"no autostart job", "left alone", "outpost install --enable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
	if !ipc.NewClient(filepath.Join(home, "outpost.sock")).Available() {
		t.Error("the daemon went away")
	}
}

func TestDaemonRestart_NoJobNoDaemon(t *testing.T) {
	stubInit(t, true)
	_, err := restart(t, shortHome(t))
	if err == nil || !strings.Contains(err.Error(), "no autostart job") || strings.Contains(err.Error(), "left alone") {
		t.Errorf("err = %v", err)
	}
}

// The job with this label runs another home. This is the mistake that would
// take down someone's real services from a scratch --home, so it must stop
// before the init system hears anything about it.
func TestDaemonRestart_RefusesAJobRunningAnotherHome(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	other := shortHome(t)
	writeJob(t, other)

	_, err := restart(t, home)
	if err == nil {
		t.Fatal("restarted another home's job")
	}
	if !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), "left alone") {
		t.Errorf("err = %v", err)
	}
	if len(stub.commands()) != 0 || len(stub.questions()) != 0 {
		t.Errorf("ran %v, asked %v", stub.commands(), stub.questions())
	}
}

// A hand-started daemon in a scratch home, on a machine whose real job has the
// default label. The system-scope path doesn't move with $HOME, so sandboxing
// that isn't enough to reach the no-job message; it has to still say what to do.
func TestDaemonRestart_AnotherHomesJobMentionsAHandStartedDaemon(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	writeJob(t, shortHome(t))
	startDaemon(t, home)

	_, err := restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "no autostart job runs this home") ||
		!strings.Contains(err.Error(), "A daemon is running") {
		t.Errorf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
	if !ipc.NewClient(filepath.Join(home, "outpost.sock")).Available() {
		t.Error("the daemon went away")
	}
}

// An unreadable file gets the same treatment as a foreign one: restarting what
// might be someone else's daemon is the worse mistake.
func TestDaemonRestart_RefusesAJobItCantRead(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	_, _, path := writeJob(t, home)
	if err := os.WriteFile(path, []byte("not a unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "can't tell") {
		t.Errorf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

func TestDaemonRestart_NotLoadedSaysHowToStartIt(t *testing.T) {
	stub := stubInit(t, false)
	home := shortHome(t)
	mgr, cfg, path := writeJob(t, home)

	_, err := restart(t, home)
	if err == nil {
		t.Fatal("restarted a job that isn't loaded")
	}
	for _, cmd := range mgr.EnableCommands(path, cfg) {
		if !strings.Contains(err.Error(), strings.Join(cmd, " ")) {
			t.Errorf("err lacks %v: %v", cmd, err)
		}
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

func TestDaemonRestart_PerUserJobRefusesRoot(t *testing.T) {
	stub := stubInit(t, true)
	effectiveUID = func() int { return 0 }
	home := shortHome(t)
	writeJob(t, home)

	_, err := restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Errorf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

func TestDaemonRestart_FailedCommandNamesTheLog(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	mgr, cfg, _ := writeJob(t, home)
	stub.onRun = func([]string) error { return errors.New("boom") }

	_, err := restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), mgr.LogHint(cfg)) {
		t.Errorf("err = %v", err)
	}
}

// Uses the real lock, so this fails if lockUpdate moves it.
func TestDaemonRestart_RefusesDuringAnUpdate(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	writeJob(t, home)
	stopOld := startDaemon(t, home)

	dir := filepath.Join(home, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(dir, "yarr"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, err = restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "yarr") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}

	var stopNew func()
	done := make(chan struct{})
	stub.onRun = func([]string) error {
		go func() {
			defer close(done)
			stopOld()
			stopNew = startDaemon(t, home)
		}()
		return nil
	}
	if out, err := restart(t, home, "--force"); err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	<-done
	stopNew()
}

// An update that finished leaves its lock file behind, which isn't an update.
func TestDaemonRestart_IgnoresAFinishedUpdatesLockFile(t *testing.T) {
	home := shortHome(t)
	dir := filepath.Join(home, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(dir, "yarr"))
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	updating, release, err := holdUpdateLocks(home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if len(updating) != 0 {
		t.Errorf("updating = %v", updating)
	}
}

// Between the check and the restart, a new update must not get in.
func TestHoldUpdateLocks_KeepsAnUpdateOut(t *testing.T) {
	home := shortHome(t)
	bin := filepath.Join(home, "bin", "yarr")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(bin)
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	_, release, err := holdUpdateLocks(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockUpdate(bin); err == nil {
		t.Error("an update got the lock while it was held")
	}
	release()
	unlock, err = lockUpdate(bin)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	unlock()
}

func TestHoldUpdateLocks_NoBinDirectory(t *testing.T) {
	updating, release, err := holdUpdateLocks(shortHome(t))
	if err != nil || len(updating) != 0 {
		t.Errorf("updating %v, err %v", updating, err)
	}
	release()
}

// A running daemon tolerates a bad manifest and a starting one doesn't, so
// restarting on one would turn an unnoticed typo into every service down.
func TestDaemonRestart_RefusesAManifestTheNewDaemonWouldRefuse(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	writeJob(t, home)
	if err := os.WriteFile(filepath.Join(home, "services", "bad.toml"), []byte("name = ["), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "refuse to start") {
		t.Fatalf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
	_, err = restart(t, home, "--force", "--wait", "1ms")
	if len(stub.commands()) == 0 {
		t.Errorf("--force ran nothing (err %v)", err)
	}
}

func TestDaemon_Usage(t *testing.T) {
	stubInit(t, true)
	home := shortHome(t)
	for _, args := range [][]string{{"daemon"}, {"daemon", "stop"}} {
		if _, _, err := runCLI(t, home, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, _, err := runCLI(t, home, "daemon", "restart", "extra"); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("extra argument: %v", err)
	}
}

func installJob(t *testing.T, home string, args ...string) string {
	t.Helper()
	out, _, err := runCLI(t, home, append([]string{"install", "--no-tailscale", "--label", testLabel}, args...)...)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	return out
}

// A fresh install keeps saying what it always has, and doesn't ask the init
// system about a job it has only just written.
func TestInstall_FreshKeepsTheEnableInstructions(t *testing.T) {
	stub := stubInit(t, true)
	out := installJob(t, shortHome(t))

	if !strings.Contains(out, "To start it now and keep it running:") || strings.Contains(out, "already loaded") {
		t.Errorf("output:\n%s", out)
	}
	if len(stub.questions()) != 0 || len(stub.commands()) != 0 {
		t.Errorf("asked %v, ran %v", stub.questions(), stub.commands())
	}
}

func TestInstall_NotLoadedKeepsTheEnableInstructions(t *testing.T) {
	stubInit(t, false)
	home := shortHome(t)
	installJob(t, home)
	if out := installJob(t, home); !strings.Contains(out, "To start it now and keep it running:") {
		t.Errorf("output:\n%s", out)
	}
}

// What a real upgrade hit: the printed `launchctl load -w` does nothing to a
// job that's already loaded.
func TestInstall_AlreadyLoadedSuggestsARestart(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	installJob(t, home)

	out := installJob(t, home)
	if !strings.Contains(out, testLabel+" is already loaded") || !strings.Contains(out, "daemon restart --label "+testLabel) {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "To start it now") || strings.Contains(out, " load -w") || strings.Contains(out, "enable --now") {
		t.Errorf("still offers to enable it:\n%s", out)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

// launchd's restart wouldn't apply the rewritten file, so suggesting it would
// be the same dishonesty again.
func TestInstall_ChangedFileOnLoadedJob(t *testing.T) {
	stubInit(t, true)
	home := shortHome(t)
	mgr, cfg, path := writeJob(t, home)
	if err := os.WriteFile(path, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := installJob(t, home)
	reload := mgr.ReloadCommands(path, cfg, 501)
	if len(reload) == 0 {
		if !strings.Contains(out, "daemon restart") {
			t.Errorf("output:\n%s", out)
		}
		return
	}
	for _, cmd := range reload {
		if !strings.Contains(out, strings.Join(cmd, " ")) {
			t.Errorf("output lacks %v:\n%s", cmd, out)
		}
	}
	if strings.Contains(out, "daemon restart") {
		t.Errorf("suggests a restart that wouldn't apply it:\n%s", out)
	}
}

func TestInstall_EnableOnLoadedJob(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	mgr, cfg, path := writeJob(t, home)
	installJob(t, home)

	out := installJob(t, home, "--enable")
	if !strings.Contains(out, "is already loaded") {
		t.Errorf("output:\n%s", out)
	}
	want := 0
	if mgr.EnableIsIdempotent() {
		want = len(mgr.EnableCommands(path, cfg))
	}
	if got := len(stub.commands()); got != want {
		t.Errorf("ran %v; want %d commands", stub.commands(), want)
	}
}

// Sets what an old daemon's lack of an identity falls back on.
func TestCurrentDaemon_FallsBackOnThePIDFile(t *testing.T) {
	home := shortHome(t)
	a := &app{home: home}
	if id := a.currentDaemon(); id.PID != 0 {
		t.Errorf("no daemon, no pidfile: pid %d", id.PID)
	}
	if err := a.writePID(); err != nil {
		t.Fatal(err)
	}
	if id := a.currentDaemon(); id.PID != os.Getpid() {
		t.Errorf("pidfile: pid %d", id.PID)
	}
}
