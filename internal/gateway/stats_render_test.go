package gateway

import (
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/stats"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeStats answers Usage from a fixed map.
type fakeStats map[string]stats.Usage

func (f fakeStats) Usage(name string) (stats.Usage, bool) {
	u, ok := f[name]
	return u, ok
}

// daySeries is a day of buckets: NaN, then vals at the end, so it reads as
// history that began part way through the day.
func daySeries(vals ...float64) []float64 {
	s := make([]float64, stats.Buckets)
	for i := range s {
		s[i] = math.NaN()
	}
	copy(s[len(s)-len(vals):], vals)
	return s
}

func busyUsage(now time.Time) stats.Usage {
	return stats.Usage{
		Up: true, Measured: true,
		CPU: 0.02, RSS: 84e6,
		Storage: 1.2e9, StorageAt: now,
		In24: 18e6, Out24: 220e6,
		CPUSeries:     daySeries(0.4, 0.03),
		RSSSeries:     daySeries(80e6, 84e6),
		StorageSeries: daySeries(1.2e9, 1.2e9),
		InSeries:      daySeries(9e6, 9e6),
		OutSeries:     daySeries(100e6, 120e6),
		Since:         now.Add(-30 * time.Minute),
	}
}

func TestCatalogShowsUsageLineAndGraphs(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.g.stats = fakeStats{"yarr": busyUsage(now)}
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Description: "RSS reader"})
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	row := rowOf(t, b, "yarr")

	for _, want := range []string{
		`<summary class="usage"><span class="figs">2.0% CPU · 84 MB memory · 1.2 GB stored</span><span class="when"><span class="sr">, </span>Since `,
		`<span class="sr"> for yarr</span></summary>`,
		"<dt>CPU</dt>", "<dt>Memory</dt>", "<dt>Network</dt>", "<dt>Storage</dt>",
		`<span class="tile-val">peak 40%</span><span class="tile-val">now 2.0%</span>`,
		`<span class="tile-val">peak 84 MB</span><span class="tile-val">now 84 MB</span>`,
		`<span class="tile-val key in">18 MB in</span>`,
		`<span class="tile-val key out">220 MB out</span>`,
		`<span class="tile-val">now 1.2 GB</span>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q:\n%s", want, row)
		}
	}
	if !before(row, `class="figs"`, `class="stats-panel"`) {
		t.Error("the summary line doesn't come before the graphs")
	}
	svgs := regexp.MustCompile(`<svg viewBox="0 0 96 36" preserveAspectRatio="none" aria-hidden="true" focusable="false">`).FindAllString(row, -1)
	if len(svgs) != 4 {
		t.Errorf("want 4 hidden graphs, got %d", len(svgs))
	}
	if !regexp.MustCompile(`<path class="spark" d="M[0-9., ML]+" vector-effect="non-scaling-stroke"/>`).MatchString(row) {
		t.Errorf("no drawn path:\n%s", row)
	}
	if !strings.Contains(row, `class="spark in"`) || !strings.Contains(row, `class="spark out"`) {
		t.Error("network isn't drawn as two lines")
	}
	if strings.Contains(b, "style=") {
		t.Error("the page has an inline style, which the CSP blocks")
	}
}

func TestUsageFollowsWhoCanSeeTheApp(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.g.stats = fakeStats{"yarr": busyUsage(now), "wiki": busyUsage(now), "vault": busyUsage(now)}
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Hidden: true},
		supervisor.ServiceStatus{Name: "vault", Status: "running", Allow: []string{"root"}},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if !strings.Contains(rowOf(t, b, "yarr"), `class="usage"`) {
		t.Error("a plain user doesn't see an allowed app's usage")
	}
	if strings.Contains(b, `id="app-wiki"`) || strings.Contains(b, `id="app-vault"`) {
		t.Error("a hidden or disallowed app is listed")
	}
	if n := strings.Count(b, `class="usage"`); n != 1 {
		t.Errorf("want usage for one app, got %d", n)
	}
}

func TestNoStatsMeansNoUsageMarkup(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running"})
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	for _, no := range []string{`class="usage"`, `class="stats"`, "<svg"} {
		if strings.Contains(b, no) {
			t.Errorf("with no sampler the page has %q", no)
		}
	}

	// A sampler that hasn't seen the app yet is the same.
	h.g.stats = fakeStats{}
	b = body(t, h.login(h.client(true), "alice", "", "/"))
	if strings.Contains(b, `class="usage"`) || strings.Contains(b, `class="stats"`) {
		t.Error("an app the sampler hasn't seen shows usage")
	}
}

func TestUnmeasuredAndPartialUsage(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	fresh := stats.Usage{
		Up: true, Measured: true, CPU: math.NaN(), RSS: 5e6,
		Storage: 3e9, StorageAt: now, StoragePartial: true,
		CPUSeries: daySeries(), RSSSeries: daySeries(5e6), StorageSeries: daySeries(3e9),
		InSeries: daySeries(), OutSeries: daySeries(),
		Since: now,
	}
	stopped := stats.Usage{
		CPU: 0, Storage: 4e6, StorageAt: now,
		CPUSeries: daySeries(), RSSSeries: daySeries(), StorageSeries: daySeries(4e6),
		InSeries: daySeries(), OutSeries: daySeries(),
	}
	h.g.stats = fakeStats{"yarr": fresh, "notes": stopped}
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "notes", Status: "stopped", Desired: "stopped"},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))

	if strings.Contains(b, "NaN") {
		t.Errorf("an unmeasured reading leaked as NaN:\n%s", b)
	}
	yarr := rowOf(t, b, "yarr")
	for _, want := range []string{
		`<span aria-hidden="true">– CPU</span><span class="sr">CPU not measured yet</span>`,
		"5.0 MB memory", "at least 3.0 GB stored", "now at least 3.0 GB",
	} {
		if !strings.Contains(yarr, want) {
			t.Errorf("yarr's row lacks %q:\n%s", want, yarr)
		}
	}
	if strings.Contains(yarr, "<dt>CPU</dt>") || strings.Contains(yarr, "<dt>Network</dt>") {
		t.Error("a graph with no readings is drawn")
	}

	notes := rowOf(t, b, "notes")
	if !strings.Contains(notes, `<summary class="usage"><span class="figs">4.0 MB stored</span>`) {
		t.Errorf("a stopped app doesn't show just its storage:\n%s", notes)
	}
	if strings.Contains(notes, "CPU") || strings.Contains(notes, "memory") {
		t.Error("a stopped app shows readings it doesn't have")
	}
}

func TestSinceText(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 30, 0, 0, time.Local)
	for _, c := range []struct {
		since time.Time
		want  string
	}{
		{time.Time{}, ""},
		{now.Add(-24 * time.Hour), ""},
		{now.Add(-2 * time.Hour), "Since 07:30"},
		{now.Add(-12 * time.Hour), "Since yesterday 21:30"},
	} {
		if got := sinceText(c.since, now); got != c.want {
			t.Errorf("sinceText(%v) = %q, want %q", c.since, got, c.want)
		}
	}
}

// Every graph has a baseline. One whose history starts part way through the
// day draws the rest broken, and every line ends in a dot, so a graph a
// minute old is still a mark and not a sliver at the edge.
func TestGraphsHaveBaselineGapAndEndDots(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	full := busyUsage(now)
	full.CPUSeries = make([]float64, stats.Buckets) // a whole day of idle
	full.Since = now.Add(-30 * time.Hour)
	h.g.stats = fakeStats{"yarr": busyUsage(now), "wiki": full}
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Title: "Family Wiki"},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))

	yarr := rowOf(t, b, "yarr")
	// Two of 96 buckets measured: the gap runs from the left edge to the
	// first of them, and the baseline from there to the right edge.
	if n := strings.Count(yarr, `<path class="unmeasured" d="M0,35.5H95" vector-effect="non-scaling-stroke"/>`); n != 4 {
		t.Errorf("want the unmeasured stretch broken on all 4 graphs, got %d:\n%s", n, yarr)
	}
	if n := strings.Count(yarr, `<path class="base" d="M95,35.5H96" vector-effect="non-scaling-stroke"/>`); n != 4 {
		t.Errorf("want a baseline under all 4 graphs, got %d", n)
	}
	dots := regexp.MustCompile(`<path class="dot( in| out)?" d="M96,[0-9.]+h0"`).FindAllString(yarr, -1)
	if len(dots) != 5 { // CPU, memory, storage, and network in and out
		t.Errorf("want a dot at the right edge of each of 5 lines, got %d:\n%s", len(dots), yarr)
	}

	wiki := rowOf(t, b, "wiki")
	if !strings.Contains(wiki, `<path class="base" d="M0,35.5H96"`) {
		t.Errorf("a full day's graph has no full-width baseline:\n%s", wiki)
	}
	if !strings.Contains(wiki, `<span class="when"><span class="sr">, </span>Last 24 hours</span><span class="sr"> for Family Wiki</span>`) {
		t.Errorf("a full day isn't labelled as the last 24 hours, for its app:\n%s", wiki)
	}
}

// Storage is walked every ten minutes, so the quarter hour now in often has
// no walk yet; the graph still runs to the right edge like the others.
func TestStorageGraphReachesNow(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	u := busyUsage(now)
	u.StorageSeries = daySeries(1.2e9, math.NaN())
	u.StorageAt = now.Add(-5 * time.Minute)
	h.g.stats = fakeStats{"yarr": u}
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running"})
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	storage := rowOf(t, b, "yarr")[strings.Index(rowOf(t, b, "yarr"), "<dt>Storage</dt>"):]
	if !regexp.MustCompile(`<path class="spark" d="M[0-9.,]+L96,[0-9.]+"`).MatchString(storage) {
		t.Errorf("the storage line stops short of now:\n%s", storage)
	}
	if !strings.Contains(storage, `<path class="dot" d="M96,`) {
		t.Errorf("the storage dot isn't at now:\n%s", storage)
	}
}

