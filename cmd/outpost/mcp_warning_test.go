package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `outpost apply` says when a manifest has mcp = true but no mcp_path, as
// the gateway then can't route /mcp/<name>, and still applies: it's a
// warning, not an error.
func TestCLI_ApplyWarnsMCPWithoutPath(t *testing.T) {
	home := newHome(t)
	cfg := "[tailscale]\nenabled = false\n\n[gateway]\nenabled = true\npublic_url = \"https://outpost.example.test\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, home, "yarr", freePort(t))
	f, err := os.OpenFile(filepath.Join(home, "services", "yarr.toml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("mcp = true\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runCLI(t, home, "apply")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "1 service(s) valid") {
		t.Errorf("apply didn't validate the manifest:\n%s", out)
	}
	if !strings.Contains(errOut, "warning: yarr: mcp = true but no mcp_path") {
		t.Errorf("apply didn't warn about the missing mcp_path; stderr:\n%s", errOut)
	}
}
