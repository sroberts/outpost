package manifest

import (
	"strings"
	"testing"
)

func TestValidate_MCPPath(t *testing.T) {
	good := []string{"", "/mcp", "/", "/api/v1/mcp", "/.well-known-ish/mcp"}
	bad := map[string]string{
		"mcp":           "must start with",
		"/mcp?x=1":      "without a query",
		"/mcp#top":      "without a query",
		"/mcp/":         "simplest form",
		"/a/../mcp":     "simplest form",
		"/a//mcp":       "simplest form",
		"/mcp/.":        "simplest form",
		"/m%63p":        "escaping",
		"/mcp;x":        "escaping",
		"/m\\cp":        "escaping",
		"/m cp":         "escaping",
		"/.outpost":     "/.outpost/",
		"/.outpost/mcp": "/.outpost/",
		"/mcp\n":        "escaping",
	}
	for _, p := range good {
		s := mcpTestService()
		s.MCPPath = p
		if errs := ValidateWith([]*Service{s}, Options{RequireTailscale: true}); len(errs) != 0 {
			t.Errorf("mcp_path %q: %v", p, errs)
		}
	}
	for p, want := range bad {
		s := mcpTestService()
		s.MCPPath = p
		errs := ValidateWith([]*Service{s}, Options{RequireTailscale: true})
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), want) {
			t.Errorf("mcp_path %q: errs = %v; want one mentioning %q", p, errs, want)
		}
	}
}

func mcpTestService() *Service {
	return &Service{Name: "yarr", Runtime: "process", Binary: "/bin/true", Port: 9001, Storage: "data/yarr",
		Healthcheck: "/up", TailscaleName: "yarr"}
}
