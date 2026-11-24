package tui

import (
	"math"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// keyMsgType is a special key by name, for the keys press doesn't know.
func keyMsgType(k string) tea.KeyMsg {
	return tea.KeyMsg{Type: map[string]tea.KeyType{"right": tea.KeyRight, "left": tea.KeyLeft, "tab": tea.KeyTab}[k]}
}

// Like btop, the dashboard covers the whole terminal: every view is exactly
// as tall as the terminal, whatever it holds, so nothing is left blank below
// it and nothing scrolls off the top.
func TestFrame_EveryViewFillsTheTerminal(t *testing.T) {
	m, _ := fullDashboard(t, 12)
	m = drain(t, m, m.fetchOverview())
	m.lv.lines = []logLine{{text: "one"}, {text: "two"}}
	m.lv.service = "notes"
	setups := map[string]func(Model) Model{
		"services":      func(m Model) Model { m.view = viewServices; return m },
		"services+logs": func(m Model) Model { m.view, m.showLogs = viewServices, true; return m },
		"zoom":          func(m Model) Model { m.view, m.zoom = viewServices, true; return m },
		"overview":      func(m Model) Model { m.view = viewOverview; return m },
		"users":         func(m Model) Model { m.view = viewUsers; return drain(t, m, m.fetchUsers()) },
		"gateway":       func(m Model) Model { m.view = viewGateway; return m },
		"secrets":       func(m Model) Model { m.view = viewSecrets; return drain(t, m, m.fetchSecrets()) },
		"tokens":        func(m Model) Model { m.view = viewTokens; return drain(t, m, m.fetchTokens()) },
		"logs":          func(m Model) Model { m.view = viewLogs; return m },
		"palette":       func(m Model) Model { return m.openPalette() },
		"form":          func(m Model) Model { m, _ = m.openAction("user-add"); return m },
	}
	for name, setup := range setups {
		for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
			mm := setup(m)
			mm.width, mm.height = sz[0], sz[1]
			mm.help.Width = sz[0]
			if h := lipgloss.Height(mm.View()); h != sz[1] {
				t.Errorf("%s at %dx%d: %d rows, want the terminal's %d", name, sz[0], sz[1], h, sz[1])
			}
		}
	}
}

// A wide terminal gets usage across every service along the top, drawn as
// line charts with a labelled axis.
func TestFrame_SystemChartsOnATallWideTerminal(t *testing.T) {
	m, _ := fullDashboard(t, 3)
	m.width, m.height = 160, 50
	v := stripANSI(m.View())
	for _, want := range []string{"cpu · 25%", "memory · 48 MiB", "every service"} {
		if !strings.Contains(v, want) {
			t.Errorf("no %q:\n%s", want, v)
		}
	}
	m.height = 24
	if strings.Contains(stripANSI(m.View()), "every service") {
		t.Error("the usage charts crowd a short terminal")
	}
}

func TestBox_IsExactlyItsSizeWithTheTitleInTheBorder(t *testing.T) {
	lines := box("services", []string{"a line far too long for the panel it is in", "日本語"}, 20, 5, false, "2 of 9")
	if len(lines) != 5 {
		t.Fatalf("%d rows, want 5", len(lines))
	}
	for _, l := range lines {
		if w := lipgloss.Width(l); w != 20 {
			t.Errorf("row %q is %d cells, want 20", stripANSI(l), w)
		}
	}
	if top := stripANSI(lines[0]); !strings.HasPrefix(top, "╭─ services ") || !strings.HasSuffix(top, "╮") {
		t.Errorf("top edge = %q", top)
	}
	if bottom := stripANSI(lines[4]); !strings.Contains(bottom, " 2 of 9 ─╯") {
		t.Errorf("bottom edge = %q", bottom)
	}
	// A title too long for the edge is cut, not run past the corner.
	if w := lipgloss.Width(box(strings.Repeat("x", 50), nil, 12, 3, true, "")[0]); w != 12 {
		t.Errorf("long title: top edge is %d cells", w)
	}
}

