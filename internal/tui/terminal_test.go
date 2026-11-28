package tui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeExec struct{ ran bool }

func (f *fakeExec) Run() error          { f.ran = true; return nil }
func (f *fakeExec) SetStdin(io.Reader)  {}
func (f *fakeExec) SetStdout(io.Writer) {}
func (f *fakeExec) SetStderr(io.Writer) {}

// Restarting the daemon, and updating Outpost, can stop for a sudo password,
// so they go to the terminal rather than through the captured runner.
func TestPalette_PromptingCommandsGetTheTerminal(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"daemon-restart", "daemon restart"},
		{"self-update", "update --self"},
	} {
		var onTerminal, captured []string
		m := New(newSecretsClient(), nil).
			WithRunner(func(args ...string) (string, error) { captured = args; return "", nil }).
			WithTerminal(func(args ...string) (tea.ExecCommand, func() string) {
				onTerminal = args
				return &fakeExec{}, func() string { return "done" }
			})
		m.width, m.height = 100, 40
		m = drain(t, m, m.fetchServices())
		m, _ = m.openAction(tc.id)
		if m.pal.form == nil || !m.pal.form.confirming {
			t.Fatalf("%s: no confirmation asked:\n%s", tc.id, m.View())
		}
		_, cmd := run(t, m, keyMsg("y"))
		if cmd == nil {
			t.Fatalf("%s: confirming ran nothing", tc.id)
		}
		if strings.Join(onTerminal, " ") != tc.want || captured != nil {
			t.Errorf("%s: on the terminal %v, captured %v; want %q on the terminal", tc.id, onTerminal, captured, tc.want)
		}
	}
}

// Without a TerminalRunner they fall back to the captured runner.
func TestPalette_PromptingCommandsFallBackToTheRunner(t *testing.T) {
	var captured []string
	m := New(newSecretsClient(), nil).
		WithRunner(func(args ...string) (string, error) { captured = args; return "restarted", nil })
	m.width, m.height = 100, 40
	m = drain(t, m, m.fetchServices())
	m, _ = m.openAction("daemon-restart")
	m, cmd := run(t, m, keyMsg("y"))
	m = drain(t, m, cmd)
	if strings.Join(captured, " ") != "daemon restart" {
		t.Fatalf("ran %v", captured)
	}
	if m.out == nil || !strings.Contains(m.out.text, "restarted") {
		t.Errorf("output pane = %+v", m.out)
	}
}
