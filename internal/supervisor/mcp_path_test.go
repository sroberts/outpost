package supervisor

import (
	"context"
	"testing"
)

// The gateway learns where an app serves MCP only through the supervisor's
// API, like everything else it knows about it.
func TestStatus_CarriesMCPPath(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", `mcp_path = "/mcp"`)
	h.writeManifest("beta", "")
	h.apply()

	if st, err := h.sup.Status(context.Background(), "alpha"); err != nil || st.MCPPath != "/mcp" {
		t.Errorf("alpha: MCPPath = %q, %v; want /mcp", st.MCPPath, err)
	}
	list, err := h.sup.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range list {
		want := map[string]string{"alpha": "/mcp", "beta": ""}[st.Name]
		if st.MCPPath != want {
			t.Errorf("List: %s MCPPath = %q; want %q", st.Name, st.MCPPath, want)
		}
	}
}
