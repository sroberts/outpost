package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/version"
)

// outpostScript is a stand-in Outpost binary: it answers --version the way
// the real one does, so the staged-download probe runs for real.
func outpostScript(tag string) []byte {
	return []byte("#!/bin/sh\necho 'outpost " + tag + "'\necho '  go:       go1.26'\n")
}

// selfRelease is one release of sroberts/outpost on the fake GitHub.
type selfRelease struct {
	binary      []byte
	noChecksums bool
	wrongSum    bool
}

// fakeOutpostReleases is GitHub serving Outpost's own releases.
type fakeOutpostReleases struct {
	mu       sync.Mutex
	latest   string
	releases map[string]selfRelease
	requests int
}

func newFakeOutpostReleases(t *testing.T, latest string, releases map[string]selfRelease) *fakeOutpostReleases {
	t.Helper()
	f := &fakeOutpostReleases{latest: latest, releases: releases}
	asset := fmt.Sprintf("outpost-%s-%s", runtime.GOOS, runtime.GOARCH)
	var srv *httptest.Server
	release := func(w http.ResponseWriter, tag string) {
		rel, ok := f.releases[tag]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assets := []map[string]any{{
			"name":                 asset,
			"browser_download_url": srv.URL + "/dl/" + tag + "/" + asset,
			"size":                 len(rel.binary),
		}}
		if !rel.noChecksums {
			assets = append(assets, map[string]any{
				"name":                 "SHA256SUMS",
				"browser_download_url": srv.URL + "/dl/" + tag + "/SHA256SUMS",
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": assets})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/sroberts/outpost/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		release(w, f.latest)
	})
	mux.HandleFunc("/repos/sroberts/outpost/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests++
		release(w, r.PathValue("tag"))
	})
	mux.HandleFunc("/dl/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rel := f.releases[r.PathValue("tag")]
		if r.PathValue("name") != "SHA256SUMS" {
			_, _ = w.Write(rel.binary)
			return
		}
		sum := sha256.Sum256(rel.binary)
		hash := hex.EncodeToString(sum[:])
		if rel.wrongSum {
			hash = strings.Repeat("0", 64)
		}
		_, _ = fmt.Fprintf(w, "%s  %s\n", hash, asset)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useFakeGitHub(t, srv)
	return f
}

func (f *fakeOutpostReleases) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// fakePing is a daemon reduced to answering ping on the home's socket, which
// is all update --self asks of it. Each start is a new run, with its own PID
// and start time; the PIDs are ones no process has.
type fakePing struct {
	t    *testing.T
	path string
	mu   sync.Mutex
	ln   net.Listener
	id   ipc.DaemonIdentity
	runs int
}

func newFakePing(t *testing.T, home string) *fakePing {
	p := &fakePing{t: t, path: filepath.Join(home, "outpost.sock")}
	t.Cleanup(p.stop)
	return p
}

func (p *fakePing) start(build string) {
	p.t.Helper()
	p.stop()
	ln, err := net.Listen("unix", p.path)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.runs++
	p.ln = ln
	p.id = ipc.DaemonIdentity{Version: build, PID: 3_999_000 + p.runs, Started: time.Now()}
	id := p.id
	p.mu.Unlock()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.ReadAll(conn)
			_ = json.NewEncoder(conn).Encode(ipc.Response{OK: true, Message: "pong", Version: id.Version, PID: id.PID, Started: id.Started})
			_ = conn.Close()
		}
	}()
}

func (p *fakePing) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}
	_ = os.Remove(p.path)
}

// selfFixture is a home whose daemon runs from an installed job, a binary of
// its own on disk, and a fake daemon on the socket.
type selfFixture struct {
	t    *testing.T
	home string
	bin  string
	stub *initStub
	ping *fakePing
	mgr  autostart.Manager
	cfg  autostart.Config

	mu       sync.Mutex
	restarts int
	// onRestart decides what the nth restart does (from 1): whether a new
	// daemon comes up, and an error for the restart command to fail with.
	// The default brings up a daemon reporting the binary now on disk.
	onRestart func(n int) (up bool, err error)
}

const oldBuild = "v1.0.0"

