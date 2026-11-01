package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// A service halted on its first exit was never restarted, so the catalog
// doesn't count crashes it didn't have.
func TestCatalog_HaltedWithoutRestartsCountsNoCrashes(t *testing.T) {
	for _, c := range []struct {
		status, want string
	}{
		{"failed", "Crashed, and isn&#39;t set to restart"},
		{"stopped", "Stopped, and isn&#39;t set to restart"},
	} {
		h := newHarness(t)
		h.withServices(supervisor.ServiceStatus{Name: "yarr", Port: 1, Status: c.status, Desired: "running", Halted: true})
		h.addUser(auth.User{Username: "alice"})
		cl := h.client(true)
		b := body(t, h.login(cl, "alice", "", "/"))
		if strings.Contains(b, "0 crashes") || !strings.Contains(b, c.want) {
			t.Errorf("%s: catalog lacks %q or counts 0 crashes:\n%s", c.status, c.want, b)
		}
	}
}
