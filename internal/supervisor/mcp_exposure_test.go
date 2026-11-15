package supervisor

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// fakeMCPExposure is fakeExposure with the portal's mcp_funnel settings too.
type fakeMCPExposure struct {
	fakeExposure
	mcp map[string]bool
}

func (f *fakeMCPExposure) MCPOverride(service string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	on, ok := f.mcp[service]
	return on, ok
}

// The portal's mcp_funnel wins over the manifest's, either way, and status
// says both; forgetting it puts the manifest's back.
func TestExposure_PortalSetsMCPFunnel(t *testing.T) {
	h := newHarness(t)
	ex := &fakeMCPExposure{mcp: map[string]bool{"alpha": true, "beta": false}}
	h.sup.exposure = ex
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true, Gateway: true}
	h.writeManifest("alpha", `mcp_path = "/mcp"`)
	h.writeManifest("beta", "mcp_path = \"/mcp\"\nmcp_funnel = true")
	h.apply()

	if st := exposureStatus(t, h, "alpha"); !st.MCPFunnel || !st.MCPFromPortal || st.ManifestMCPFunnel || st.FromPortal {
		t.Errorf("alpha = %+v", st)
	}
	if !h.sup.desired["alpha"].FunnelListener() {
		t.Error("alpha runs without its node's Funnel listener")
	}
	if st := exposureStatus(t, h, "beta"); st.MCPFunnel || !st.MCPFromPortal || !st.ManifestMCPFunnel {
		t.Errorf("beta = %+v", st)
	}
	if h.sup.desired["beta"].FunnelListener() {
		t.Error("beta, turned off on the portal, still has its node's Funnel listener")
	}

	ex.mu.Lock()
	ex.mcp = nil
	ex.mu.Unlock()
	h.apply()
	if st := exposureStatus(t, h, "alpha"); st.MCPFunnel || st.MCPFromPortal {
		t.Errorf("alpha after a reset = %+v", st)
	}
	if st := exposureStatus(t, h, "beta"); !st.MCPFunnel || st.MCPFromPortal {
		t.Errorf("beta after a reset = %+v", st)
	}
}

// A setting the manifest's rules refuse, such as on for an app with no
// mcp_path, is left out and logged, and the rest still apply.
func TestExposure_AnMCPSettingThatDoesntApplyIsLeftOut(t *testing.T) {
	h := newHarness(t)
	ex := &fakeMCPExposure{mcp: map[string]bool{"alpha": true}}
	h.sup.exposure = ex
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true, Gateway: true}
	var logs []string
	h.sup.logf = func(format string, args ...any) { logs = append(logs, format) }
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()

	if st := exposureStatus(t, h, "alpha"); st.MCPFunnel || st.MCPFromPortal {
		t.Errorf("alpha took a setting the rules refuse: %+v", st)
	}
	if _, ok := h.sup.desired["beta"]; !ok {
		t.Error("beta wasn't applied")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "MCP endpoint on the internet doesn't apply") {
		t.Errorf("not logged: %v", logs)
	}
}
