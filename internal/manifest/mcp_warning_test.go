package manifest

import (
	"strings"
	"testing"
)

// mcp = true without mcp_path is what `outpost add` writes, and the gateway
// then answers /mcp/<name> with a 401 that says nothing about why. It's a
// warning rather than an error: the service itself runs fine.
func TestWarnings_MCPWithoutPath(t *testing.T) {
	services := []*Service{
		{Name: "yarr", MCP: true},
		{Name: "sous", MCP: true, MCPPath: "/mcp"},
		{Name: "wiki"},
		{Name: "notes", MCPPath: "/mcp"},
	}

	warns := Warnings(services, Options{Gateway: true})
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want 1 (yarr only): %v", len(warns), warns)
	}
	msg := warns[0].Error()
	for _, want := range []string{"yarr", "mcp_path", "/mcp/yarr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q doesn't mention %q", msg, want)
		}
	}
}

// Without the gateway nothing routes /mcp/<name>, so mcp_path does nothing
// and its absence isn't worth a word.
func TestWarnings_NoneWithoutTheGateway(t *testing.T) {
	services := []*Service{{Name: "yarr", MCP: true}}
	if warns := Warnings(services, Options{RequireTailscale: true}); len(warns) != 0 {
		t.Errorf("gateway off: got %v, want no warnings", warns)
	}
}

// A warning never makes a manifest invalid.
func TestWarnings_DontFailValidation(t *testing.T) {
	svc := &Service{
		Name: "yarr", Runtime: "process", Binary: "/bin/true", Port: 9100,
		Storage: "/tmp/yarr", TailscaleName: "yarr", Healthcheck: "/up", MCP: true,
	}
	if errs := ValidateWith([]*Service{svc}, Options{Gateway: true, RequireDNSName: true}); len(errs) != 0 {
		t.Errorf("mcp = true without mcp_path should validate: %v", errs)
	}
}