func newSelfFixture(t *testing.T) *selfFixture {
	t.Helper()
	f := &selfFixture{t: t, stub: stubInit(t, true), home: shortHome(t)}
	f.bin = filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(f.bin, outpostScript(oldBuild), 0o755); err != nil {
		t.Fatal(err)
	}
	f.bin, _ = filepath.EvalSymlinks(f.bin)

	oldExe, oldBuildFn, oldLabel := selfExecutable, selfBuild, selfLabel
	t.Cleanup(func() { selfExecutable, selfBuild, selfLabel = oldExe, oldBuildFn, oldLabel })
	selfExecutable = func() (string, error) { return f.bin, nil }
	selfBuild = func() version.Info { return version.Info{Version: oldBuild, Commit: "0123456789abcdef"} }
	selfLabel = testLabel

	mgr, err := autostart.ForHost()
	if err != nil {
		t.Skip(err)
	}
	f.mgr = mgr
	f.cfg = autostart.Config{Label: testLabel, Binary: f.bin, Home: f.home}

	f.ping = newFakePing(t, f.home)
	f.stub.onRun = func(argv []string) error {
		if !swapsTheDaemon(argv) {
			return nil
		}
		f.mu.Lock()
		f.restarts++
		n := f.restarts
		f.mu.Unlock()
		up, err := true, error(nil)
		if f.onRestart != nil {
			up, err = f.onRestart(n)
		}
		f.ping.stop()
		if up {
			f.ping.start(f.onDisk() + " (0123456789ab)")
		}
		return err
	}
	return f
}

