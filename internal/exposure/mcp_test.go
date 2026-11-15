package exposure

import (
	"path/filepath"
	"testing"
)

// The MCP setting is kept, reset and reopened apart from public paths and
// Funnel, so changing either leaves the other alone.
func TestStore_MCPFunnelIsApartFromPublicPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exposure.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MCPOverride("sous"); ok {
		t.Error("a new store has an MCP setting")
	}
	if err := s.SetMCPFunnel("sous", true, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMCPFunnel("yarr", false, "root"); err != nil {
		t.Fatal(err)
	}
	if on, ok := s.MCPOverride("sous"); !ok || !on {
		t.Errorf("sous = %v %v; want on, set", on, ok)
	}
	if on, ok := s.MCPOverride("yarr"); !ok || on {
		t.Errorf("yarr = %v %v; want off, set", on, ok)
	}
	if _, _, ok := s.Override("sous"); ok {
		t.Error("setting the MCP endpoint set public paths and Funnel too")
	}

	if err := s.Set("sous", true, []string{"/hook"}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset("sous"); err != nil {
		t.Fatal(err)
	}
	if on, ok := s.MCPOverride("sous"); !ok || !on {
		t.Error("resetting public paths reset the MCP endpoint too")
	}
	if err := s.Set("sous", true, []string{"/hook"}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetMCPFunnel("sous"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MCPOverride("sous"); ok {
		t.Error("ResetMCPFunnel left the setting")
	}
	if _, _, ok := s.Override("sous"); !ok {
		t.Error("resetting the MCP endpoint reset public paths too")
	}
	if err := s.SetMCPFunnel("", true, "root"); err == nil {
		t.Error("a setting with no service was kept")
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if on, ok := s.MCPOverride("yarr"); !ok || on {
		t.Errorf("after reopening, yarr = %v %v", on, ok)
	}
	if _, ok := s.MCPOverride("sous"); ok {
		t.Error("after reopening, sous's reset setting is back")
	}
}
