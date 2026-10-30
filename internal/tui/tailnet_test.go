package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// The detail view shows where to log a node in, and a long tailnet error is
// cut to the line rather than wrapping.
func TestDetail_TailnetLoginAndLongError(t *testing.T) {
	waiting := svc("yarr", "running")
	waiting.Tailnet = &supervisor.TailnetStatus{State: supervisor.TailnetNeedsLogin, LoginURL: "https://login.tailscale.com/a/abc"}
	c := newFakeClient(waiting)
	m := New(c, nil)
	m.width = 80
	m, _ = run(t, m, servicesMsg{services: c.services})
	if v := m.View(); !strings.Contains(v, "yarr (needs login)") || !strings.Contains(v, "https://login.tailscale.com/a/abc") {
		t.Errorf("view:\n%s", v)
	}

	failing := svc("yarr", "running")
	failing.Tailnet = &supervisor.TailnetStatus{State: supervisor.TailnetStarting, Error: strings.Repeat("invalid key ", 20)}
	m, _ = run(t, m, servicesMsg{services: []supervisor.ServiceStatus{failing}})
	for _, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("a %d-cell line at width 80: %q", w, line)
		}
	}
}