// installJob writes the job, running the fixture's binary.
func (f *selfFixture) installJob() {
	f.t.Helper()
	path, err := f.mgr.UnitPath(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	body, err := f.mgr.Render(f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// onDisk is the release the binary on disk says it is.
func (f *selfFixture) onDisk() string {
	f.t.Helper()
	v, err := probeBinaryVersion(f.bin)
	if err != nil {
		f.t.Fatalf("probe %s: %v", f.bin, err)
	}
	return v
}

func (f *selfFixture) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

func (f *selfFixture) update(args ...string) (string, string, error) {
	f.t.Helper()
	return runCLI(f.t, f.home, append([]string{"update", "--self"}, args...)...)
}

// untouched fails the test if anything was changed or restarted.
func (f *selfFixture) untouched() {
	f.t.Helper()
	if got := readFile(f.t, f.bin); got != string(outpostScript(oldBuild)) {
		f.t.Errorf("the binary was replaced:\n%s", got)
	}
	for _, p := range []string{f.bin + ".previous", filepath.Join(filepath.Dir(f.bin), ".outpost-selfupdate-new")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			f.t.Errorf("%s was left behind (%v)", p, err)
		}
	}
	if n := f.restartCount(); n != 0 {
		f.t.Errorf("the daemon was restarted %d times", n)
	}
}

// writeAuthDB gives the home a user, so there is something to snapshot.
func writeAuthDB(t *testing.T, home string) {
	t.Helper()
	s, err := auth.Open(filepath.Join(home, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
}

func snapshots(t *testing.T, home string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(home, "backups", "auth-"+oldBuild+"-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestUpdateSelf_InstallsTheNewReleaseAndKeepsThePrevious(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	writeAuthDB(t, f.home)
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	out, errOut, err := f.update()
	if err != nil {
		t.Fatalf("update --self: %v\n%s%s", err, out, errOut)
	}
	if got := readFile(t, f.bin); got != string(outpostScript("v1.1.0")) {
		t.Errorf("binary is now:\n%s", got)
	}
	if got := readFile(t, f.bin+".previous"); got != string(outpostScript(oldBuild)) {
		t.Errorf("previous is:\n%s", got)
	}
	for _, want := range []string{"Outpost is running v1.1.0 (was v1.0.0)", f.bin + ".previous", "checksum verified", "Every service stops"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := f.restartCount(); n != 1 {
		t.Errorf("restarted %d times; want 1", n)
	}
	snaps := snapshots(t, f.home)
	if len(snaps) != 1 {
		t.Fatalf("auth.db snapshots: %v", snaps)
	}
	if fi, err := os.Stat(snaps[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("snapshot %s: %v %v", snaps[0], fi.Mode(), err)
	}
	if !strings.Contains(out, snaps[0]) {
		t.Errorf("output doesn't name the snapshot:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.bin), ".outpost-selfupdate-new")); !os.IsNotExist(err) {
		t.Errorf("the staged download was left behind: %v", err)
	}
}

// A loaded job whose daemon isn't answering, crash-looping say, is exactly
// one worth updating; the first daemon to answer afterwards is the new one.
func TestUpdateSelf_ProceedsWithNoDaemonAnswering(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	out, errOut, err := f.update()
	if err != nil {
		t.Fatalf("update --self: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, "Outpost is running v1.1.0") || !strings.Contains(out, "no auth.db") {
		t.Errorf("output:\n%s", out)
	}
}

func TestUpdateSelf_UpToDate(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, oldBuild, map[string]selfRelease{oldBuild: {binary: outpostScript(oldBuild)}})

	out, _, err := f.update()
	if err != nil || !strings.Contains(out, "already at v1.0.0") {
		t.Errorf("err = %v, out:\n%s", err, out)
	}
	f.untouched()
}

func TestUpdateSelf_CheckChangesNothing(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	out, _, err := f.update("--check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v1.0.0 installed, v1.1.0 available") || !strings.Contains(out, "outpost update --self") {
		t.Errorf("output:\n%s", out)
	}
	f.untouched()
	if len(f.stub.commands()) != 0 {
		t.Errorf("ran %v", f.stub.commands())
	}
}

// GitHub's latest skips prereleases, so after installing one by tag it names
// an older release. That is not an update.
func TestUpdateSelf_LatestOlderThanInstalledChangesNothing(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v0.9.0", map[string]selfRelease{"v0.9.0": {binary: outpostScript("v0.9.0")}})

	out, _, err := f.update()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "newer than its latest release, v0.9.0") || !strings.Contains(out, "--self --to v0.9.0") {
		t.Errorf("output:\n%s", out)
	}
	f.untouched()
}

func TestUpdateSelf_GoingBackWarns(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, oldBuild, map[string]selfRelease{"v0.9.0": {binary: outpostScript("v0.9.0")}})

	out, errOut, err := f.update("--to", "v0.9.0")
	if err != nil {
		t.Fatalf("update --self --to: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(errOut, "going back from v1.0.0 to v0.9.0") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if !strings.Contains(out, "Outpost is running v0.9.0") {
		t.Errorf("output:\n%s", out)
	}
}

func TestUpdateSelf_RequiresChecksums(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0"), noChecksums: true}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "no checksums") || !strings.Contains(err.Error(), "--allow-unverified") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()

	out, errOut, err := f.update("--allow-unverified")
	if err != nil {
		t.Fatalf("--allow-unverified: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, "unverified") || !strings.Contains(out, "Outpost is running v1.1.0") {
		t.Errorf("output:\n%s", out)
	}
}

func TestUpdateSelf_ChecksumMismatchChangesNothing(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0"), wrongSum: true}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
}

// A build between tags reads as a prerelease of a real version, which would
// make "newer" and "already at" both wrong.
func TestUpdateSelf_RefusesADevelopmentBuild(t *testing.T) {
	for _, build := range []version.Info{
		{Version: "v1.0.0-3-gabc1234-dirty"},
		{Version: "v1.0.0", Commit: "0123456789abcdef", Dirty: true},
		{Version: "dev"},
	} {
		f := newSelfFixture(t)
		f.installJob()
		selfBuild = func() version.Info { return build }
		gh := newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

		_, _, err := f.update()
		if err == nil || !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), "--to") {
			t.Errorf("%s: err = %v", build, err)
		}
		f.untouched()
		if gh.asked() != 0 {
			t.Errorf("%s: asked GitHub %d times", build, gh.asked())
		}

		out, _, err := f.update("--check")
		if err != nil || !strings.Contains(out, "development build") || !strings.Contains(out, "v1.1.0") {
			t.Errorf("%s --check: err = %v, out:\n%s", build, err, out)
		}
		f.untouched()
	}
}

func TestUpdateSelf_RefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newSelfFixture(t)
	f.installJob()
	gh := newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	dir := filepath.Dir(f.bin)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "can't be written") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
	if gh.asked() != 0 {
		t.Errorf("asked GitHub %d times", gh.asked())
	}
}

func TestRefuseReplace(t *testing.T) {
	for _, path := range []string{
		"/opt/homebrew/bin/outpost",
		"/usr/local/Cellar/outpost/1.0.0/bin/outpost",
		"/home/linuxbrew/.linuxbrew/bin/outpost",
		"/nix/store/abc123-outpost-1.0.0/bin/outpost",
		"/usr/bin/outpost",
		"/bin/outpost",
	} {
		if err := refuseReplace(path); err == nil || !strings.Contains(err.Error(), "package manager") {
			t.Errorf("%s: %v", path, err)
		}
	}
	// A bin directory of one's own isn't a package manager's for having
	// "/bin/" in it.
	if err := refuseReplace("/opt/tools/bin/outpost-absent"); err == nil || strings.Contains(err.Error(), "package manager") {
		t.Errorf("/opt/tools/bin: %v", err)
	}
	mine := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(mine, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := refuseReplace(mine); err != nil {
		t.Errorf("%s: %v", mine, err)
	}
}

// The release compared against is this CLI's, so a daemon running another
// binary would be "updated" on the strength of the wrong version.
func TestUpdateSelf_RefusesWhenTheJobRunsAnotherBinary(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	other := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(other, outpostScript(oldBuild), 0o755); err != nil {
		t.Fatal(err)
	}
	selfExecutable = func() (string, error) { return other, nil }
	gh := newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "the daemon runs "+f.bin) || !strings.Contains(err.Error(), f.bin+" update --self") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
	if got := readFile(t, other); got != string(outpostScript(oldBuild)) {
		t.Errorf("the CLI's binary was replaced")
	}
	if gh.asked() != 0 {
		t.Errorf("asked GitHub %d times", gh.asked())
	}
}

// Nothing could start the new binary without a job, so nothing is replaced.
func TestUpdateSelf_RefusesWithNoJob(t *testing.T) {
	f := newSelfFixture(t)
	f.ping.start(oldBuild)
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "no autostart job") || !strings.Contains(err.Error(), "outpost install --enable") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
}

// Restarting the daemon would end a service's update where it stands.
func TestUpdateSelf_RefusesDuringAServiceUpdate(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(f.home, "bin", "svc"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	for _, args := range [][]string{nil, {"--force"}} {
		_, _, err = f.update(args...)
		if err == nil || !strings.Contains(err.Error(), "an update is running (svc)") || !strings.Contains(err.Error(), "Wait for it") {
			t.Errorf("%v: err = %v", args, err)
		}
		f.untouched()
	}
}

// Two at once would each keep the other's binary as the one to roll back to.
func TestUpdateSelf_RefusesASecondUpdateSelf(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(selfUpdateLock(f.home))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, _, err = f.update()
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
}

// A download that won't run here, built for another platform say, is caught
// before any service has stopped.
func TestUpdateSelf_StagedBinaryThatDoesntRunChangesNothing(t *testing.T) {
	for name, binary := range map[string][]byte{
		"fails":         []byte("#!/bin/sh\nexit 1\n"),
		"wrong release": outpostScript("v9.9.9"),
	} {
		f := newSelfFixture(t)
		f.installJob()
		f.ping.start(oldBuild)
		newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: binary}})

		_, _, err := f.update()
		if err == nil || !strings.Contains(err.Error(), "doesn't run here") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Errorf("%s: err = %v", name, err)
		}
		f.untouched()
		if len(f.stub.commands()) != 0 {
			t.Errorf("%s: ran %v", name, f.stub.commands())
		}
	}
}

