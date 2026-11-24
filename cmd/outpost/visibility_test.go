package main

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

// `outpost list` says how far each app reaches, and "-" for a daemon too old
// to say.
func TestWriteTable_ShowsVisibility(t *testing.T) {
	var b strings.Builder
	err := writeTable(&b, []supervisor.ServiceStatus{
		{Name: "open", Runtime: "process", Status: "running", TailscaleName: "open", Visibility: "tailnet"},
		{Name: "quiet", Runtime: "process", Status: "running", Visibility: "gateway"},
		{Name: "old", Runtime: "process", Status: "running"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	want := [][]string{
		{"NAME", "RUNTIME", "STATUS", "UPTIME", "RESTARTS", "TAILSCALE", "VISIBILITY"},
		{"open", "process", "running", "-", "0", "open", "tailnet"},
		{"quiet", "process", "running", "-", "0", "-", "gateway"},
		{"old", "process", "running", "-", "0", "-", "-"},
	}
	if len(lines) != len(want) {
		t.Fatalf("table:\n%s", b.String())
	}
	for i, w := range want {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != strings.Join(w, " ") {
			t.Errorf("line %d = %q; want %q", i, got, strings.Join(w, " "))
		}
	}
}
