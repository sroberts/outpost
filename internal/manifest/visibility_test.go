package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func visibleSvc(visibility string) *Service {
	return &Service{
		Name: "quiet", Runtime: "process", Binary: "/bin/true", Port: 9400,
		Storage: "/tmp/quiet", Healthcheck: "/up", Visibility: visibility,
	}
}

func onlyError(t *testing.T, errs []error, want ...string) {
	t.Helper()
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(errs), errs)
	}
	for _, w := range want {
		if !strings.Contains(errs[0].Error(), w) {
			t.Errorf("error %q doesn't mention %q", errs[0], w)
		}
	}
}

var tailnetGateway = Options{RequireTailscale: true, Gateway: true, RequireDNSName: true}

// Unset and "tailnet" are the same thing, so an existing manifest still has
// to name its node with Tailscale on.
func TestVisibility_DefaultIsTailnet(t *testing.T) {
	for _, v := range []string{"", VisibilityTailnet} {
		s := visibleSvc(v)
		if s.GatewayOnly() || s.VisibilityOrDefault() != VisibilityTailnet {
			t.Errorf("visibility %q: GatewayOnly=%v, default %q", v, s.GatewayOnly(), s.VisibilityOrDefault())
		}
		onlyError(t, ValidateWith([]*Service{s}, tailnetGateway), "tailscale_name is required")
	}
}

func TestVisibility_GatewayNeedsNoTailscaleName(t *testing.T) {
	if errs := ValidateWith([]*Service{visibleSvc(VisibilityGateway)}, tailnetGateway); len(errs) != 0 {
		t.Errorf("gateway-only without tailscale_name: %v", errs)
	}
}

func TestVisibility_GatewayRefusesTailscaleName(t *testing.T) {
	s := visibleSvc(VisibilityGateway)
	s.TailscaleName = "quiet"
	onlyError(t, ValidateWith([]*Service{s}, tailnetGateway), "tailscale_name", "no tailnet node")
}

func TestVisibility_UnknownValue(t *testing.T) {
	onlyError(t, ValidateWith([]*Service{visibleSvc("public")}, Options{Gateway: true}), `visibility must be "tailnet" or "gateway"`)
}

// Funnel, mcp_funnel and tcp all live on the node, so each says that's why
// it can't be had, rather than asking for a tailscale_name it would refuse.
func TestVisibility_GatewayRefusesWhatNeedsANode(t *testing.T) {
	for field, set := range map[string]func(*Service){
		"funnel":     func(s *Service) { s.Funnel, s.PublicPaths = true, []string{"/hook"} },
		"mcp_funnel": func(s *Service) { s.MCPFunnel, s.MCPPath = true, "/mcp" },
		"tcp":        func(s *Service) { s.TCP = map[string]int{"22": 2222} },
	} {
		s := visibleSvc(VisibilityGateway)
		set(s)
		errs := ValidateWith([]*Service{s}, tailnetGateway)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), field+" is served on the service's tailnet node") ||
			!strings.Contains(errs[0].Error(), `visibility = "gateway"`) {
			t.Errorf("%s: %v", field, errs)
		}
	}
}

// The portal's Internet access setting goes through the same rules, so it
// can't turn Funnel on for a gateway-only app either.
func TestVisibility_PortalCantFunnelIt(t *testing.T) {
	s := visibleSvc(VisibilityGateway)
	if _, errs := s.WithExposure(true, []string{"/hook"}, tailnetGateway); len(errs) != 1 {
		t.Errorf("WithExposure: %v; want the visibility error", errs)
	}
	s.MCPPath = "/mcp"
	if _, errs := s.WithMCPFunnel(true, tailnetGateway); len(errs) != 1 {
		t.Errorf("WithMCPFunnel: %v; want the visibility error", errs)
	}
}

func TestVisibility_Warnings(t *testing.T) {
	gw := visibleSvc(VisibilityGateway)
	if w := Warnings([]*Service{gw}, Options{}); len(w) != 1 || !strings.Contains(w[0].Error(), "gateway is off") {
		t.Errorf("gateway off: %v", w)
	}
	// The portal's node as the domain: MagicDNS resolves nothing under it.
	w := Warnings([]*Service{gw}, Options{Gateway: true, AppDomain: "outpost.tail1234.ts.net"})
	if len(w) != 1 || !strings.Contains(w[0].Error(), "quiet.outpost.tail1234.ts.net") {
		t.Errorf("ts.net domain: %v", w)
	}
	// Wildcard DNS of its own, or a node-shaped hostname, is fine.
	if w := Warnings([]*Service{gw}, Options{Gateway: true, AppDomain: "outpost.example"}); len(w) != 0 {
		t.Errorf("own domain: %v", w)
	}
	named := visibleSvc(VisibilityGateway)
	named.Hostname = "quiet.tail1234.ts.net"
	if w := Warnings([]*Service{named}, Options{Gateway: true, AppDomain: "outpost.tail1234.ts.net"}); len(w) != 0 {
		t.Errorf("node-shaped hostname: %v", w)
	}
	// None of it for the default.
	if w := Warnings([]*Service{visibleSvc("")}, Options{Gateway: true, AppDomain: "outpost.tail1234.ts.net"}); len(w) != 0 {
		t.Errorf("tailnet app: %v", w)
	}
}

func TestVisibility_LoadsFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet.toml")
	if err := os.WriteFile(path, []byte(`
runtime    = "process"
binary     = "/bin/true"
port       = 9401
storage    = "/tmp/q"
visibility = "gateway"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.GatewayOnly() {
		t.Errorf("visibility = %q; want gateway", s.Visibility)
	}
}
