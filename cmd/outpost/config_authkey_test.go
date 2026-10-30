package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/config"
)

func authKeyIn(t *testing.T, home string) string {
	t.Helper()
	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Tailscale.AuthKey
}

// The key goes in from stdin, keeps the rest of the file, and comes out
// with --no-authkey.
func TestConfigCmd_AuthKeyFromStdin(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	home := newHome(t)
	if _, _, err := runCLI(t, home, "config", "--tailscale", "on"); err != nil {
		t.Fatal(err)
	}

	withStdin(t, "  tskey-auth-abc123 \n")
	out, _, err := runCLI(t, home, "config", "--authkey")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "tskey-auth-abc123") {
		t.Errorf("the key was echoed: %q", out)
	}
	if got := authKeyIn(t, home); got != "tskey-auth-abc123" {
		t.Errorf("auth_key = %q", got)
	}
	if explicit, on := setting(t, home); !explicit || !on {
		t.Error("saving the key lost the tailscale setting")
	}
	if fi, err := os.Stat(filepath.Join(home, "config.toml")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	if _, _, err := runCLI(t, home, "config", "--no-authkey"); err != nil {
		t.Fatal(err)
	}
	if got := authKeyIn(t, home); got != "" {
		t.Errorf("auth_key after --no-authkey = %q", got)
	}
}

func TestConfigCmd_AuthKeyFromTerminal(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "")
	home := newHome(t)
	asked := withTerminal(t, "tskey-auth-typed")
	if _, _, err := runCLI(t, home, "config", "--authkey"); err != nil {
		t.Fatal(err)
	}
	if *asked != 1 || authKeyIn(t, home) != "tskey-auth-typed" {
		t.Errorf("asked %d times; auth_key = %q", *asked, authKeyIn(t, home))
	}
}

// Nothing, or something that isn't a key, changes nothing.
func TestConfigCmd_AuthKeyRefusesWhatIsNotAKey(t *testing.T) {
	home := newHome(t)
	for _, in := range []string{"", "\n", "hunter2\n"} {
		withStdin(t, in)
		if _, _, err := runCLI(t, home, "config", "--authkey"); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Error("a refused key still wrote config.toml")
	}
	if _, _, err := runCLI(t, home, "config", "--authkey", "--no-authkey"); err == nil {
		t.Error("--authkey with --no-authkey was accepted")
	}
}

// `outpost config` says a key is set without printing it.
func TestConfigCmd_ShowHidesTheAuthKey(t *testing.T) {
	home := newHome(t)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[tailscale]\nenabled = true\nauth_key = \"tskey-secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, home, "config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "tskey-secret") || !strings.Contains(out, "# auth_key is set; not shown") {
		t.Errorf("config output:\n%s", out)
	}
	// Saved back as the file, as people do, it carries no key at all.
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := authKeyIn(t, home); got != "" {
		t.Errorf("output saved as config.toml has auth_key %q", got)
	}
}
