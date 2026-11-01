package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/autostart"
)

// signals stands in for signalling the daemon: each signal stops the fake
// daemon, and up decides whether its init system brings one back, on the
// binary then on disk.
type signals struct {
	mu     sync.Mutex
	pids   []int
	phases []string // the status file's phase as each signal came
}

func (s *signals) sent() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pids...)
}

func stubSignal(t *testing.T, f *selfFixture, up func(n int) (bool, string)) *signals {
	t.Helper()
	s := &signals{}
	prevSignal, prevWait := signalDaemon, selfWebWait
	t.Cleanup(func() { signalDaemon, selfWebWait = prevSignal, prevWait })
	selfWebWait = 2 * time.Second
	signalDaemon = func(pid int) error {
		st, _, _ := readSelfStatus(f.home)
		s.mu.Lock()
		s.pids = append(s.pids, pid)
		s.phases = append(s.phases, st.Phase)
		n := len(s.pids)
		s.mu.Unlock()
		f.ping.stop()
		if ok, build := up(n); ok {
			if build == "" {
				build = f.onDisk() + " (0123456789ab)"
			}
			f.ping.start(build)
		}
		return nil
	}
	return s
}

func (f *selfFixture) helper() (string, string, error) {
	f.t.Helper()
	return runCLI(f.t, f.home, selfUpdateCommand, "--label", testLabel)
}

func (f *selfFixture) status() selfStatus {
	f.t.Helper()
	st, ok, err := readSelfStatus(f.home)
	if err != nil || !ok {
		f.t.Fatalf("status file: %v %v", ok, err)
	}
	return st
}

