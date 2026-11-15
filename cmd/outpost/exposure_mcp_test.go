package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/supervisor"
)

// The page's MCP switch is checked by the manifest's rules, stored, and in
// force when it returns; resetting it puts the manifest's back, and neither
// touches public paths and Funnel.
func TestExposureWeb_MCPFunnelSetAndReset(t *testing.T) {
	opts := manifest.Options{RequireTailscale: true, Gateway: true}
	e, store, sup := exposureFixture(t, opts)
	ctx := context.Background()

	// Without an mcp_path there is nothing to put on the internet.
	if err := e.SetMCPFunnel(ctx, "tasking", true, "root"); err == nil || !strings.Contains(err.Error(), "mcp_path") {
		t.Errorf("on without an mcp_path = %v", err)
	}
	if _, ok := store.MCPOverride("tasking"); ok {
		t.Error("a refused setting was stored")
	}

	m := filepath.Join(e.a.servicesDir(), "tasking.toml")
	body, err := os.ReadFile(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m, append(body, "mcp_path = \"/mcp\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.SetMCPFunnel(ctx, "tasking", true, "root"); err != nil {
		t.Fatal(err)
	}
	st := taskingStatus(t, sup)
	if !st.MCPFunnel || !st.MCPFromPortal || st.ManifestMCPFunnel || st.FromPortal {
		t.Errorf("after setting it: %+v", st)
	}
	if _, _, ok := store.Override("tasking"); ok {
		t.Error("the MCP switch stored public paths and Funnel too")
	}

	if err := e.ResetMCPFunnel(ctx, "tasking", "root"); err != nil {
		t.Fatal(err)
	}
	if st := taskingStatus(t, sup); st.MCPFunnel || st.MCPFromPortal {
		t.Errorf("after resetting it: %+v", st)
	}
}

// outpost status says when the portal set the MCP endpoint, including off.
func TestMCPFunnelLine_FromThePortal(t *testing.T) {
	off := supervisor.ServiceStatus{Name: "sous", MCPPath: "/mcp", MCPFromPortal: true, ManifestMCPFunnel: true}
	if got := mcpFunnelLine(off); !strings.HasPrefix(got, "off") {
		t.Errorf("turned off on the portal = %q", got)
	}
}
