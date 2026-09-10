package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/tui"
)

// cmdTUI launches the Bubble Tea dashboard (§12).
func (a *app) cmdTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(a.err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	// The dashboard reads the daemon through the same client the CLI uses, so
	// there is one code path to the supervisor.
	model := tui.New(a.client(), a.tailLog)

	// The alt screen keeps the dashboard from scrolling the user's scrollback
	// away; mouse cell motion enables wheel scrolling where the terminal sends it.
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// tailLog returns the last n lines of a service's log file.
//
// Like `outpost logs`, this reads the file directly rather than going through
// the socket: it is a local file, and streaming it would need a framed protocol.
func (a *app) tailLog(service string, n int) (string, error) {
	path := filepath.Join(a.logsDir(), service+".log")
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}
