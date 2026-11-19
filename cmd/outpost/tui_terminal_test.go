package main

import (
	"bytes"
	"strings"
	"testing"
)

// A command handed the terminal prints there as it runs, and keeps a copy
// for the dashboard's output pane.
func TestTerminalForTUI_PrintsAndKeepsATranscript(t *testing.T) {
	a := &app{}
	cmd, output := a.terminalForTUI("version")
	var screen bytes.Buffer
	cmd.SetStdout(&screen)
	cmd.SetStderr(&screen)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen.String(), "$ outpost version") {
		t.Errorf("no header on the terminal:\n%s", screen.String())
	}
	got := output()
	if strings.TrimSpace(got) == "" || strings.Contains(got, "$ outpost") {
		t.Errorf("transcript = %q; want the command's output alone", got)
	}
	if !strings.Contains(screen.String(), strings.TrimSpace(got)) {
		t.Errorf("the terminal didn't show what the transcript kept:\n%s", screen.String())
	}
}
