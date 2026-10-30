package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	outpostruntime "github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeRepos is GitHub with several repositories under o/, each with its own
// latest release, binary and etag, for updates that differ between services.
type fakeRepos struct {
	srv *httptest.Server

	mu      sync.Mutex
	repos   map[string]fakeRepo
	full    int // answered with the release
	unmoved int // answered 304
}

type fakeRepo struct {
	tag    string
	binary []byte
}

func newFakeRepos(t *testing.T, repos map[string]fakeRepo) *fakeRepos {
	t.Helper()
	f := &fakeRepos{repos: repos}
	asset := fmt.Sprintf("svc-%s-%s", runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/{repo}/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		repo, ok := f.repos[r.PathValue("repo")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		etag := `"` + repo.tag + `"`
		if r.Header.Get("If-None-Match") == etag {
			f.unmoved++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.full++
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": repo.tag,
			"assets": []map[string]any{{
				"name":                 asset,
				"browser_download_url": f.srv.URL + "/dl/" + r.PathValue("repo"),
				"size":                 len(repo.binary),
			}},
		})
	})
	mux.HandleFunc("/repos/o/{repo}/contents/outpost.toml", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/dl/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = w.Write(f.repos[r.PathValue("repo")].binary)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	useFakeGitHub(t, f.srv)
	return f
}

func (f *fakeRepos) counts() (full, unmoved int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.full, f.unmoved
}

