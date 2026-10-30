package tui

import (
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

// A removed service (#32) gets a mark of its own rather than the generic one,
// so it doesn't read as an unknown state.
func TestStatusMark_Removed(t *testing.T) {
	if got := statusMark(supervisor.StatusRemoved); got == statusMark("no-such-status") {
		t.Errorf("removed shares the unknown-status mark %q", got)
	}
}
