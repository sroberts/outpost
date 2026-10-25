package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasTailscaleSetting(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		body string // "" means no file
		want bool
	}{
		"no file":        {"", false},
		"empty file":     {"\n", false},
		"auth key only":  {"[tailscale]\nauth_key = \"tskey-a\"\n", false},
		"explicit true":  {"[tailscale]\nenabled = true\n", true},
		"explicit false": {"[tailscale]\nenabled = false\n", true},
		"commented out":  {"[tailscale]\n# enabled = false\n", false},
	}
	for name, c := range cases {
		path := filepath.Join(dir, name+".toml")
		if c.body != "" {
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := HasTailscaleSetting(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: HasTailscaleSetting = %v; want %v", name, got, c.want)
		}
	}

	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("not = valid ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := HasTailscaleSetting(bad); err == nil {
		t.Error("a malformed file reported no error")
	}
}

// What Save writes is explicit, so a home whose answer was recorded is never
// asked again.
func TestHasTailscaleSetting_AfterSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	off := false
	if err := Save(path, Config{Tailscale: Tailscale{Enabled: &off}}); err != nil {
		t.Fatal(err)
	}
	if got, err := HasTailscaleSetting(path); err != nil || !got {
		t.Errorf("HasTailscaleSetting after Save = %v, %v; want true", got, err)
	}
}
