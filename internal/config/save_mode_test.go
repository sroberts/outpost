package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config.toml written by hand with the usual umask is world-readable;
// saving to it, as setting an auth key does, makes it owner-only.
func TestSave_MakesAnExistingFileOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tailscale]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Tailscale.AuthKey = "tskey-a"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v; want 0600", fi.Mode().Perm())
	}
}