// A daemon that comes back as the old release didn't take the new binary;
// the update hasn't happened, and the old binary goes back.
func TestUpdateSelf_RollsBackWhenTheDaemonReportsTheOldRelease(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	writeAuthDB(t, f.home)
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	f.stub.onRun = func(argv []string) error {
		if swapsTheDaemon(argv) {
			f.mu.Lock()
			f.restarts++
			f.mu.Unlock()
			f.ping.start(oldBuild + " (0123456789ab)")
		}
		return nil
	}

	out, _, err := f.update()
	if err == nil {
		t.Fatalf("reported success:\n%s", out)
	}
	for _, want := range []string{"came back as v1.0.0", "Rolled back: the daemon is running v1.0.0", snapshots(t, f.home)[0]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary after rollback:\n%s", got)
	}
	if n := f.restartCount(); n != 2 {
		t.Errorf("restarted %d times; want 2", n)
	}
}

func TestUpdateSelf_RollsBackWhenNoDaemonAnswers(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	f.onRestart = func(n int) (bool, error) { return n > 1, nil }

	out, _, err := f.update("--wait", "100ms")
	if err == nil {
		t.Fatalf("reported success:\n%s", out)
	}
	for _, want := range []string{"no new one answered", "Rolled back: the daemon is running v1.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary after rollback:\n%s", got)
	}
}

