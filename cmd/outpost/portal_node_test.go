package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/gateway"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// A node in front of the gateway never dials a service, and the portal's has
// none, so only a node without --upstream needs a port.
func TestTsnetNode_PortIsRequiredOnlyWithoutAnUpstream(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{outposttsnet.NodeCommand, "--service", "svc", "--hostname", "svc", "--dir", t.TempDir()}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--port") {
		t.Errorf("error = %v; want it to ask for --port", err)
	}
}

func TestGatewayStatus_ShowsThePortalsNode(t *testing.T) {
	var out strings.Builder
	a := &app{out: &out, err: &out}

	a.printPortalNode(gateway.PortalNode{State: "needs-login", LoginURL: "https://login.tailscale.com/a/x"}, "https://outpost.example.ts.net")
	if got := out.String(); !strings.Contains(got, "needs a login") || !strings.Contains(got, "https://login.tailscale.com/a/x") {
		t.Errorf("waiting for a login:\n%s", got)
	}

	out.Reset()
	a.printPortalNode(gateway.PortalNode{State: "on", Host: "outpost-1.example.ts.net", Renamed: true}, "https://outpost.example.ts.net")
	got := out.String()
	for _, want := range []string{"outpost-1.example.ts.net", "https://outpost.example.ts.net", "config.toml"} {
		if !strings.Contains(got, want) {
			t.Errorf("renamed node: output lacks %q:\n%s", want, got)
		}
	}
}

// The portal's tailnet name is a service's to avoid only while the portal
// is on the tailnet at all.
func TestManifestOptions_CarryThePortalsName(t *testing.T) {
	cfg := config.Config{Gateway: config.Gateway{Enabled: true, PublicURL: "https://outpost.example.ts.net", TailscaleName: "outpost"}}
	a := &app{config: cfg}
	if got := a.manifestOptions(true).PortalTailscaleName; got != "outpost" {
		t.Errorf("PortalTailscaleName = %q; want outpost", got)
	}
	cfg.Gateway.Enabled = false
	a.config = cfg
	if got := a.manifestOptions(true).PortalTailscaleName; got != "" {
		t.Errorf("gateway off: PortalTailscaleName = %q; want none", got)
	}
}
