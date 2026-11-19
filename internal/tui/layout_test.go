package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// fullDashboard is a full client with many services and a log reader.
func fullDashboard(t *testing.T, n int) (Model, *fullClient) {
	t.Helper()
	c := newSecretsClient()
	for i := 1; i <= n; i++ {
		c.services = append(c.services, svc(fmt.Sprintf("service-%02d", i), "running"))
	}
	c.services = append(c.services, svc("日本語サービス", "failed"))
	m := New(c, func(service string, lines int) (string, error) {
		return strings.Repeat("a log line from "+service+" \x1b[31mred\x1b[0m\n", 50), nil
	})
	m.width, m.height = 100, 40
	m = drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway(), m.fetchUsage()))
	m.logText = strings.Repeat("a log line\n", 60)
	return m, c
}

// Every view, at every size, fits the terminal: no line wider than it, and
// no more lines than it has (down to the floor the services view has).
func TestLayout_EveryViewFitsTheTerminal(t *testing.T) {
	m, _ := fullDashboard(t, 30)
	m.lv.lines = []logLine{{text: strings.Repeat("long ", 80)}, {text: "short"}}
	m.lv.service = "notes"
	sizes := [][2]int{{80, 24}, {100, 30}, {120, 24}, {120, 40}, {160, 50}, {240, 70}, {60, 12}}
	setups := map[string]func(Model) Model{
		"services":      func(m Model) Model { m.view = viewServices; return m },
		"services+logs": func(m Model) Model { m.view, m.showLogs = viewServices, true; return m },
		"zoom":          func(m Model) Model { m.view, m.zoom = viewServices, true; return m },
		"overview":      func(m Model) Model { m.view = viewOverview; return m },
		"secrets":       func(m Model) Model { m.view = viewSecrets; return drain(t, m, m.fetchSecrets()) },
		"tokens":        func(m Model) Model { m.view = viewTokens; return drain(t, m, m.fetchTokens()) },
		"logs":          func(m Model) Model { m.view = viewLogs; return m },
		"logs wrapped":  func(m Model) Model { m.view, m.lv.wrap = viewLogs, true; return m },
		"palette":       func(m Model) Model { return m.openPalette() },
		"output": func(m Model) Model {
			m.out = &outputPane{title: "x", text: strings.Repeat("output line\n", 200)}
			return m
		},
	}
	for name, setup := range setups {
		for _, sz := range sizes {
			mm := setup(m)
			mm.width, mm.height = sz[0], sz[1]
			mm.help.Width = sz[0]
			view := mm.View()
			limit := max(sz[1], minViewHeight)
			if h := lipgloss.Height(view); h > limit {
				t.Errorf("%s at %dx%d: %d lines", name, sz[0], sz[1], h)
			}
			for _, line := range strings.Split(view, "\n") {
				if w := lipgloss.Width(line); w > sz[0] {
					t.Errorf("%s at %dx%d: a line is %d cells: %q", name, sz[0], sz[1], w, stripANSI(line))
					break
				}
			}
			if strings.Contains(view, "\x1b[31m") {
				t.Errorf("%s: a service's own escapes reached the screen", name)
			}
		}
	}
}

// On a wide terminal the list and the detail sit side by side, and the
// graphs are drawn.
func TestLayout_WideSplitsListAndDetail(t *testing.T) {
	m, _ := fullDashboard(t, 3)
	m.width, m.height = 160, 45
	view := stripANSI(m.View())
	side := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "NAME") && strings.Contains(line, "│") && strings.Contains(line, "notes") {
			side = true
		}
	}
	if !side {
		t.Errorf("no line holds both the list's header and the detail:\n%s", view)
	}
	if !strings.Contains(view, "usage") || !strings.Contains(view, "peak") {
		t.Errorf("no tall graphs on a wide terminal:\n%s", view)
	}
}

// A short list on a tall terminal gives the rest to the log.
func TestLayout_LogFillsWhatTheListLeaves(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	m.showLogs, m.height = true, 60
	n := strings.Count(stripANSI(m.View()), "a log line")
	if n <= logLines {
		t.Errorf("log shows %d lines on a 60-row terminal; want more than %d", n, logLines)
	}
	if h := lipgloss.Height(m.View()); h > 60 {
		t.Errorf("rendered %d lines", h)
	}
}