// The network footnote is said once for the page, not once for every app.
func TestNetworkNoteOncePerPage(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.g.stats = fakeStats{"yarr": busyUsage(now), "wiki": busyUsage(now)}
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running"},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if n := strings.Count(b, "Network counts only traffic through Outpost."); n != 1 {
		t.Errorf("want the network note once, got %d", n)
	}
	if strings.Contains(rowOf(t, b, "yarr"), "Network counts") {
		t.Error("the network note is inside an app's row")
	}

	// With no network graph anywhere there's nothing for it to qualify.
	quiet := busyUsage(now)
	quiet.InSeries, quiet.OutSeries = daySeries(), daySeries()
	h.g.stats = fakeStats{"yarr": quiet, "wiki": quiet}
	b = body(t, h.login(h.client(true), "alice", "", "/"))
	if strings.Contains(b, "Network counts") {
		t.Error("the network note shows with no network graph")
	}
}

// When an admin opens both, the Manage actions stay next to their pill,
// with the graphs below them rather than between.
func TestManageComesBeforeTheGraphs(t *testing.T) {
	h, _ := actionHarness(t)
	h.g.stats = fakeStats{"yarr": busyUsage(time.Now())}
	b := body(t, h.login(h.client(true), "root", "", "/"))
	row := rowOf(t, b, "yarr")
	if !before(row, `class="acts"`, `class="stats"`) {
		t.Errorf("the graphs come between the Manage pill and its actions:\n%s", row)
	}
}

