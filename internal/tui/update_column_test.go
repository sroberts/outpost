package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestTable_ShowsAnUpdateOnlyWhenOneIsWaiting(t *testing.T) {
	plain := svc("yarr", "running")
	plain.Source, plain.Release = "o/yarr", "v1"
	if table := loaded(t, newFakeClient(plain)).table(); strings.Contains(table, "UPDATE") {
		t.Errorf("column with nothing to say:\n%s", table)
	}

	waiting := plain
	waiting.LatestRelease, waiting.UpdateAvailable = "v2.47s", true
	m := loaded(t, newFakeClient(svc("wiki", "running"), waiting))
	table := m.table()
	if !strings.Contains(table, "UPDATE") || !strings.Contains(table, "v2.47s available") {
		t.Errorf("table doesn't show it:\n%s", table)
	}
}

// The column has to line up in display cells, with names and tags that aren't
// a byte per cell.
func TestTable_UpdateColumnLinesUpInCells(t *testing.T) {
	a := svc("日本語サービス", "running")
	a.LatestRelease, a.UpdateAvailable = "v2-日本語", true
	b := svc("wiki", "running")
	b.LatestRelease, b.UpdateAvailable = "v10.20.30", true
	c := svc("plain", "running")

	table := loaded(t, newFakeClient(a, b, c)).table()
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d:\n%s", len(lines), table)
	}
	want := lipgloss.Width(lines[0])
	for _, line := range lines[1:] {
		if got := lipgloss.Width(line); got != want {
			t.Errorf("row is %d cells, header %d:\n%s", got, want, table)
		}
	}
}

func TestDetail_SaysInstalledAndAvailable(t *testing.T) {
	s := svc("yarr", "running")
	s.Source, s.Release = "o/yarr", "v1"
	if d := loaded(t, newFakeClient(s)).detail(); !strings.Contains(d, "installed") || strings.Contains(d, "available") {
		t.Errorf("nothing waiting:\n%s", d)
	}
	s.LatestRelease, s.UpdateAvailable = "v2", true
	if d := loaded(t, newFakeClient(s)).detail(); !strings.Contains(d, "available") || !strings.Contains(d, "v2") {
		t.Errorf("one waiting:\n%s", d)
	}
	if d := loaded(t, newFakeClient(svc("byhand", "running"))).detail(); strings.Contains(d, "installed") {
		t.Errorf("no source:\n%s", d)
	}
}
