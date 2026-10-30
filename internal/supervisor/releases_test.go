package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// fakeSource answers release checks and counts them.
type fakeSource struct {
	mu     sync.Mutex
	answer ReleaseCheck
	err    error
	panics bool
	asked  []string // the release each check was made against
}

func (f *fakeSource) check(_ context.Context, svc *manifest.Service) (ReleaseCheck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, svc.Name+"@"+svc.Release)
	if f.panics {
		panic("boom")
	}
	return f.answer, f.err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// releaseHarness has "app" installed at v1 from a source, and "byhand" without.
func releaseHarness(t *testing.T) (*harness, *fakeSource) {
	t.Helper()
	h := newHarness(t)
	f := &fakeSource{answer: ReleaseCheck{Latest: "v2", Newer: true}}
	h.sup.checkRelease = f.check
	h.writeManifest("app", "source = \"o/app\"\nrelease = \"v1\"")
	h.writeManifest("byhand", "")
	h.apply()
	return h, f
}

// pass does what one round of RunReleaseChecks does.
func (h *harness) pass() {
	h.t.Helper()
	due, err := h.sup.releaseChecksDue(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, svc := range due {
		_, _ = h.sup.checkOneRelease(context.Background(), svc)
	}
}

func (h *harness) statusOf(name string) ServiceStatus {
	h.t.Helper()
	st, err := h.sup.Status(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func TestReleaseCheck_OnlyServicesWithASourceAndOnlyDaily(t *testing.T) {
	h, f := releaseHarness(t)
	h.pass()
	if len(f.asked) != 1 || f.asked[0] != "app@v1" {
		t.Fatalf("asked = %v; want app, at v1", f.asked)
	}
	st := h.statusOf("app")
	if st.LatestRelease != "v2" || !st.UpdateAvailable || st.ReleaseCheckedAt.IsZero() {
		t.Errorf("status = %+v", st)
	}
	if st := h.statusOf("byhand"); st.LatestRelease != "" || !st.ReleaseCheckedAt.IsZero() {
		t.Errorf("no source, but status = %+v", st)
	}

	h.clock.Advance(releaseCheckEvery - time.Minute)
	h.pass()
	if f.count() != 1 {
		t.Errorf("%d checks before a day was up", f.count())
	}
	h.clock.Advance(time.Minute)
	h.pass()
	if f.count() != 2 {
		t.Errorf("%d checks after a day; want the second", f.count())
	}
}

func TestReleaseCheck_FailureKeepsTheLastAnswerAndRetriesSooner(t *testing.T) {
	h, f := releaseHarness(t)
	h.pass()
	answered := h.statusOf("app").ReleaseCheckedAt

	h.clock.Advance(releaseCheckEvery)
	f.err = errors.New("rate limited")
	h.pass()
	st := h.statusOf("app")
	if st.LatestRelease != "v2" || !st.UpdateAvailable || !st.ReleaseCheckedAt.Equal(answered) {
		t.Errorf("after a failure, status = %+v; want the last answer", st)
	}
	if st.ReleaseCheckError != "rate limited" {
		t.Errorf("error = %q", st.ReleaseCheckError)
	}

	h.pass()
	if f.count() != 2 {
		t.Errorf("%d checks; a failure shouldn't mean asking on every pass", f.count())
	}
	h.clock.Advance(releaseRetryEvery)
	f.err = nil
	h.pass()
	if f.count() != 3 {
		t.Errorf("%d checks; want a retry after %s", f.count(), releaseRetryEvery)
	}
	if st := h.statusOf("app"); st.ReleaseCheckError != "" {
		t.Errorf("error stayed after a good check: %q", st.ReleaseCheckError)
	}
}

// With nothing known yet, a failure is all there is to say.
func TestReleaseCheck_FirstFailureShowsNoAnswer(t *testing.T) {
	h, f := releaseHarness(t)
	f.err = errors.New("offline")
	h.pass()
	st := h.statusOf("app")
	if st.LatestRelease != "" || st.UpdateAvailable || st.ReleaseCheckError != "offline" {
		t.Errorf("status = %+v", st)
	}
}

// An answer about a release that's gone says nothing, so an update can't leave
// a stale offer behind, and the new release is asked about straight away.
func TestReleaseCheck_ReleaseChangeHidesTheAnswerAndAsksAgain(t *testing.T) {
	h, f := releaseHarness(t)
	h.pass()

	h.writeManifest("app", "source = \"o/app\"\nrelease = \"v2\"")
	h.apply()
	st := h.statusOf("app")
	if st.UpdateAvailable || st.LatestRelease != "" || !st.ReleaseCheckedAt.IsZero() {
		t.Errorf("after an update, status = %+v; want the old answer gone", st)
	}

	f.answer = ReleaseCheck{Latest: "v2"}
	h.pass()
	if len(f.asked) != 2 || f.asked[1] != "app@v2" {
		t.Errorf("asked = %v; want a check of v2", f.asked)
	}
	if st := h.statusOf("app"); st.UpdateAvailable || st.LatestRelease != "v2" {
		t.Errorf("status = %+v", st)
	}
	h.pass()
	if f.count() != 2 {
		t.Errorf("%d checks; one change is one check", f.count())
	}
}

func TestReleaseCheck_ForgetsAServiceNoLongerDeclared(t *testing.T) {
	h, _ := releaseHarness(t)
	h.pass()
	if err := os.Remove(filepath.Join(h.dir, "services", "app.toml")); err != nil {
		t.Fatal(err)
	}
	h.apply()
	h.pass()
	if _, ok := h.sup.releases["app"]; ok {
		t.Error("still remembers app")
	}
}

func TestCheckRelease_AsksNowAndRemembers(t *testing.T) {
	h, f := releaseHarness(t)
	ctx := context.Background()
	got, err := h.sup.CheckRelease(ctx, "app")
	if err != nil || got.Latest != "v2" {
		t.Fatalf("CheckRelease = %+v, %v", got, err)
	}
	if st := h.statusOf("app"); !st.UpdateAvailable {
		t.Errorf("not remembered: %+v", st)
	}
	h.pass()
	if f.count() != 1 {
		t.Errorf("%d checks; the one just made should count", f.count())
	}

	for _, name := range []string{"byhand", "nope"} {
		if _, err := h.sup.CheckRelease(ctx, name); err == nil {
			t.Errorf("CheckRelease(%s) succeeded", name)
		}
	}
	f.err = errors.New("offline")
	if _, err := h.sup.CheckRelease(ctx, "app"); err == nil {
		t.Error("a failed check didn't say so")
	}
}

func TestReleaseCheck_PanicIsAFailedCheck(t *testing.T) {
	h, f := releaseHarness(t)
	f.panics = true
	h.pass()
	if st := h.statusOf("app"); !strings.Contains(st.ReleaseCheckError, "boom") {
		t.Errorf("error = %q", st.ReleaseCheckError)
	}
}

// The reason this isn't on the loop: GitHub being slow can't hold up
// supervision, and everything else keeps answering meanwhile.
func TestRunReleaseChecks_DoesNotHoldUpTheLoop(t *testing.T) {
	h, _ := releaseHarness(t)
	h.sup.Interval = 5 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.sup.checkRelease = func(context.Context, *manifest.Service) (ReleaseCheck, error) {
		once.Do(func() { close(entered) })
		<-release
		return ReleaseCheck{Latest: "v2", Newer: true}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = h.sup.Run(ctx) }()
	go func() { defer wg.Done(); h.sup.RunReleaseChecks(ctx) }()
	defer func() {
		cancel()
		wg.Wait()
	}()

	<-entered
	listCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if _, err := h.sup.List(listCtx); err != nil {
		t.Fatalf("List while a check is stuck: %v", err)
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := h.sup.Status(listCtx, "app"); err == nil && st.UpdateAvailable {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the answer never reached the supervisor")
}

func TestRunReleaseChecks_WithoutAChecker(t *testing.T) {
	h := newHarness(t)
	h.sup.RunReleaseChecks(context.Background()) // returns, rather than polling for nothing
	if _, err := h.sup.CheckRelease(context.Background(), "app"); err == nil {
		t.Error("CheckRelease with no checker succeeded")
	}
}

// A release caught while its build uploads hides nothing for long: it could be
// all that stands between the operator and an update.
func TestReleaseCheck_PendingReleaseIsAskedAboutSooner(t *testing.T) {
	h, f := releaseHarness(t)
	f.answer = ReleaseCheck{Latest: "v2", Note: "still uploading", Pending: true}
	h.pass()

	h.clock.Advance(releaseRetryEvery - time.Minute)
	h.pass()
	if f.count() != 1 {
		t.Errorf("%d checks; want no more within the hour", f.count())
	}
	h.clock.Advance(time.Minute)
	f.answer = ReleaseCheck{Latest: "v2", Newer: true}
	h.pass()
	if f.count() != 2 {
		t.Errorf("%d checks; want another after an hour", f.count())
	}
	if st := h.statusOf("app"); !st.UpdateAvailable {
		t.Errorf("status = %+v", st)
	}
	h.clock.Advance(releaseRetryEvery)
	h.pass()
	if f.count() != 2 {
		t.Errorf("%d checks; back to daily once it's there", f.count())
	}
}