func TestTable_UsageColumnsOnlyWithUsage(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	if table := stripANSI(m.table()); !strings.Contains(table, "CPU") || !strings.Contains(table, "25%") || !strings.Contains(table, "48 MiB") {
		t.Errorf("no usage columns:\n%s", table)
	}
	m.usage = nil
	if strings.Contains(m.table(), "CPU") {
		t.Error("usage columns with no usage")
	}
}

func TestGraphs_ToggleRange(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	if !strings.Contains(stripANSI(m.View()), "last 30 min") {
		t.Fatal("not on the half hour")
	}
	m = press(t, m, "g")
	if !strings.Contains(stripANSI(m.View()), "last 24 h") {
		t.Fatal("g didn't switch to the day")
	}
}

func TestSparkline(t *testing.T) {
	nan := math.NaN()
	if got := sparkline([]float64{0, 1, 2, nan, 4}, 5, 0); got != "▁▃▅ █" {
		t.Errorf("sparkline = %q", got)
	}
	if got := sparkline(nil, 3, 0); got != "   " {
		t.Errorf("no data = %q", got)
	}
	// Squeezed, a spike survives: each cell is its values' largest.
	if got := sparkline([]float64{0, 0, 0, 9, 0, 0}, 2, 0); got != "▁█" {
		t.Errorf("resampled = %q", got)
	}
	if w := lipgloss.Width(sparkline(make([]float64, 96), 17, 0)); w != 17 {
		t.Errorf("width %d", w)
	}
}

func TestChart(t *testing.T) {
	rows := chart([]float64{0, 2, 4, math.NaN()}, 4, 2, 4)
	if len(rows) != 2 || rows[0] != "  █ " || rows[1] != "▁██ " {
		t.Errorf("chart = %q", rows)
	}
}

func TestFormat(t *testing.T) {
	for in, want := range map[float64]string{0: "0 B", 1536: "1.5 KiB", 48 << 20: "48 MiB", 3 << 30: "3.0 GiB"} {
		if got := fmtBytes(in); got != want {
			t.Errorf("fmtBytes(%v) = %q, want %q", in, got, want)
		}
	}
	if fmtCPU(0.25) != "25%" || fmtCPU(0.004) != "0.4%" || fmtCPU(math.NaN()) != "-" {
		t.Error("fmtCPU")
	}
}

func TestTabs_FitEightyColumnsWithEveryView(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	for _, w := range []int{80, 60, 40} {
		m.width = w
		for _, v := range m.views() {
			m.view = v
			if got := lipgloss.Width(m.tabs()); got > w && w >= 60 {
				t.Errorf("tabs at %d: %d cells", w, got)
			}
		}
	}
}

func TestTabs_DigitsJump(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	m = press(t, m, "1")
	if m.view != viewOverview {
		t.Errorf("1 went to %d", m.view)
	}
	m = press(t, m, "3")
	if m.view != viewUsers {
		t.Errorf("3 went to %d", m.view)
	}
}

func TestOverview_ShowsTilesAndOpensThem(t *testing.T) {
	m, _ := fullDashboard(t, 2)
	m.width, m.height = 140, 50
	m.view = viewOverview
	m = drain(t, m, m.fetchOverview())
	v := stripANSI(m.View())
	for _, want := range []string{"services", "attention", "reach", "backups", "activity", "1 failed", "日本語サービス", "https://outpost.example"} {
		if !strings.Contains(v, want) {
			t.Errorf("overview lacks %q:\n%s", want, v)
		}
	}
	m = press(t, m, "down", "down", "enter")
	if m.view != viewGateway {
		t.Errorf("enter on reach went to %d", m.view)
	}
}

