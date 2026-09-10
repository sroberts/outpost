package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// manyServices builds a client with n running services, plus two whose names
// are multi-byte — this package has repeatedly measured bytes where it meant
// display cells, and the viewport does its own width arithmetic.
func manyServices(n int) *fakeClient {
	c := newFakeClient()
	for i := 1; i <= n; i++ {
		c.services = append(c.services, svc(fmt.Sprintf("service-%02d", i), "running"))
	}
	c.services = append(c.services, svc("日本語サービス", "running"))
	c.services = append(c.services, svc("café-münster", "failed"))
	return c
}

// The whole point of the issue: the dashboard must fit the terminal it is
// given, at every height, with or without the log pane.
func TestView_NeverExceedsTerminalHeight(t *testing.T) {
	m := loaded(t, manyServices(40))
	m.width = 100

	for _, logs := range []bool{false, true} {
		m.showLogs = logs
		m.logText = strings.Repeat("a log line\n", 8)
		for height := 1; height <= 60; height++ {
			m.height = height
			// Below minViewHeight the chrome alone will not fit; the guarantee
			// there is that it degrades to that floor rather than to the whole
			// forty-two-service list.
			limit := height
			if limit < minViewHeight {
				limit = minViewHeight
			}
			for _, cursor := range []int{0, 21, 41} {
				m.cursor = cursor
				got := lipgloss.Height(m.View())
				if got > limit {
					t.Fatalf("height=%d logs=%v cursor=%d: rendered %d lines (limit %d)",
						height, logs, cursor, got, limit)
				}
			}
		}
	}
}

// A service the operator cannot see is a service they cannot act on, and the
// keys act on the cursor.
func TestView_SelectedServiceIsAlwaysOnScreen(t *testing.T) {
	c := manyServices(40)
	m := loaded(t, c)
	m.width, m.height = 100, 20

	for cursor := 0; cursor < len(m.services); cursor++ {
		m.cursor = cursor
		name := m.services[cursor].Name
		if !strings.Contains(stripANSI(m.View()), name) {
			t.Fatalf("cursor=%d: selected service %q is not on screen", cursor, name)
		}
	}
}

// Windowing must not disturb the selection: the cursor is the input and the
// viewport follows it, never the other way round.
func TestWindow_FollowsTheCursorWithoutMovingIt(t *testing.T) {
	m := loaded(t, manyServices(40))
	m.width, m.height = 100, 20

	for cursor := 0; cursor < len(m.services); cursor++ {
		m.cursor = cursor
		before := m.cursor
		start, end := m.window(10)
		if m.cursor != before {
			t.Fatalf("window moved the cursor from %d to %d", before, m.cursor)
		}
		if cursor < start || cursor >= end {
			t.Errorf("cursor %d outside the window [%d,%d)", cursor, start, end)
		}
	}
}

// A list that fits needs no indicator and no scrolling.
func TestWindow_ShortListIsShownWhole(t *testing.T) {
	m := loaded(t, manyServices(2))
	n := len(m.services)

	start, end := m.window(n + 5)
	if start != 0 || end != n {
		t.Errorf("window = [%d,%d); want the whole list [0,%d)", start, end, n)
	}
	if note := m.rangeNote(start, end); note != "" {
		t.Errorf("rangeNote = %q; want nothing when the list fits", note)
	}
}

func TestRangeNote_ReportsThePositionInTheList(t *testing.T) {
	m := loaded(t, manyServices(40))
	if got, want := m.rangeNote(5, 15), "showing 6–15 of 42"; got != want {
		t.Errorf("rangeNote = %q; want %q", got, want)
	}
}

// The indicator rides on the header row, so a windowed list must not be taller
// than an unwindowed one -- that is what lets View measure the chrome once.
func TestTableRange_IndicatorCostsNoExtraLine(t *testing.T) {
	m := loaded(t, manyServices(40))
	m.width = 100

	windowed := lipgloss.Height(m.tableRange(5, 15))
	unwindowed := lipgloss.Height(m.tableRange(0, 10))
	if windowed != unwindowed {
		t.Errorf("windowed table is %d lines, unwindowed %d; the indicator took a line",
			windowed, unwindowed)
	}
}
