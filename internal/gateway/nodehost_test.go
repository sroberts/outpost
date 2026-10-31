package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A node's name comes from Tailscale, so it can land on an address another
// app already holds; the first holder keeps it rather than being overwritten.
func TestRefreshRoutes_NodeHostDoesNotTakeAnAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hosts map[string]string
	}{
		{"another app's subdomain", map[string]string{"wiki": "yarr.outpost.test"}},
		{"another app's node", map[string]string{"yarr": yarrNode, "wiki": yarrNode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var logs strings.Builder
			h.g.logf = func(format string, v ...any) { fmt.Fprintf(&logs, format+"\n", v...) }
			h.g.tailnetHosts = func() map[string]string { return tc.hosts }
			h.g.refreshRoutes(context.Background())
			h.g.refreshRoutes(context.Background())

			taken := tc.hosts["wiki"]
			if r, ok := h.g.routes.forHost(taken); !ok || r.Name != "yarr" {
				t.Errorf("%s routes to %q; want yarr, which had it first", taken, r.Name)
			}
			if r, ok := h.g.routes.forHost("wiki.outpost.test"); !ok || r.Name != "wiki" || r.Host != "" {
				t.Errorf("wiki's route = %+v, %v; want it kept on its own address without the taken name", r, ok)
			}
			if got := strings.Count(logs.String(), "both want "+taken); got != 1 {
				t.Errorf("logged the clash %d times; want once:\n%s", got, logs.String())
			}
		})
	}
}
