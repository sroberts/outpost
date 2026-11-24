package tui

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/applog"
)

// o steps the logs view through Outpost's own logs, then back to the
// service it was on.
func TestLogs_OwnLogsCycle(t *testing.T) {
	m := logViewOn(t, "12:00:00 INFO notes booting\n")
	var asked []string
	m = m.WithOwnLogs(func(source string, _ int) (string, error) {
		asked = append(asked, source)
		return "12:00:01 WARN [" + source + "] something\n", nil
	})

	m = pressDrain(t, m, "o")
	if m.lv.own != applog.SourceDaemon || len(m.lv.lines) != 1 || !strings.Contains(m.lv.lines[0].text, "[daemon]") ||
		!strings.Contains(m.View(), "Outpost's own log") {
		t.Fatalf("first o: own %q, lines %v", m.lv.own, logTexts(m.lv.lines))
	}
	m = pressDrain(t, m, "o")
	if m.lv.own != applog.SourceAccess || !strings.Contains(m.View(), "requests to the gateway") {
		t.Fatalf("second o: own %q", m.lv.own)
	}
	// The level filter works on them as on a service's lines.
	m = pressDrain(t, m, "v")
	if len(m.lv.lines) != 1 {
		t.Errorf("warn and up dropped a warning: %v", logTexts(m.lv.lines))
	}
	m = pressDrain(t, m, "o")
	if m.lv.own != "" || m.lv.service != "notes" || len(m.lv.lines) != 0 && !strings.Contains(m.lv.lines[0].text, "notes") {
		t.Fatalf("third o: own %q, service %q, lines %v", m.lv.own, m.lv.service, logTexts(m.lv.lines))
	}
	if strings.Join(asked, ",") != "daemon,access,access" {
		t.Errorf("read %v", asked)
	}
}