// installFrom declares a service installed at a release from o/<repo>.
func installFrom(t *testing.T, home, name, repo string, binary []byte, release string) string {
	t.Helper()
	path := declareInstalled(t, home, name, binary, freePort(t), release)
	body := strings.Replace(readFile(t, path), `"o/r"`, fmt.Sprintf("%q", "o/"+repo), 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func releaseOf(t *testing.T, path string) string {
	t.Helper()
	svc, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return svc.Release
}

func TestUpdateAll_RefusesWhatIsAboutOneService(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	f := newFakeRepos(t, nil)
	for _, args := range [][]string{
		{"--all", "svc"},
		{"svc", "--all"},
		{"--all", "--to", "v1.0.0"},
		{"--all", "--from", "o/r"},
		{"--all", "--force"},
		{"--all", "--check", "svc"},
	} {
		_, _, err := runCLI(t, home, append([]string{"update"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "--all") {
			t.Errorf("update %v: err = %v; want a refusal naming --all", args, err)
		}
	}
	if full, _ := f.counts(); full != 0 {
		t.Errorf("a refused command asked GitHub %d times", full)
	}
}

// --check lists everything, like `update --check`, and changes nothing.
func TestUpdateAll_CheckListsThemAll(t *testing.T) {
	home := t.TempDir()
	path := installFrom(t, home, "aaa", "a", []byte("old"), "v1.0.0")
	installFrom(t, home, "bbb", "b", []byte("old"), "v2.0.0")
	declareInstalled(t, home, "ccc", []byte("old"), freePort(t), "")
	newFakeRepos(t, map[string]fakeRepo{
		"a": {"v2.0.0", []byte("new")},
		"b": {"v2.0.0", []byte("new")},
	})

	stdout, stderr, err := runCLI(t, home, "update", "--all", "--check")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{"aaa", "bbb", "ccc", "(up to date)", "not installed from a release"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if got := releaseOf(t, path); got != "v1.0.0" {
		t.Errorf("--check installed %s", got)
	}
}

// Only what has something newer gets touched, and each thing left alone is
// accounted for.
func TestUpdateAll_UpdatesWhatIsNewerAndSkipsTheRest(t *testing.T) {
	home := t.TempDir()
	older := installFrom(t, home, "older", "a", []byte("old"), "v1.0.0")
	same := installFrom(t, home, "same", "a", []byte("old"), "v2.0.0")
	ahead := installFrom(t, home, "ahead", "a", []byte("old"), "v3.0.0-rc1")
	elsewhere := installFrom(t, home, "elsewhere", "a", []byte("old"), "v1.0.0")
	declareInstalled(t, home, "byhand", []byte("old"), freePort(t), "")
	body := strings.Replace(readFile(t, elsewhere), filepath.Join(home, "bin", "elsewhere"), "/usr/bin/true", 1)
	if err := os.WriteFile(elsewhere, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	newFakeRepos(t, map[string]fakeRepo{"a": {"v2.0.0", []byte("new")}})

	stdout, stderr, err := runCLI(t, home, "update", "--all", "--no-backup")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, stdout, stderr)
	}
	if got := readFile(t, filepath.Join(home, "bin", "older")); got != "new" {
		t.Errorf("older's binary = %q", got)
	}
	for path, want := range map[string]string{older: "v2.0.0", same: "v2.0.0", ahead: "v3.0.0-rc1", elsewhere: "v1.0.0"} {
		if got := releaseOf(t, path); got != want {
			t.Errorf("%s records %s; want %s", filepath.Base(path), got, want)
		}
	}
	for _, want := range []string{
		"Updated: older (v1.0.0 to v2.0.0)",
		"Skipped: ",
		"ahead: Newer than the latest release",
		"elsewhere: v2.0.0 is available, but",
		"No source to update from: byhand",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "same") {
		t.Errorf("up to date services need no mention:\n%s", stdout)
	}
}

func TestUpdateAll_NothingToDo(t *testing.T) {
	home := t.TempDir()
	installFrom(t, home, "same", "a", []byte("old"), "v2.0.0")
	newFakeRepos(t, map[string]fakeRepo{"a": {"v2.0.0", []byte("new")}})
	stdout, _, err := runCLI(t, home, "update", "--all")
	if err != nil || !strings.Contains(stdout, "Nothing to update: 1 up to date") {
		t.Errorf("err = %v, stdout:\n%s", err, stdout)
	}
}

// Not being able to ask about one doesn't stop the others, but it isn't
// success either: the operator was told "all", and it wasn't.
func TestUpdateAll_FailedCheckIsReportedAndNonZero(t *testing.T) {
	home := t.TempDir()
	lost := installFrom(t, home, "lost", "gone", []byte("old"), "v1.0.0")
	fine := installFrom(t, home, "zfine", "a", []byte("old"), "v1.0.0")
	newFakeRepos(t, map[string]fakeRepo{"a": {"v2.0.0", []byte("new")}})

	stdout, _, err := runCLI(t, home, "update", "--all", "--no-backup")
	if err == nil || !strings.Contains(err.Error(), "couldn't check 1") {
		t.Errorf("err = %v", err)
	}
	if got := releaseOf(t, fine); got != "v2.0.0" {
		t.Errorf("zfine records %s", got)
	}
	if got := releaseOf(t, lost); got != "v1.0.0" {
		t.Errorf("lost records %s", got)
	}
	for _, want := range []string{"Updated: zfine", "Not tried, the check failed: lost ("} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
}

// The heart of it, against a real daemon and real processes: the first updates,
// the second rolls back, the third is left alone, and the command fails.
func TestUpdateAll_StopsAtTheFirstThatFails(t *testing.T) {
	home, err := os.MkdirTemp("", "op") // sockets need a short path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a temp dir; the OS reaps what's left
	original, err := os.ReadFile(fakeservice(t))
	if err != nil {
		t.Fatal(err)
	}
	first := installFrom(t, home, "afirst", "good", original, "v1.0.0")
	second := installFrom(t, home, "bsecond", "bad", original, "v1.0.0")
	third := installFrom(t, home, "cthird", "good", original, "v1.0.0")

	prev := updatePoll
	updatePoll = 50 * time.Millisecond
	t.Cleanup(func() { updatePoll = prev })

	newFakeRepos(t, map[string]fakeRepo{
		"good": {"v2.0.0", []byte("#!/bin/sh\nexec " + fakeservice(t) + " \"$@\"\n")},
		"bad":  {"v2.0.0", []byte("#!/bin/sh\nexit 1\n")},
	})
	c := runDaemon(t, home)
	for _, path := range []string{first, second, third} {
		svc, err := manifest.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := waitHealthy(c, svc, "", 10*time.Second); err != nil {
			t.Fatalf("%s never came up: %v", svc.Name, err)
		}
	}

	stdout, stderr, err := runCLI(t, home, "update", "--all", "--wait", "20s")
	if err == nil {
		t.Fatalf("a rolled-back release counted as success:\n%s\n%s", stdout, stderr)
	}
	if !strings.Contains(err.Error(), "bsecond") || !strings.Contains(err.Error(), "rolled back to v1.0.0") {
		t.Errorf("err = %v", err)
	}
	for path, want := range map[string]string{first: "v2.0.0", second: "v1.0.0", third: "v1.0.0"} {
		if got := releaseOf(t, path); got != want {
			t.Errorf("%s records %s; want %s", filepath.Base(path), got, want)
		}
	}
	for _, want := range []string{
		"Updated: afirst (v1.0.0 to v2.0.0)",
		"Failed: bsecond (v1.0.0 to v2.0.0)",
		"Not tried: cthird",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	// Reused the per-service path, backup included, rather than its own.
	if !strings.Contains(stdout, "backed up afirst") {
		t.Errorf("no backup:\n%s", stdout)
	}
	if st, err := c.Status("bsecond"); err != nil || st.Status != "running" {
		t.Errorf("bsecond = %+v, %v; want it back up on the old release", st, err)
	}
	if got := readFile(t, filepath.Join(home, "bin", "cthird")); got != string(original) {
		t.Error("cthird's binary was replaced")
	}
}

// Asking again costs a 304, and what's installed is still weighed afresh: the
// etag says the latest hasn't moved, not that the answer is the same.
func TestReleaseChecks_UseTheETag(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	f := newFakeRepos(t, map[string]fakeRepo{"r": {"v2.0.0", []byte("new")}})
	svc, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	checks := &releaseChecks{a: &app{home: home}}
	ctx := context.Background()

	got, err := checks.check(ctx, svc)
	if err != nil || !got.Newer || got.Latest != "v2.0.0" {
		t.Fatalf("first = %+v, %v", got, err)
	}
	got, err = checks.check(ctx, svc)
	if err != nil || !got.Newer || got.Latest != "v2.0.0" {
		t.Errorf("second = %+v, %v", got, err)
	}
	if full, unmoved := f.counts(); full != 1 || unmoved != 1 {
		t.Errorf("full = %d, 304s = %d; want one of each", full, unmoved)
	}

	svc.Release = "v2.0.0"
	if got, err = checks.check(ctx, svc); err != nil || got.Newer {
		t.Errorf("after updating = %+v, %v; want nothing to offer", got, err)
	}
	if full, unmoved := f.counts(); full != 1 || unmoved != 2 {
		t.Errorf("full = %d, 304s = %d", full, unmoved)
	}

	// A new release gets past the etag.
	f.mu.Lock()
	f.repos["r"] = fakeRepo{"v3.0.0", []byte("newer")}
	f.mu.Unlock()
	if got, err = checks.check(ctx, svc); err != nil || !got.Newer || got.Latest != "v3.0.0" {
		t.Errorf("after a release = %+v, %v", got, err)
	}
}

// A check that fails mustn't lose the etag, or the next costs a full request.
func TestReleaseChecks_FailureKeepsTheETag(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	f := newFakeRepos(t, map[string]fakeRepo{"r": {"v2.0.0", []byte("new")}})
	svc, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	checks := &releaseChecks{a: &app{home: home}}
	ctx := context.Background()
	if _, err := checks.check(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := checks.check(cancelled, svc); err == nil {
		t.Fatal("a cancelled check succeeded")
	}
	if got, err := checks.check(ctx, svc); err != nil || !got.Newer {
		t.Errorf("after a failure = %+v, %v", got, err)
	}
	if full, unmoved := f.counts(); full != 1 || unmoved != 1 {
		t.Errorf("full = %d, 304s = %d", full, unmoved)
	}
}

// checkingDaemon is runDaemon with release checks running, as `serve` has them.
func checkingDaemon(t *testing.T, home string) *ipc.Client {
	t.Helper()
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &outpostruntime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		BackupDir:      filepath.Join(home, "backups"),
		Interval:       50 * time.Millisecond,
		CheckRelease:   (&releaseChecks{a: &app{home: home}}).check,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, err := ipc.Listen(filepath.Join(home, "outpost.sock"), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = sup.Run(ctx) }()
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	go func() { defer wg.Done(); sup.RunReleaseChecks(ctx) }()
	t.Cleanup(func() {
		_ = sup.Shutdown(context.Background()) // the test is over either way
		cancel()
		srv.Close()
		wg.Wait()
		store.Close()
	})
	return ipc.NewClient(filepath.Join(home, "outpost.sock"))
}

// With no gateway anywhere, list and status say what's waiting.
func TestListAndStatus_ShowWhatTheDaemonFound(t *testing.T) {
	home, err := os.MkdirTemp("", "op") // sockets need a short path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) }) // a temp dir; the OS reaps what's left
	original, err := os.ReadFile(fakeservice(t))
	if err != nil {
		t.Fatal(err)
	}
	installFrom(t, home, "newer", "a", original, "v1.0.0")
	installFrom(t, home, "same", "a", original, "v2.0.0")
	declareInstalled(t, home, "byhand", original, freePort(t), "")
	newFakeRepos(t, map[string]fakeRepo{"a": {"v2.0.0", []byte("x")}})
	c := checkingDaemon(t, home)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := c.Status("same")
		if err == nil && !st.ReleaseCheckedAt.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	stdout, stderr, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if !strings.HasSuffix(strings.TrimSpace(lines[0]), "UPDATE") {
		t.Errorf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		want := "-"
		if fields[0] == "newer" {
			want = "available"
		}
		if fields[len(fields)-1] != want {
			t.Errorf("row %q ends in %q; want %q", line, fields[len(fields)-1], want)
		}
	}
	if !strings.Contains(stdout, "v2.0.0 available") {
		t.Errorf("list:\n%s", stdout)
	}

	stdout, _, err = runCLI(t, home, "status", "newer")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"installed:     v1.0.0 from o/a", "available:     v2.0.0; `outpost update newer` installs it"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = runCLI(t, home, "status", "same")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "available:     nothing newer") {
		t.Errorf("status:\n%s", stdout)
	}
	stdout, _, err = runCLI(t, home, "status", "byhand")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "available:") || strings.Contains(stdout, "installed:") {
		t.Errorf("no source to speak of:\n%s", stdout)
	}
}

func TestWriteTable_NoColumnWithNothingWaiting(t *testing.T) {
	var b strings.Builder
	if err := writeTable(&b, []supervisor.ServiceStatus{{Name: "svc", Runtime: "process", Status: "running", Source: "o/r", Release: "v1"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "UPDATE") {
		t.Errorf("table:\n%s", b.String())
	}
}

func TestAvailableLine(t *testing.T) {
	at := time.Now().Add(-3 * time.Hour)
	for name, tc := range map[string]struct {
		st   supervisor.ServiceStatus
		want []string
	}{
		"not asked yet":  {supervisor.ServiceStatus{}, []string{"not checked yet"}},
		"never answered": {supervisor.ServiceStatus{ReleaseCheckError: "offline"}, []string{"unknown", "offline"}},
		"nothing":        {supervisor.ServiceStatus{ReleaseCheckedAt: at}, []string{"nothing newer", "3h0m0s ago"}},
		"a note":         {supervisor.ServiceStatus{ReleaseCheckedAt: at, ReleaseNote: "its build may still be uploading"}, []string{"nothing to install", "uploading"}},
		"last known":     {supervisor.ServiceStatus{Name: "svc", ReleaseCheckedAt: at, UpdateAvailable: true, LatestRelease: "v2", ReleaseCheckError: "offline"}, []string{"v2;", "`outpost update svc`", "3h0m0s ago", "latest check failed: offline"}},
	} {
		got := availableLine(tc.st)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q lacks %q", name, got, want)
			}
		}
	}
}

// The page's checks go through the supervisor, so `list` learns from them too.
func TestSupervisedUpdates_CheckIsRemembered(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	newFakeRepos(t, map[string]fakeRepo{"r": {"v2.0.0", []byte("new")}})
	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &app{home: home}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &outpostruntime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		CheckRelease:   (&releaseChecks{a: a}).check,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reload rather than Apply: it loads the manifests without starting anything.
	if _, err := sup.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := supervisedUpdates{daemonUpdates{a}, sup}.Latest(ctx, "svc")
	if err != nil || !got.Newer || got.Latest != "v2.0.0" {
		t.Fatalf("Latest = %+v, %v", got, err)
	}
	st, err := sup.Status(ctx, "svc")
	if err != nil || !st.UpdateAvailable || st.LatestRelease != "v2.0.0" {
		t.Errorf("status = %+v, %v", st, err)
	}
}
