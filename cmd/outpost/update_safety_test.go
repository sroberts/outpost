package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareReleases(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.3", "1.2.3", 0, true},
		{"v1.10.0", "v1.9.0", 1, true},
		{"v2.0.0-rc1", "v1.9.0", 1, true},
		{"v2.0.0-rc1", "v2.0.0", -1, true},
		{"v2.0.0-rc.2", "v2.0.0-rc.10", -1, true},
		{"v2.0.0-alpha", "v2.0.0-1", 1, true},
		{"v2.0.0-alpha", "v2.0.0-alpha.1", -1, true},
		{"v1.2", "v1.2.0", 0, true},
		{"v1.2.3+build.5", "v1.2.3", 0, true},
		{"nightly", "v1.0.0", 0, false},
		{"", "v1.0.0", 0, false},
		{"v1.2.3.4", "v1.2.3", 0, false},
	}
	for _, tc := range cases {
		got, ok := compareReleases(tc.a, tc.b)
		if got != tc.want || ok != tc.ok {
			t.Errorf("compareReleases(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
}

// GitHub's latest release skips prereleases, so after installing one the
// latest can be older. A plain update must not quietly go back to it.
func TestUpdate_DoesNotGoBackToAnOlderLatest(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("rc"), freePort(t), "v2.0.0-rc1")
	before := readFile(t, path)
	useFakeGitHub(t, fakeGitHub(t, []byte("older"), "", true)) // latest is v1.2.3

	stdout, _, err := runCLI(t, home, "update", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "newer than o/r's latest release, v1.2.3") || !strings.Contains(stdout, "--to v1.2.3") {
		t.Errorf("stdout = %q", stdout)
	}
	if readFile(t, path) != before || readFile(t, filepath.Join(home, "bin", "svc")) != "rc" {
		t.Error("update went back to the older release")
	}

	stdout, _, err = runCLI(t, home, "update", "svc", "--check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "newer than") || strings.Contains(stdout, "available") {
		t.Errorf("--check = %q", stdout)
	}
	stdout, _, err = runCLI(t, home, "update", "--check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "older than installed") {
		t.Errorf("--check over every service = %q", stdout)
	}
}

func TestUpdate_RejectsANegativeWait(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	_, _, err := runCLI(t, home, "update", "svc", "--wait", "-1s")
	if err == nil || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetRelease_QuotedKeysAndComments(t *testing.T) {
	in := "name = \"a\"\n\"source\" = \"x/y\"\n'release' = \"v1\"  # pinned \"for now\"\n"
	want := "name = \"a\"\nsource         = \"o/r\"\nrelease        = \"v2\"  # pinned \"for now\"\n"
	got := setRelease([]byte(in), "o/r", "v2")
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if err := checkRecorded(got, "o/r", "v2"); err != nil {
		t.Error(err)
	}
}

// A manifest the line edit can't change cleanly stops the update before
// anything is touched.
func TestUpdate_UneditableManifestChangesNothing(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "")
	body := readFile(t, path) + "source = \"o/r\"\nrelease = \"\"\"\nv1.0.0\"\"\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true))

	_, _, err := runCLI(t, home, "update", "svc")
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if readFile(t, path) != body || readFile(t, filepath.Join(home, "bin", "svc")) != "old" {
		t.Error("an uneditable manifest still changed something")
	}
}

func TestReplaceFile_KeepsSymlinkAndMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles.toml")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "svc.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the manifest is no longer a symlink: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Errorf("target = %q", got)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, %v; want 0644 kept", fi.Mode().Perm(), err)
	}
}

func TestLockUpdate_OneAtATime(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "svc")
	unlock, err := lockUpdate(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockUpdate(bin); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock: err = %v", err)
	}
	unlock()
	again, err := lockUpdate(bin)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}

// A stopped service is rolled back too when the daemon won't load the update,
// rather than left on a binary the manifest no longer matches.
func TestUpdate_StoppedServiceRollsBackWhenApplyFails(t *testing.T) {
	home, c, original := daemonHome(t)
	if _, err := c.Stop("svc"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "services", "svc.toml")
	manifestBefore := readFile(t, path)
	// Another manifest the daemon refuses makes its Apply fail.
	if err := os.WriteFile(filepath.Join(home, "services", "bad.toml"), []byte("name = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true))

	_, _, err := runCLI(t, home, "update", "svc", "--no-backup")
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v; want a rollback", err)
	}
	if !bytes.Equal([]byte(readFile(t, filepath.Join(home, "bin", "svc"))), original) {
		t.Error("the old binary was not put back")
	}
	if got := readFile(t, path); got != manifestBefore {
		t.Errorf("manifest not restored:\n%s", got)
	}
}

func TestSameRelease(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.2.0", "v1.2.0", true},
		{"1.2.0", "v1.2.0", true},
		{"v1.2.0+a", "v1.2.0+b", false},
		{"v1.2.0", "v1.2.1", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := sameRelease(tc.a, tc.b); got != tc.want {
			t.Errorf("sameRelease(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// A manifest that spells the tag without its v is still up to date.
func TestUpdate_TagSpelledWithoutVIsCurrent(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("old"), freePort(t), "1.2.3")
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true)) // latest is v1.2.3

	stdout, _, err := runCLI(t, home, "update", "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "already at") || readFile(t, filepath.Join(home, "bin", "svc")) != "old" {
		t.Errorf("stdout = %q; the same version was reinstalled", stdout)
	}
}

// With its binary gone there is nothing to roll back to, so an update needs
// --force, and then it is how the binary gets put back.
func TestUpdate_MissingBinaryNeedsForce(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	bin := filepath.Join(home, "bin", "svc")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true))

	_, _, err := runCLI(t, home, "update", "svc")
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v; want it to ask for --force", err)
	}
	stdout, _, err := runCLI(t, home, "update", "svc", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, bin); got != "new" {
		t.Errorf("binary = %q", got)
	}
	if _, err := os.Stat(bin + ".previous"); !os.IsNotExist(err) {
		t.Errorf("a .previous was made from no binary: %v", err)
	}
	if strings.Contains(stdout, "previous binary is kept") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestKeepPrevious(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "svc")
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := bin + ".previous"
	if err := os.WriteFile(prev, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := keepPrevious(bin, prev); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, prev); got != "v1" {
		t.Errorf("previous = %q; want the current binary", got)
	}
}

// Where hard links aren't possible the previous binary is copied instead.
func TestKeepPrevious_CopiesWithoutLinks(t *testing.T) {
	prev := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: os.ErrPermission} }
	t.Cleanup(func() { linkFile = prev })

	dir := t.TempDir()
	bin := filepath.Join(dir, "svc")
	if err := os.WriteFile(bin, []byte("v1"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := keepPrevious(bin, bin+".previous"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, bin+".previous"); got != "v1" {
		t.Errorf("previous = %q", got)
	}
	if fi, err := os.Stat(bin + ".previous"); err != nil || fi.Mode().Perm() != 0o751 {
		t.Errorf("mode = %v, %v; want 0751", fi.Mode().Perm(), err)
	}
}
