package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpFunnelService() *Service {
	s := funnelService()
	s.PublicPaths = nil
	s.MCPPath = "/mcp"
	s.MCPFunnel = true
	return s
}

// Unlike funnel, mcp_funnel needs no public_paths, and the two go together.
func TestMCPFunnel_Valid(t *testing.T) {
	if errs := ValidateWith([]*Service{mcpFunnelService()}, withGateway); len(errs) != 0 {
		t.Errorf("mcp_funnel alone: %v", errs)
	}
	both := mcpFunnelService()
	both.Funnel, both.PublicPaths = true, []string{"/webhooks/github"}
	if errs := ValidateWith([]*Service{both}, withGateway); len(errs) != 0 {
		t.Errorf("funnel and mcp_funnel: %v", errs)
	}
}

func TestMCPFunnel_RefusesWhatWouldExposeMoreOrNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Service, *Options)
		want string
	}{
		"no gateway":        {func(_ *Service, o *Options) { o.Gateway = false }, "mcp_funnel needs the gateway"},
		"no mcp_path":       {func(s *Service, _ *Options) { s.MCPPath = "" }, "there is none"},
		"no tailscale_name": {func(s *Service, _ *Options) { s.TailscaleName = "" }, "mcp_funnel is served on the service's tailnet node"},
		"no tailscale":      {func(_ *Service, o *Options) { o.RequireTailscale = false }, "mcp_funnel is served on the service's tailnet node"},
	} {
		s, opts := mcpFunnelService(), withGateway
		tc.edit(s, &opts)
		errs := ValidateWith([]*Service{s}, opts)
		found := false
		for _, err := range errs {
			found = found || strings.Contains(err.Error(), tc.want)
		}
		if !found {
			t.Errorf("%s: errors %v; want one saying %q", name, errs, tc.want)
		}
	}
}

func TestMCPFunnel_ParsesFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sous.toml")
	if err := os.WriteFile(path, []byte(`
name = "sous"
runtime = "process"
binary = "/usr/bin/true"
port = 8080
storage = "/tmp/sous"
tailscale_name = "sous"
mcp_path = "/mcp"
mcp_funnel = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.MCPFunnel {
		t.Error("mcp_funnel = true wasn't read")
	}
}

func TestFunnelListener(t *testing.T) {
	for _, tc := range []struct{ funnel, mcp, want bool }{
		{false, false, false}, {true, false, true}, {false, true, true}, {true, true, true},
	} {
		s := &Service{Funnel: tc.funnel, MCPFunnel: tc.mcp}
		if got := s.FunnelListener(); got != tc.want {
			t.Errorf("funnel %v, mcp_funnel %v: %v", tc.funnel, tc.mcp, got)
		}
	}
}
