package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/version"
)

// Another home whose daemon runs the same binary would share the staged and
// .previous files beside it, so its update holds this one off.
func TestUpdateSelf_RefusesWhileTheBinaryIsBeingUpdated(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	unlock, err := lockUpdate(f.bin)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, _, err = f.update()
	if err == nil || !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), f.bin) {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
}

// A service whose binary happens to be called outpost-self is a service, and
// its update is not mistaken for this one's own lock.
func TestUpdateSelf_AServiceCalledOutpostSelfIsAService(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	if err := os.MkdirAll(filepath.Join(f.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(filepath.Join(f.home, "bin", "outpost-self"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, _, err = f.update()
	if err == nil || !strings.Contains(err.Error(), "an update is running (outpost-self)") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
}

// --force is for reinstalling; a manifest the new daemon would refuse leaves
// every service down whatever the flags say.
func TestUpdateSelf_InvalidManifestRefusedEvenWithForce(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	if err := os.WriteFile(filepath.Join(f.home, "services", "broken.toml"), []byte("name = \"broken\"\nport = \"nope\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--force"}} {
		_, _, err := f.update(args...)
		if err == nil || !strings.Contains(err.Error(), "refuse to start") || !strings.Contains(err.Error(), "Fix the manifest") {
			t.Errorf("%v: err = %v", args, err)
		}
		f.untouched()
	}
}

// daemon restart would end an update of Outpost itself mid-swap, so it says
// that is what's running.
func TestDaemonRestart_RefusesDuringUpdateSelf(t *testing.T) {
	stub := stubInit(t, true)
	home := shortHome(t)
	writeJob(t, home)
	unlock, err := lockFile(selfUpdateLock(home))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, err = restart(t, home)
	if err == nil || !strings.Contains(err.Error(), "an update is running (Outpost itself)") {
		t.Fatalf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

// A service update mustn't start while the daemon is being restarted, by the
// update or by its rollback.
func TestUpdateSelf_HoldsServiceLocksThroughTheRollback(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	svc := filepath.Join(f.home, "bin", "svc")
	if err := os.MkdirAll(filepath.Dir(svc), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockUpdate(svc) // leaves the lock file for holdUpdateLocks to find
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	var gotIn []int
	f.onRestart = func(n int) (bool, error) {
		if u, err := lockUpdate(svc); err == nil {
			u()
			gotIn = append(gotIn, n)
		}
		return n > 1, nil
	}

	_, _, err = f.update("--wait", "100ms")
	if err == nil || !strings.Contains(err.Error(), "Rolled back") {
		t.Fatalf("err = %v", err)
	}
	if n := f.restartCount(); n != 2 {
		t.Fatalf("restarted %d times; want 2", n)
	}
	if len(gotIn) != 0 {
		t.Errorf("a service update could start during restarts %v", gotIn)
	}
	u, err := lockUpdate(svc)
	if err != nil {
		t.Errorf("the service lock is still held after update --self returned: %v", err)
	} else {
		u()
	}
}

// A build from a modified tree that names the right tag is not that release.
func TestUpdateSelf_RefusesADirtyStagedBinary(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild)
	dirty := []byte("#!/bin/sh\necho 'outpost v1.1.0'\necho '  commit:   0123456789ab (working tree had uncommitted changes)'\n")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: dirty}})

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "doesn't run here") || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("err = %v", err)
	}
	f.untouched()
	if len(f.stub.commands()) != 0 {
		t.Errorf("ran %v", f.stub.commands())
	}
}

func TestUpdateSelf_RollsBackWhenTheNewDaemonIsDirty(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	f.stub.onRun = func(argv []string) error {
		if !swapsTheDaemon(argv) {
			return nil
		}
		f.mu.Lock()
		f.restarts++
		f.mu.Unlock()
		f.ping.start(f.onDisk() + " (0123456789ab-dirty)")
		return nil
	}

	_, _, err := f.update()
	if err == nil || !strings.Contains(err.Error(), "came back as v1.1.0 (0123456789ab-dirty)") || !strings.Contains(err.Error(), "Rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, f.bin); got != string(outpostScript(oldBuild)) {
		t.Errorf("binary after rollback:\n%s", got)
	}
}

// Old copies hold credentials since changed, so only the newest two stay, and
// nothing that isn't one of them is touched.
func TestUpdateSelf_KeepsTheTwoNewestAuthSnapshots(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	writeAuthDB(t, f.home)
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	backups := filepath.Join(f.home, "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"auth-v0.8.0-20240101T000000Z.db",
		"auth-v0.9.0-20250101T000000Z.db",
		"auth-v0.9.1-rc.1-20250601T000000Z.db",
		"auth-notes.db",
		"auth-v0.7.0-20230101T000000Z.db.bak",
		"auth-v0.7.0-20230101T000000Z.db.keep",
		"other-v0.7.0-20230101T000000Z.db",
	} {
		if err := os.WriteFile(filepath.Join(backups, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, errOut, err := f.update()
	if err != nil {
		t.Fatalf("update --self: %v\n%s%s", err, out, errOut)
	}
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	newest := filepath.Base(snapshots(t, f.home)[0])
	want := []string{
		"auth-notes.db",
		"auth-v0.7.0-20230101T000000Z.db.bak",
		"auth-v0.7.0-20230101T000000Z.db.keep",
		"auth-v0.9.1-rc.1-20250601T000000Z.db",
		newest,
		"other-v0.7.0-20230101T000000Z.db",
	}
	sort.Strings(want)
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("backups:\n  %v\nwant\n  %v", names, want)
	}
}

// --check with --to is about the named release, which may be older.
func TestUpdateSelf_CheckWordsANamedRelease(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{
		"v0.9.0": {binary: outpostScript("v0.9.0")},
		"v1.1.0": {binary: outpostScript("v1.1.0")},
	})

	out, _, err := f.update("--check", "--to", "v0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "would go back") || strings.Contains(out, "available") {
		t.Errorf("older --to:\n%s", out)
	}

	out, _, err = f.update("--check", "--to", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "release v1.1.0 available") || !strings.Contains(out, "--self --to v1.1.0") {
		t.Errorf("newer --to:\n%s", out)
	}

	selfBuild = func() version.Info { return version.Info{Version: "dev"} }
	out, _, err = f.update("--check", "--to", "v0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "release v0.9.0 is available") || strings.Contains(out, "latest") {
		t.Errorf("dev build, --to:\n%s", out)
	}
	f.untouched()
}
