package node

import (
	"fmt"
	"testing"
)

// tsnet repeats the login URL every few seconds; the daemon's log should
// carry it once.
func TestOnceEach_PassesEachDistinctLineOnce(t *testing.T) {
	var got []string
	logf := onceEach(func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) })
	for range 5 {
		logf("To start this tsnet server, go to: %s", "https://login.tailscale.com/a/abc")
	}
	logf("To start this tsnet server, go to: %s", "https://login.tailscale.com/a/def")
	logf("100%% done") // a format verb in the line itself survives
	if len(got) != 3 {
		t.Fatalf("logged %d lines; want 3: %q", len(got), got)
	}
	if got[2] != "100% done" {
		t.Errorf("line mangled: %q", got[2])
	}
}