func TestNewSince(t *testing.T) {
	cases := []struct {
		prev, cur, want []string
	}{
		{nil, []string{"a"}, []string{"a"}},
		{[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{[]string{"a", "b", "c"}, []string{"b", "c", "d", "e"}, []string{"d", "e"}},
		{[]string{"x", "y"}, []string{"p", "q"}, []string{"p", "q"}},
		// The same line written again is new, not the old one.
		{[]string{"a", "ok", "ok"}, []string{"a", "ok", "ok", "ok"}, []string{"ok"}},
		{[]string{"a", "b"}, []string{"a", "b"}, []string{}},
	}
	for _, c := range cases {
		got := newSince(c.prev, c.cur)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("newSince(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

// logViewOn is a dashboard on the logs view for notes, with the read done,
// without starting its ticker (which a drain would wait on forever).
func logViewOn(t *testing.T, text string) Model {
	t.Helper()
	m, _ := fullDashboard(t, 0)
	m.logs = func(string, int) (string, error) { return text, nil }
	m.view, m.lv.service = viewLogs, "notes"
	m = drain(t, m, m.fetchLogView())
	return m
}

func TestLogs_SearchFindsAndHighlights(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString("panic: boom\n")
	for i := range 100 {
		fmt.Fprintf(&b, "after %d\n", i)
	}
	m := logViewOn(t, b.String())
	if strings.Contains(m.View(), "panic") {
		t.Fatal("the match is on screen before searching")
	}
	m = press(t, m, "/")
	for _, r := range "PANIC" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	if !strings.Contains(stripANSI(m.View()), "panic: boom") || m.lv.follow {
		t.Fatalf("search didn't scroll to the match:\n%s", stripANSI(m.View()))
	}
	m = press(t, m, "G")
	if !m.lv.follow || m.lv.offset != 0 {
		t.Error("G didn't go back to following")
	}
}

func TestLogs_FollowHoldsStillWhenScrolledUp(t *testing.T) {
	// Longer than the screen: a log that fits has nowhere to scroll.
	var lines []logLine
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "line %d\n", i)
		lines = append(lines, logLine{text: fmt.Sprintf("line %d", i)})
	}
	m := logViewOn(t, b.String())
	m = press(t, m, "up")
	if m.lv.follow || m.lv.offset != 1 {
		t.Fatalf("up: follow=%v offset=%d", m.lv.follow, m.lv.offset)
	}
	m = m.updateLogView(logViewMsg{service: "notes", lines: append(lines, logLine{text: "line 100"})})
	if m.lv.offset != 2 {
		t.Errorf("offset %d after a line arrived; want 2, the same lines on screen", m.lv.offset)
	}
}

func TestLogs_EveryServiceInterleavesNewLines(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	logs := map[string]string{"notes": "n1\n", "wiki": "w1\n"}
	m.logs = func(s string, _ int) (string, error) { return logs[s], nil }
	m.view, m.lv.all = viewLogs, true
	m = drain(t, m, m.fetchLogView())
	logs["wiki"] = "w1\nw2\n"
	m = drain(t, m, m.fetchLogView())
	logs["notes"] = "n1\nn2\n"
	m = drain(t, m, m.fetchLogView())
	var got []string
	for _, l := range m.lv.lines {
		got = append(got, l.svc+":"+l.text)
	}
	if strings.Join(got, " ") != "notes:n1 wiki:w1 wiki:w2 notes:n2" {
		t.Errorf("lines = %v", got)
	}
}

func TestServices_LOpensTheFullLog(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	next, _ := m.Update(keyMsg("L"))
	m = next.(Model)
	if m.view != viewLogs || m.lv.service != "notes" || !m.lv.ticking {
		t.Errorf("L: view %d service %q ticking %v", m.view, m.lv.service, m.lv.ticking)
	}
}

func TestWatch_PushedListsAreShownAndPollingResumesWhenItEnds(t *testing.T) {
	c := &streamClient{fullClient: newSecretsClient(), ch: make(chan []supervisor.ServiceStatus, 1)}
	m := New(c, nil)
	m.width, m.height = 100, 40
	m, cmd := run(t, m, exec(t, m.startWatch()))
	if !m.watching {
		t.Fatal("not watching after the stream started")
	}
	c.ch <- []supervisor.ServiceStatus{svc("zeta", "running"), svc("alpha", "failed")}
	m, cmd = run(t, m, exec(t, cmd))
	if len(m.services) != 2 || m.services[0].Name != "alpha" {
		t.Fatalf("pushed list not shown sorted: %v", m.services)
	}
	if !strings.Contains(stripANSI(m.View()), "live") {
		t.Error("header doesn't say the list is live")
	}
	// The next frame is waited for: the batch holds nextFrame.
	if cmd == nil {
		t.Fatal("no wait for the next frame")
	}
	close(c.ch)
	m, _ = run(t, m, nextFrame(c.ch)())
	if m.watching {
		t.Error("still watching after the stream closed")
	}
}

// Opening a view reads what it shows then, not on the next tick.
func TestViews_OpeningOneFetchesIt(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	for _, v := range []view{viewSecrets, viewTokens} {
		next, cmd := m.enterView(v)
		next = drain(t, next, cmd)
		if strings.Contains(next.View(), "loading…") {
			t.Errorf("%s still loading after opening:\n%s", viewNames[v], stripANSI(next.View()))
		}
	}
}
