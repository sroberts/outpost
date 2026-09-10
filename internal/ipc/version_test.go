package ipc

import (
	"testing"

	"github.com/sroberts/outpost/internal/version"
)

// The daemon has to report its own build over the socket, because that is the
// only way the CLI can notice it is talking to a binary that was replaced on
// disk months ago and never restarted.
func TestPing_ReportsTheDaemonBuild(t *testing.T) {
	c, _ := testDaemon(t)

	got, err := c.DaemonVersion()
	if err != nil {
		t.Fatalf("DaemonVersion: %v", err)
	}
	if got == "" {
		t.Fatal("daemon reported no version")
	}
	// Same process here, so it must match exactly; a mismatch would mean the
	// wire value is being derived somewhere other than the version package.
	if want := version.Short(); got != want {
		t.Errorf("daemon version = %q; want %q", got, want)
	}
}
