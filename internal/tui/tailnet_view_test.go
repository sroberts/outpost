package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/tailnet"
)

// The detail pane must say what was actually observed on the tailnet, not
// just the configured name — that is the whole point of internal/tailnet.
func TestDetail_ShowsTailnetState(t *testing.T) {
	s := svc("plumbline", "running")
	s.Tailnet = tailnet.NodeStatus{State: tailnet.StateAttached, Hostname: "plumbline", Addr: "100.64.1.2"}
	m := loaded(t, newFakeClient(s))

	view := m.View()
	if !strings.Contains(view, "attached, 100.64.1.2") {
		t.Errorf("attached state with address not shown:\n%s", view)
	}
}

// A node stuck awaiting authorisation is the one state where the detail pane
// gains a row: nothing else here names something the operator has to go do.
func TestDetail_NeedsLoginAddsLoginRow(t *testing.T) {
	s := svc("plumbline", "running")
	s.Tailnet = tailnet.NodeStatus{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/abc123"}
	m := loaded(t, newFakeClient(s))

	view := m.View()
	if !strings.Contains(view, "login") {
		t.Errorf("needs-login state missing a login row:\n%s", view)
	}
	if !strings.Contains(view, "login.tailscale.com") {
		t.Errorf("login URL not shown:\n%s", view)
	}
}

// A very long login URL must not stretch the pane past the model's width —
// the invariant every other user-visible string in this package already
// honours. Measured in display cells, per the package's own standing rule.
func TestDetail_LongLoginURLDoesNotOverflowPane(t *testing.T) {
	s := svc("plumbline", "running")
	s.Tailnet = tailnet.NodeStatus{
		State:    tailnet.StateNeedsLogin,
		LoginURL: "https://login.tailscale.com/a/" + strings.Repeat("x", 200),
	}
	m := loaded(t, newFakeClient(s))
	m.width = 60

	for _, line := range strings.Split(m.detail(), "\n") {
		if w := lipgloss.Width(stripANSI(line)); w > m.width {
			t.Errorf("line %d cells wide, exceeds model width %d: %q", w, m.width, line)
		}
	}
}