func TestUpdateSelf_RollbackWhoseRestartFailsSaysSo(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	writeAuthDB(t, f.home)
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	f.onRestart = func(n int) (bool, error) {
		if n > 1 {
			return false, errors.New("launchctl said no")
		}
		return false, nil
	}

	out, _, err := f.update("--wait", "100ms")
	if err == nil {
		t.Fatalf("reported success:\n%s", out)
	}
	for _, want := range []string{"restarting the daemon on it failed", "launchctl said no", "probably down",
		f.mgr.LogHint(f.cfg), snapshots(t, f.home)[0]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary after rollback:\n%s", got)
	}
}

func TestUpdateSelf_RefusesWhatIsAboutServices(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	gh := newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	for _, args := range [][]string{
		{"svc"},
		{"--all"},
		{"--from", "o/r"},
		{"--no-backup"},
	} {
		_, _, err := f.update(args...)
		if err == nil || !strings.Contains(err.Error(), "--self") {
			t.Errorf("update --self %v: err = %v", args, err)
		}
	}
	f.untouched()
	if gh.asked() != 0 {
		t.Errorf("a refused command asked GitHub %d times", gh.asked())
	}
}

func TestBuildRelease(t *testing.T) {
	for _, c := range []struct {
		in    string
		tag   string
		clean bool
	}{
		{"v0.1.0 (abc123def456)", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", true},
		{"v1.0.0-rc.1 (abc123def456)", "v1.0.0-rc.1", true},
		{"v0.1.0 (abc123def456-dirty)", "v0.1.0", false},
		{"v0.1.0 (dirty)", "v0.1.0", false},
		{"dev", "dev", false},
		{"dev (abc123def456)", "dev", false},
		{"v0.1.0-3-gabc1234 (abc123def456)", "v0.1.0-3-gabc1234", false},
		{"v0.1.0-3-gabc1234-dirty", "v0.1.0-3-gabc1234-dirty", false},
		{"v0.0.0-20260102030405-abcdef123456", "v0.0.0-20260102030405-abcdef123456", false},
		{"v1.2.4-0.20260102030405-abcdef123456", "v1.2.4-0.20260102030405-abcdef123456", false},
		{"v0.0.0-20260102030405-abcdef123456+dirty", "v0.0.0-20260102030405-abcdef123456+dirty", false},
		{"abc1234", "abc1234", false},
		{"", "", false},
	} {
		tag, clean := buildRelease(c.in)
		if tag != c.tag || clean != c.clean {
			t.Errorf("buildRelease(%q) = %q, %v; want %q, %v", c.in, tag, clean, c.tag, c.clean)
		}
	}
}

func TestParseVersionOutput(t *testing.T) {
	long := version.Info{Version: "v1.2.3", Commit: "0123456789abcdef", Go: "go1.26", OS: "linux", Arch: "arm64"}
	if got, err := parseVersionOutput([]byte(long.Long())); err != nil || got != "v1.2.3" {
		t.Errorf("clean: %q, %v", got, err)
	}
	long.Dirty = true
	if got, err := parseVersionOutput([]byte(long.Long())); err != nil || got != "v1.2.3 (dirty)" {
		t.Errorf("dirty: %q, %v", got, err)
	}
	for _, bad := range []string{"", "hello world\n", "outpost\n"} {
		if got, err := parseVersionOutput([]byte(bad)); err == nil {
			t.Errorf("%q: got %q, no error", bad, got)
		}
	}
}