// Out is drawn in porcelain: amber is the lamp's alone.
func TestNetworkOutIsNotLampColoured(t *testing.T) {
	css, err := os.ReadFile("static/portal.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range regexp.MustCompile(`(?m)^[^{}]*\.(spark|dot|key)[^{]*\{[^}]*\}`).FindAllString(string(css), -1) {
		if strings.Contains(rule, "--lamp") {
			t.Errorf("a graph rule uses the lamp's colour: %s", rule)
		}
	}
	for _, want := range []string{
		".spark.out { stroke: var(--porcelain); }",
		".dot.out { stroke: var(--porcelain); }",
		".key.out::before { background: var(--porcelain); }",
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("the stylesheet lacks %q", want)
		}
	}
}

// The unmeasured stretch ends where the daemon's first reading of any kind
// is, not where each graph's line starts: an app stopped for the first
// hours is a gap in its line, not history the daemon doesn't have.
func TestUnmeasuredStretchEndsAtTheFirstReadingOfAny(t *testing.T) {
	nan := math.NaN()
	early, late := make([]float64, stats.Buckets), make([]float64, stats.Buckets)
	for i := range early {
		early[i], late[i] = nan, nan
	}
	early[48], late[90] = 1, 0.5
	if got := firstFinite(late, early); got != 48 {
		t.Fatalf("firstFinite = %d; want 48", got)
	}
	tile := graph(48, usageTile{}, line("", late, cpuFloor))
	if want := "M0," + coord(baseY) + "H" + coord(float64(sparkW)*48/float64(stats.Buckets-1)); tile.Gap != want {
		t.Errorf("gap = %q; want %q, ending at the earliest reading", tile.Gap, want)
	}
}
