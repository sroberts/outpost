package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/ipc"
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
	c := a.client()
	model := tui.New(c, a.tailLog).WithConfigCheck(func() bool {
		// Re-read the file on every check: a.config is what it held when the
		// dashboard started, so comparing against that would miss an edit made
		// while it runs, and flag a daemon restarted onto that edit as stale.
		cfg, err := config.Load(a.configPath())
		if err != nil {
			return false
		}
		running, err := c.DaemonConfig()
		return err == nil && running != "" && running != cfg.Fingerprint()
	})

	// The alt screen keeps the dashboard from scrolling the user's scrollback
	// away.
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}

// The dashboard shows users, the gateway and the AI models through the same
// client.
var (
	_ tui.Extras   = (*ipc.Client)(nil)
	_ tui.AIExtras = (*ipc.Client)(nil)
)

// tailWindow is how much of the end of a log file tailLog reads. Logs rotate at
// 10 MiB, and the dashboard re-reads them every couple of seconds, so it reads a
// bounded window rather than the whole file to keep a dozen lines.
const tailWindow = 64 << 10

// tailLog returns the last n lines of a service's log file.
//
// Like `outpost logs`, this reads the file directly rather than going through
// the socket: it is a local file, and streaming it would need a framed protocol.
func (a *app) tailLog(service string, n int) (string, error) {
	path := filepath.Join(a.logsDir(), service+".log")
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	start := int64(0)
	if info.Size() > tailWindow {
		start = info.Size() - tailWindow
	}
	body := make([]byte, info.Size()-start)
	read, err := f.ReadAt(body, start)
	if err != nil && err != io.EOF {
		return "", err
	}
	body = body[:read]

	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	// Starting mid-file means the first line is a fragment of a longer one.
	if start > 0 && len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}
