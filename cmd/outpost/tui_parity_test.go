package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/tui"
)

// tuiExempt are the commands the dashboard deliberately doesn't run, and why.
var tuiExempt = map[string]string{
	"serve":     "is the daemon itself",
	"install":   "sets up the OS service, may need root, and can ask on the terminal the dashboard owns",
	"uninstall": "takes that service down, and the daemon the dashboard is talking to with it",
}

// usageCommand is a command line in the usage text: two spaces, a command,
// and perhaps a subcommand, before any argument or flag.
var usageCommand = regexp.MustCompile(`^  ([a-z][a-z-]*)(?: ([a-z][a-z-]*))?(?:\s|$)`)

// Every command `outpost help` lists can be done from the dashboard, so the
// TUI can't fall behind the CLI without this failing.
func TestTUIParity_EveryCommandIsReachable(t *testing.T) {
	covers := tui.Covers()
	seen := 0
	for _, line := range strings.Split(usage, "\n") {
		m := usageCommand.FindStringSubmatch(line)
		if m == nil || m[1] == "outpost" {
			continue
		}
		cmd := m[1]
		if m[2] != "" && covers["cli:"+m[1]+" "+m[2]] {
			cmd = m[1] + " " + m[2]
		} else if m[2] != "" && !covers["cli:"+m[1]] {
			cmd = m[1] + " " + m[2]
		}
		seen++
		if _, ok := tuiExempt[cmd]; ok {
			continue
		}
		if !covers["cli:"+cmd] {
			t.Errorf("`outpost %s` can't be done from the dashboard: give it an action covering cli:%s, or say in tuiExempt why not", cmd, cmd)
		}
	}
	if seen < 30 {
		t.Fatalf("read only %d commands from the usage; has its layout changed?", seen)
	}
}
