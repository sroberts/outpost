package tui

import (
	"fmt"
	"strings"
	"testing"
)

// A digit typed into the log search is part of the query, not a jump to
// another view.
func TestLogs_SearchTakesDigits(t *testing.T) {
	m := logViewOn(t, "port 8080\n")
	m = press(t, m, "/", "8", "0")
	if m.view != viewLogs || !m.lv.searching || m.lv.query.Value() != "80" {
		t.Fatalf("view %d searching %v query %q", m.view, m.lv.searching, m.lv.query.Value())
	}
}

// Two reads of every log in flight at once add each new line once.
func TestLogs_EveryServiceOverlappingReadsDontDuplicate(t *testing.T) {
	m, _ := fullDashboard(t, 0)
	logs := map[string]string{"notes": "n1\n"}
	m.logs = func(s string, _ int) (string, error) { return logs[s], nil }
	m.view, m.lv.all = viewLogs, true
	m = drain(t, m, m.fetchLogView())
	logs["notes"] = "n1\nn2\n"
	first, second := m.fetchLogView(), m.fetchLogView()
	m = drain(t, m, first)
	m = drain(t, m, second)
	var got []string
	for _, l := range m.lv.lines {
		if l.svc == "notes" {
			got = append(got, l.text)
		}
	}
	if strings.Join(got, " ") != "n1 n2" {
		t.Errorf("notes lines = %v", got)
	}
}

// Scrolled back in a log longer than one read, the view holds still as the
// read's window slides.
func TestLogs_ScrolledBackHoldsAsTheWindowSlides(t *testing.T) {
	window := func(from int) string {
		var b strings.Builder
		for i := from; i < from+100; i++ { // longer than the screen
			fmt.Fprintf(&b, "line %d\n", i)
		}
		return b.String()
	}
	m := logViewOn(t, window(0))
	m = press(t, m, "up")
	m = m.updateLogView(logViewMsg{service: "notes", lines: linesOf(window(2))})
	if m.lv.offset != 3 {
		t.Errorf("offset %d after two lines arrived; want 3", m.lv.offset)
	}
}

func linesOf(text string) []logLine {
	var out []logLine
	for _, l := range splitLines(text) {
		out = append(out, logLine{text: l})
	}
	return out
}

// A log that repeats a line must still show what was written after it.
func TestNewSince_RepeatedLines(t *testing.T) {
	cases := []struct {
		prev, cur, want []string
	}{
		{[]string{"x", "x", "x"}, []string{"x", "x", "x", "x"}, []string{"x"}}, // grew, uncapped
		{[]string{"a", "x", "x"}, []string{"x", "x", "x"}, []string{"x"}},      // capped window slid by one
		{[]string{"x", "x", "x"}, []string{"x", "x", "x", "b"}, []string{"b"}}, // something new after repeats
		{[]string{"a", "b", "a", "b"}, []string{"a", "b", "a", "b", "a", "b"}, []string{"a", "b"}},
		{[]string{"a", "b"}, []string{"a", "b"}, nil},                         // nothing written
		{[]string{"old"}, []string{"new1", "new2"}, []string{"new1", "new2"}}, // rotated
	}
	for _, c := range cases {
		got := newSince(c.prev, c.cur)
		if len(got) != len(c.want) || (len(got) > 0 && !equalLines(got, c.want)) {
			t.Errorf("newSince(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

// g goes to the top of the log with the screen full, not to the first line
// alone above blank rows; scrolling up can't go past that either.
func TestLogs_TopFillsTheScreen(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	m := logViewOn(t, b.String())
	rows := logRows(m.bodyHeight(), false)

	m = press(t, m, "g")
	shown := m.visibleLogLines(rows)
	if len(shown) != rows {
		t.Fatalf("top of the log shows %d rows, want a full %d", len(shown), rows)
	}
	if !strings.Contains(plainText(shown[0]), "line 0") {
		t.Errorf("top row = %q, want line 0", plainText(shown[0]))
	}
	top := m.lv.offset
	m = press(t, m, "up", "pgup")
	if m.lv.offset != top {
		t.Errorf("scrolled past the top: offset %d, top is %d", m.lv.offset, top)
	}

	// Wrapped, each line takes two rows, so the top is further down.
	long := strings.Repeat("x", m.width) // wider than the text area
	b.Reset()
	for range 500 {
		b.WriteString(long + "\n")
	}
	m = logViewOn(t, b.String())
	m = press(t, m, "w", "g")
	if got := len(m.visibleLogLines(rows)); got != rows {
		t.Errorf("wrapped top shows %d rows, want %d", got, rows)
	}
	if want := 500 - (rows+1)/2; m.lv.offset != want {
		t.Errorf("wrapped top offset %d, want %d", m.lv.offset, want)
	}
}