// The helper is `update --self` with the daemon stopped by a signal, which
// its init system answers by starting it again on the new binary; it runs
// none of the init system's own commands.
func TestSelfUpdateHelper_InstallsByASignal(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	oldPID := f.ping.id.PID
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	sig := stubSignal(t, f, func(int) (bool, string) { return true, "" })

	out, errOut, err := f.helper()
	if err != nil {
		t.Fatalf("helper: %v\n%s%s", err, out, errOut)
	}
	if got := readFile(t, f.bin); got != string(outpostScript("v1.1.0")) {
		t.Errorf("binary is now:\n%s", got)
	}
	if got := sig.sent(); !slices.Equal(got, []int{oldPID}) {
		t.Errorf("signalled %v; want only the daemon on the socket, %d", got, oldPID)
	}
	if ran := f.stub.commands(); len(ran) != 0 {
		t.Errorf("ran init commands %v", ran)
	}
	if !slices.Equal(sig.phases, []string{selfRestarting}) {
		t.Errorf("phase at the signal = %v", sig.phases)
	}

	st := f.status()
	if st.Phase != selfDone || st.Failed || st.To != "v1.1.0" || st.HelperPID != os.Getpid() || st.Finished.IsZero() {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(st.Output, "Outpost is running v1.1.0 (was v1.0.0)") || !strings.Contains(st.Output, "Stopping the daemon") {
		t.Errorf("status output:\n%s", st.Output)
	}
	if !strings.Contains(out, "Outpost is running v1.1.0") {
		t.Errorf("the output isn't also where it would have gone:\n%s", out)
	}
	if fi, err := os.Stat(selfStatusPath(f.home)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("status file: %v %v", fi.Mode(), err)
	}
}

// A daemon that comes back as the wrong release is signalled again once the
// old binary is back.
func TestSelfUpdateHelper_RollsBackAWrongVersion(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	sig := stubSignal(t, f, func(n int) (bool, string) {
		if n == 1 {
			return true, "v1.0.5 (0123456789ab)"
		}
		return true, ""
	})

	out, errOut, err := f.helper()
	if err == nil || !strings.Contains(err.Error(), "came back as v1.0.5") || !strings.Contains(err.Error(), "Rolled back: the daemon is running v1.0.0") {
		t.Fatalf("err = %v\n%s%s", err, out, errOut)
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary is now:\n%s", got)
	}
	if n := len(sig.sent()); n != 2 {
		t.Errorf("signalled %d times; want 2", n)
	}
	if !slices.Equal(sig.phases, []string{selfRestarting, selfRollingBack}) {
		t.Errorf("phases at the signals = %v", sig.phases)
	}
	st := f.status()
	if st.Phase != selfFailed || !st.Failed || !strings.Contains(st.Output, "Rolled back") {
		t.Errorf("status = %+v", st)
	}
	if ran := f.stub.commands(); len(ran) != 0 {
		t.Errorf("ran init commands %v", ran)
	}
}

// In a crash loop nothing answers to be signalled: the init system's next
// start is on whatever binary is in place, so putting the old one back is
// enough.
func TestSelfUpdateHelper_RollsBackACrashLoopWithoutASecondSignal(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	sig := stubSignal(t, f, func(int) (bool, string) { return false, "" })
	selfWebWait = 300 * time.Millisecond

	// The init system, restarting the job until a binary stays up: once the
	// new one has been swapped in and out again, the old one does.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		swapped := false
		for ctx.Err() == nil {
			body, _ := os.ReadFile(f.bin)
			switch string(body) {
			case string(outpostScript("v1.1.0")):
				swapped = true
			case string(outpostScript(oldBuild)):
				if swapped {
					f.ping.start(oldBuild + " (0123456789ab)")
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	out, errOut, err := f.helper()
	if err == nil || !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), "Rolled back: the daemon is running v1.0.0") {
		t.Fatalf("err = %v\n%s%s", err, out, errOut)
	}
	if n := len(sig.sent()); n != 1 {
		t.Errorf("signalled %d times; want 1", n)
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary is now:\n%s", got)
	}
	if st := f.status(); st.Phase != selfFailed || !strings.Contains(st.Output, "No daemon is answering") {
		t.Errorf("status = %+v", st)
	}
}

// Restarting the daemon would end an app's update with no rollback.
func TestSelfUpdateHelper_RefusedDuringAnAppUpdate(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	sig := stubSignal(t, f, func(int) (bool, string) { return true, "" })
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(f.home, "bin", "yarr"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, _, err := f.helper(); err == nil || !strings.Contains(err.Error(), "an update is running (yarr)") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
	if n := len(sig.sent()); n != 0 {
		t.Errorf("signalled %d times", n)
	}
	if st := f.status(); st.Phase != selfFailed || !strings.Contains(st.Output, "an update is running (yarr)") {
		t.Errorf("status = %+v", st)
	}
}

// spawned records what Start would have run.
type spawned struct {
	mu     sync.Mutex
	argv   [][]string
	env    [][]string
	log    []string
	before []selfStatus // the status file as each spawn came
}

func stubSpawn(t *testing.T, home string) *spawned {
	t.Helper()
	s := &spawned{}
	prev := spawnSelfUpdate
	t.Cleanup(func() { spawnSelfUpdate = prev })
	spawnSelfUpdate = func(argv, env []string, log string) (int, error) {
		st, _, _ := readSelfStatus(home)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.argv, s.env, s.log, s.before = append(s.argv, argv), append(s.env, env), append(s.log, log), append(s.before, st)
		return 4242, nil
	}
	return s
}

func stubJobLabel(t *testing.T, label string) {
	t.Helper()
	prev := jobLabel
	t.Cleanup(func() { jobLabel = prev })
	jobLabel = func() string { return label }
}

func (f *selfFixture) web() *selfWeb {
	return &selfWeb{a: &app{home: f.home, out: io.Discard, err: io.Discard}}
}

func TestSelfWebStart_SpawnsTheHelperWithAFixedCommand(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	stubJobLabel(t, testLabel)
	sp := stubSpawn(t, f.home)
	t.Setenv("TS_AUTHKEY", "tskey-a")
	t.Setenv("TS_AUTH_KEY", "tskey-b")
	t.Setenv("OUTPOST_TSNET_AUTHKEY", "tskey-c")
	t.Setenv("GITHUB_TOKEN", "ghp_kept")

	w := f.web()
	if r := w.Refusal(); r != "" {
		t.Fatalf("refused: %s", r)
	}
	if err := w.Start("root"); err != nil {
		t.Fatal(err)
	}
	if len(sp.argv) != 1 {
		t.Fatalf("spawned %d times", len(sp.argv))
	}
	home, _ := filepath.Abs(f.home)
	want := []string{f.bin, "--home", home, selfUpdateCommand, "--label", testLabel}
	if f.mgr.Name() == "systemd" {
		want = append([]string{"systemd-run", "--user", "--collect", "--quiet",
			"--unit=outpost-self-update-" + testLabel, "--setenv=GITHUB_TOKEN", "--"}, want...)
	}
	if !slices.Equal(sp.argv[0], want) {
		t.Errorf("argv = %q\nwant   %q", sp.argv[0], want)
	}
	for _, kv := range sp.env[0] {
		if strings.HasPrefix(kv, "TS_AUTHKEY=") || strings.HasPrefix(kv, "TS_AUTH_KEY=") || strings.HasPrefix(kv, "OUTPOST_TSNET_AUTHKEY=") {
			t.Errorf("the helper's environment has %s", kv)
		}
	}
	if !slices.Contains(sp.env[0], "GITHUB_TOKEN=ghp_kept") {
		t.Error("the helper's environment lost GITHUB_TOKEN, which it needs for GitHub's rate limit")
	}
	if sp.log[0] != filepath.Join(f.home, "logs", "self-update.log") {
		t.Errorf("log = %s", sp.log[0])
	}
	if b := sp.before[0]; b.Phase != selfStarting || b.By != "root" || b.From == "" || b.ID == "" {
		t.Errorf("status before the spawn = %+v", b)
	}

	// A second press while the first is starting is refused, not run twice.
	if err := w.Start("root"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second Start: %v", err)
	}
	if len(sp.argv) != 1 {
		t.Errorf("spawned %d times", len(sp.argv))
	}
	if ran := f.stub.commands(); len(ran) != 0 {
		t.Errorf("ran init commands %v", ran)
	}
}

func TestSelfWebStart_RefusesADaemonNotUnderItsJob(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	stubJobLabel(t, "")
	sp := stubSpawn(t, f.home)

	w := f.web()
	if r := w.Refusal(); !strings.Contains(r, "wasn't started by its autostart job") {
		t.Errorf("refusal = %q", r)
	}
	if err := w.Start("root"); err == nil || !strings.Contains(err.Error(), "wasn't started by its autostart job") {
		t.Errorf("Start: %v", err)
	}
	if len(sp.argv) != 0 {
		t.Errorf("spawned %v", sp.argv)
	}
	if _, ok, _ := readSelfStatus(f.home); ok {
		t.Error("a refused start left a status")
	}
}

func TestSelfWebStart_RefusedDuringAnAppUpdate(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	stubJobLabel(t, testLabel)
	sp := stubSpawn(t, f.home)
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(f.home, "bin", "yarr"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if err := f.web().Start("root"); err == nil || !strings.Contains(err.Error(), "an update is running (yarr)") {
		t.Errorf("Start: %v", err)
	}
	if len(sp.argv) != 0 {
		t.Errorf("spawned %v", sp.argv)
	}
}

func TestSelfWeb_SystemdUserRunsTheHelperInAUnitOfItsOwn(t *testing.T) {
	mgr, err := autostart.For("linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gho_x")
	w := &selfWeb{a: &app{home: "/srv/outpost"}}
	job := daemonJob{mgr: mgr, cfg: autostart.Config{Label: "com.example.out post"}}
	got, err := w.helperArgv(job, "/home/o/.local/bin/outpost")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"systemd-run", "--user", "--collect", "--quiet", "--unit=outpost-self-update-com.example.out-post",
		"--setenv=GH_TOKEN", "--", "/home/o/.local/bin/outpost", "--home", "/srv/outpost", selfUpdateCommand, "--label", "com.example.out post"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q\nwant   %q", got, want)
	}

	launchd, err := autostart.For("darwin")
	if err != nil {
		t.Fatal(err)
	}
	got, err = w.helperArgv(daemonJob{mgr: launchd, cfg: autostart.Config{Label: "l"}}, "/b/outpost")
	if err != nil || got[0] != "/b/outpost" {
		t.Errorf("launchd argv = %q, %v", got, err)
	}
}

func TestSelfWeb_SystemdSystemScopeIsRefused(t *testing.T) {
	mgr, err := autostart.For("linux")
	if err != nil {
		t.Fatal(err)
	}
	err = refuseScope(daemonJob{mgr: mgr, cfg: autostart.Config{Label: "l", Scope: autostart.System, User: "outpost"}})
	if err == nil || !strings.Contains(err.Error(), "as outpost (with sudo for the restart)") {
		t.Errorf("system scope: %v", err)
	}
	if err := refuseScope(daemonJob{mgr: mgr, cfg: autostart.Config{Label: "l"}}); err != nil {
		t.Errorf("user scope: %v", err)
	}
	launchd, _ := autostart.For("darwin")
	if err := refuseScope(daemonJob{mgr: launchd, cfg: autostart.Config{Label: "l", Scope: autostart.System}}); err != nil {
		t.Errorf("launchd system scope: %v", err)
	}
}

func TestSelfStatus_RoundTripsPrivately(t *testing.T) {
	home := t.TempDir()
	want := selfStatus{ID: "abc", Started: time.Now().Round(0).UTC(), HelperPID: 7, Phase: selfWaiting, From: "v1.0.0", To: "v1.1.0", Output: "so far"}
	if err := writeSelfStatus(home, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readSelfStatus(home)
	if err != nil || !ok || !got.Started.Equal(want.Started) || got.ID != want.ID || got.Phase != want.Phase || got.Output != want.Output {
		t.Errorf("read back %+v %v %v", got, ok, err)
	}
	if fi, err := os.Stat(selfStatusPath(home)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", fi.Mode(), err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
	if _, ok, err := readSelfStatus(t.TempDir()); ok || err != nil {
		t.Errorf("missing file: %v %v", ok, err)
	}
}

func TestSelfStatus_Abandoned(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name string
		st   selfStatus
		want bool
	}{
		{"helper alive", selfStatus{Phase: selfWaiting, HelperPID: os.Getpid(), Started: now.Add(-5 * time.Minute)}, false},
		{"helper gone", selfStatus{Phase: selfWaiting, HelperPID: 3_999_999, Started: now}, true},
		{"never started", selfStatus{Phase: selfStarting, Started: now.Add(-2 * time.Minute)}, true},
		{"just started", selfStatus{Phase: selfStarting, Started: now}, false},
		{"finished", selfStatus{Phase: selfDone, HelperPID: 3_999_999, Started: now.Add(-time.Hour)}, false},
	} {
		if got := c.st.abandoned(now); got != c.want {
			t.Errorf("%s: abandoned = %v", c.name, got)
		}
	}
}

// A helper that died is reported as failed, a finished update is announced
// once, and one finished over a day ago isn't shown.
func TestSelfWebStatus_AnnouncesOnceAndForgets(t *testing.T) {
	home := t.TempDir()
	w := &selfWeb{a: &app{home: home, out: io.Discard, err: io.Discard}}
	if _, ok := w.Status(); ok {
		t.Fatal("a status with no file")
	}
	_ = writeSelfStatus(home, selfStatus{Phase: selfDownloading, HelperPID: 3_999_999, Started: time.Now(), Log: "/l/self-update.log"})
	st, ok := w.Status()
	if !ok || !st.Final || !st.Failed || !st.Announce || !strings.Contains(st.Output, "stopped before it finished. Its log: /l/self-update.log") {
		t.Errorf("abandoned = %+v %v", st, ok)
	}
	if st, _ := w.Status(); st.Announce {
		t.Error("announced twice")
	}

	_ = writeSelfStatus(home, selfStatus{Phase: selfDone, Started: time.Now().Add(-26 * time.Hour), Finished: time.Now().Add(-25 * time.Hour)})
	if _, ok := w.Status(); ok {
		t.Error("an update finished a day ago is still shown")
	}
}

func TestSelfWebLatest_FollowsUpdateSelfsRules(t *testing.T) {
	f := newSelfFixture(t)
	releases := newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	w := f.web()
	got, err := w.Latest(context.Background())
	if err != nil || !got.Newer || got.Latest != "v1.1.0" {
		t.Errorf("newer: %+v %v", got, err)
	}
	releases.mu.Lock()
	releases.latest = "v0.9.0"
	releases.releases["v0.9.0"] = selfRelease{binary: outpostScript("v0.9.0")}
	releases.mu.Unlock()
	if got, err := w.Latest(context.Background()); err != nil || got.Newer || !strings.Contains(got.Note, "Newer than the latest release") {
		t.Errorf("older: %+v %v", got, err)
	}
}
