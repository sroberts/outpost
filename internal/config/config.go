// Package config is Outpost's global settings file, ~/.outpost/config.toml
// (spec §9). It holds what applies to the installation as a whole; anything
// per-service belongs in that service's manifest.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the whole settings file.
type Config struct {
	// Tailscale controls whether services are put on a tailnet at all.
	Tailscale Tailscale `toml:"tailscale"`
}

// Tailscale settings.
type Tailscale struct {
	// Enabled decides whether the daemon gives services a Tailscale identity.
	// Defaults to true: a tailnet identity per service is the point of Outpost.
	// Set it to false to run services on loopback only.
	Enabled *bool `toml:"enabled"`
	// AuthKey registers nodes unattended. TS_AUTHKEY in the environment wins,
	// so a key need not be written to disk.
	AuthKey string `toml:"auth_key"`
}

// Defaults returns the configuration used when there is no file.
func Defaults() Config {
	enabled := true
	return Config{Tailscale: Tailscale{Enabled: &enabled}}
}

// TailscaleEnabled reports whether services should join a tailnet.
func (c Config) TailscaleEnabled() bool {
	if c.Tailscale.Enabled == nil {
		return true // the documented default
	}
	return *c.Tailscale.Enabled
}

// Load reads path, falling back to defaults if it does not exist.
//
// A missing config file is the normal case, not an error: Outpost works with
// no configuration at all.
func Load(path string) (Config, error) {
	cfg := Defaults()

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	// Decode over the defaults so an absent key keeps its default.
	if _, err := toml.Decode(string(body), &cfg); err != nil {
		return Defaults(), fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Defaults(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the settings make sense.
func (c Config) Validate() error { return nil }

// Save writes the configuration, creating the directory if needed.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// The file may hold a Tailscale auth key, so keep it owner-only.
	return os.WriteFile(path, []byte(cfg.Render()), 0o600)
}

// Render produces the file's contents, with comments explaining each setting.
func (c Config) Render() string {
	var b strings.Builder
	b.WriteString("# Outpost global configuration.\n")
	b.WriteString("# Per-service settings belong in services/<name>.toml instead.\n\n")

	b.WriteString("[tailscale]\n")
	b.WriteString("# Whether each service gets its own Tailscale identity.\n")
	b.WriteString("# With this false, services are supervised exactly as usual but stay\n")
	b.WriteString("# on loopback, and tailscale_name is not required in a manifest.\n")
	fmt.Fprintf(&b, "enabled = %t\n", c.TailscaleEnabled())
	b.WriteString("\n# Registers new nodes without a login prompt. TS_AUTHKEY in the\n")
	b.WriteString("# environment takes precedence, which keeps the key off disk.\n")
	if c.Tailscale.AuthKey != "" {
		fmt.Fprintf(&b, "auth_key = %q\n", c.Tailscale.AuthKey)
	} else {
		b.WriteString("# auth_key = \"tskey-...\"\n")
	}
	return b.String()
}

// ResolveAuthKey returns the auth key to use, preferring the environment.
func (c Config) ResolveAuthKey() string {
	if key := os.Getenv("TS_AUTHKEY"); key != "" {
		return key
	}
	return c.Tailscale.AuthKey
}