func TestAllot(t *testing.T) {
	cases := []struct {
		total, least int
		needs, want  []int
	}{
		{20, 3, []int{4, 6}, []int{9, 11}}, // room to spare, shared
		{10, 3, []int{8, 8}, []int{5, 5}},  // the largest give way
		{10, 3, []int{12, 2}, []int{7, 3}}, // none below the floor
		{4, 3, []int{5, 5}, []int{3, 3}},   // the floor wins; clipped
		{9, 1, []int{1, 1, 1}, []int{3, 3, 3}},
	}
	for _, c := range cases {
		got := allot(c.total, c.least, c.needs...)
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("allot(%d, %d, %v) = %v, want %v", c.total, c.least, c.needs, got, c.want)
				break
			}
		}
	}
}

func TestLineChart_SizeAndAxis(t *testing.T) {
	rows := lineChart([]chartSeries{
		{name: "a", vals: []float64{0, 0.5, 1, math.NaN(), 0.25}, style: graphStyle},
		{name: "b", vals: []float64{0.1, 0.1}, style: graphStyle},
	}, 30, 6, fmtCPU)
	if len(rows) != 6 {
		t.Fatalf("%d rows, want 6", len(rows))
	}
	for _, r := range rows {
		if w := lipgloss.Width(r); w != 30 {
			t.Errorf("row %q is %d cells, want 30", stripANSI(r), w)
		}
	}
	all := stripANSI(strings.Join(rows, "\n"))
	// Labelled in steps up the range, by the formatter given.
	if !strings.Contains(all, " 0%") || !strings.Contains(all, "67%") {
		t.Errorf("axis not labelled with the range:\n%s", all)
	}
}

func TestSumSeries_AlignsAtTheLatestSample(t *testing.T) {
	nan := math.NaN()
	got := sumSeries([][]float64{{1, 2, 3}, {10, nan}, {nan}})
	if len(got) != 3 || got[0] != 1 || got[1] != 12 || got[2] != 3 {
		t.Errorf("sumSeries = %v", got)
	}
	if !math.IsNaN(sumSeries([][]float64{{nan}, {nan}})[0]) {
		t.Error("a point nothing measured should stay unknown")
	}
}

// A required field left empty holds the form where it is, says why, and
// runs nothing.
func TestForm_RequiredFieldIsNeeded(t *testing.T) {
	c := newSecretsClient()
	m := loadedFull(t, c)
	m, _ = m.openAction("secret-rm")
	m = press(t, m, "enter")
	if m.pal.form == nil || m.pal.form.confirming {
		t.Fatal("an empty required field went on to the question")
	}
	if v := stripANSI(m.View()); !strings.Contains(v, "secret is needed") {
		t.Errorf("no word on what's missing:\n%s", v)
	}
	if len(c.removed) != 0 {
		t.Errorf("ran with nothing filled in: %v", c.removed)
	}
}

// The form starts from the selected service, and a choice steps with the
// arrows as well as space.
func TestForm_PrefillsAndChoices(t *testing.T) {
	var ran []string
	m := New(newSecretsClient(), nil).WithRunner(func(args ...string) (string, error) {
		ran = args
		return "", nil
	})
	m.width, m.height = 100, 40
	m = drain(t, m, m.fetchServices())
	m, _ = m.openAction("remove")
	if got := *m.pal.form.values["service"]; got != m.selectedName() || got == "" {
		t.Fatalf("service field = %q, want the selected %q", got, m.selectedName())
	}
	m = press(t, m, "enter")
	var cmd tea.Cmd
	m, cmd = run(t, m, keyMsgType("right"))
	m = drain(t, m, cmd)
	m = press(t, m, "enter")
	if !m.pal.form.confirming || !strings.Contains(m.View(), "permanently delete") {
		t.Fatalf("right didn't choose purge:\n%s", m.View())
	}
	m = press(t, m, "y")
	if strings.Join(ran, " ") != "remove "+m.selectedName()+" --purge --yes" {
		t.Errorf("ran %v", ran)
	}
}
