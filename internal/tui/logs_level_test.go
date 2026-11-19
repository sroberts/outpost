package tui

import (
	"strings"
	"testing"
)

// pressDrain presses k and runs what it starts, as the program would.
func pressDrain(t *testing.T, m Model, k string) Model {
	t.Helper()
	m, cmd := run(t, m, keyMsg(k))
	return drain(t, m, cmd)
}

// v cycles the logs view through every line, warnings and up, and errors
// only, reading each line's level from its tag.
func TestLogs_LevelFilterCycles(t *testing.T) {
	text := "12:00:00 INFO booting\n12:00:01 WARN slow\n12:00:02 ERRO db locked\n12:00:03 err  stderr line\nlegacy line\n"
	m := logViewOn(t, text)
	count := func() int { return len(m.lv.lines) }
	if count() != 5 {
		t.Fatalf("unfiltered: %d lines", count())
	}
	m = pressDrain(t, m, "v")
	if m.lv.minLevel != "warn" || count() != 3 || !strings.Contains(m.View(), "warn and up") {
		t.Fatalf("warn and up: level %q, %d lines", m.lv.minLevel, count())
	}
	m = pressDrain(t, m, "v")
	if m.lv.minLevel != "error" || count() != 2 {
		t.Fatalf("errors: level %q, %d lines: %v", m.lv.minLevel, count(), logTexts(m.lv.lines))
	}
	m = pressDrain(t, m, "v")
	if m.lv.minLevel != "" || count() != 5 {
		t.Fatalf("back to all: level %q, %d lines", m.lv.minLevel, count())
	}
}
