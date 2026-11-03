package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// --mcp is documented as "an MCP endpoint at /mcp", and the gateway routes
// /mcp/<name> only by mcp_path, so add writes both.
func TestAdd_MCPWritesMCPPath(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep", "--mcp"); err != nil {
		t.Fatal(err)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.MCPPath != "/mcp" {
		t.Errorf("mcp_path = %q, want /mcp", svc.MCPPath)
	}
}

// Without --mcp there's no endpoint to point at, and nothing is written.
func TestAdd_NoMCPNoMCPPath(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "mcp_path") {
		t.Errorf("manifest has mcp_path without --mcp:\n%s", body)
	}
}

// A release's descriptor saying mcp = true gets the same: the helloDescriptor
// fixture declares it.
func TestAdd_FromGitHubURLWritesMCPPath(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 0\n"), helloDescriptor, true))
	if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "hello.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.MCPPath != "/mcp" {
		t.Errorf("mcp_path = %q, want /mcp", svc.MCPPath)
	}
}
