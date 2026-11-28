package supervisor

import (
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// The manifest's mcp_funnel reaches the status the gateway routes by, and the
// service handed on, without the portal's exposure setting touching it.
func TestMCPFunnel_ReachesStatus(t *testing.T) {
	h := newHarness(t)
	h.sup.exposure = &fakeExposure{}
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true, Gateway: true}
	h.writeManifest("sous", "mcp_path = \"/mcp\"\nmcp_funnel = true")
	h.writeManifest("tasking", `mcp_path = "/mcp"`)
	h.apply()

	if st := exposureStatus(t, h, "sous"); !st.MCPFunnel || st.MCPPath != "/mcp" {
		t.Errorf("sous = %+v; want mcp_funnel on", st)
	}
	if st := exposureStatus(t, h, "tasking"); st.MCPFunnel {
		t.Error("tasking has mcp_funnel on without asking")
	}
	if got := h.sup.desired["sous"]; got == nil || !got.MCPFunnel || !got.FunnelListener() {
		t.Errorf("desired sous = %+v; want its node to have a Funnel listener", got)
	}
}
