package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing config file is the normal case, not an error.
func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TailscaleEnabled() {
		t.Error("Tailscale should default to enabled")
	}
	if cfg.ContainerEngine != "podman" {
		t.Errorf("ContainerEngine = %q; want podman", cfg.ContainerEngine)
	}
}

func TestLoad_ReadsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
container_engine = "docker"

[tailscale]
enabled = false
auth_key = "tskey-abc"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TailscaleEnabled() {
		t.Error("enabled = false was not honoured")
	}
	if cfg.ContainerEngine != "docker" {
		t.Errorf("ContainerEngine = %q; want docker", cfg.ContainerEngine)
	}
	if cfg.Tailscale.AuthKey != "tskey-abc" {
		t.Errorf("AuthKey = %q", cfg.Tailscale.AuthKey)
	}
}

// An absent key keeps its default rather than becoming the zero value.
func TestLoad_PartialFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("container_engine = \"colima\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TailscaleEnabled() {
		t.Error("Tailscale should stay enabled when the file does not mention it")
	}
	if cfg.ContainerEngine != "colima" {
		t.Errorf("ContainerEngine = %q; want colima", cfg.ContainerEngine)
	}
}

func TestLoad_RejectsUnknownEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`container_engine = "kubernetes"`), 0o600)

	if _, err := Load(path); err == nil {
		t.Fatal("unknown container engine was accepted")
	}
}

func TestLoad_RejectsMalformedTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("this is not = valid toml ["), 0o600)

	if _, err := Load(path); err == nil {
		t.Fatal("malformed config was accepted")
	}
}

// Save then Load must round-trip, and the rendered file must be readable.
func TestSaveLoad_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	disabled := false
	cfg := Config{
		Tailscale:       Tailscale{Enabled: &disabled, AuthKey: "tskey-xyz"},
		ContainerEngine: "docker",
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.TailscaleEnabled() {
		t.Error("enabled = false did not survive the round trip")
	}
	if back.ContainerEngine != "docker" || back.Tailscale.AuthKey != "tskey-xyz" {
		t.Errorf("round trip lost settings: %+v", back)
	}
}

// The file can hold an auth key, so it must not be world-readable.
func TestSave_FileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Defaults()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o; want 0600 (it may contain an auth key)", perm)
	}
}

func TestRender_IsCommentedAndParsable(t *testing.T) {
	out := Defaults().Render()
	if !strings.Contains(out, "#") {
		t.Error("rendered config has no explanatory comments")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(out), 0o600)
	if _, err := Load(path); err != nil {
		t.Errorf("rendered config does not parse: %v", err)
	}
}

// TS_AUTHKEY beats the file, so the key need not be written to disk.
func TestResolveAuthKey_EnvironmentWins(t *testing.T) {
	cfg := Config{Tailscale: Tailscale{AuthKey: "from-file"}}
	if got := cfg.ResolveAuthKey(); got != "from-file" {
		t.Errorf("ResolveAuthKey = %q; want the file value", got)
	}

	t.Setenv("TS_AUTHKEY", "from-env")
	if got := cfg.ResolveAuthKey(); got != "from-env" {
		t.Errorf("ResolveAuthKey = %q; want the environment value", got)
	}
}
