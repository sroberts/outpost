// Package config is Outpost's global settings file, ~/.outpost/config.toml
// (spec §9). It holds what applies to the installation as a whole; anything
// per-service belongs in that service's manifest.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// Gateway is the login front door. Off unless enabled.
	Gateway Gateway `toml:"gateway"`
	// AI is the models apps are given. Left out of Fingerprint's JSON, which
	// adds it only when set, so a file without it fingerprints as it did
	// before [ai] existed.
	AI AI `toml:"ai" json:"-"`
}

// Tailscale settings.
type Tailscale struct {
	// Enabled decides whether the daemon gives services a Tailscale identity.
	// A new install writes false. Left unset it reads as true, because homes
	// from before that change have been running with Tailscale on.
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

// TailscaleSet reports whether the file set tailscale.enabled itself, as
// opposed to leaving it to the default.
func (c Config) TailscaleSet() bool { return c.Tailscale.Enabled != nil }

// Load reads path. A missing config file is the normal case, not an error:
// Outpost works with no configuration at all.
//
// The file is decoded into a zero Config rather than over Defaults, so a
// setting it leaves out stays unset and TailscaleSet can say so; the getters
// supply the defaults.
func Load(path string) (Config, error) {
	var cfg Config

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	if _, err := toml.Decode(string(body), &cfg); err != nil {
		return Defaults(), fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Defaults(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// HasTailscaleSetting reports whether the file at path sets
// tailscale.enabled. A missing file has no setting.
func HasTailscaleSetting(path string) (bool, error) {
	cfg, err := Load(path)
	if err != nil {
		return false, err
	}
	return cfg.TailscaleSet(), nil
}

// Validate checks the settings make sense.
func (c Config) Validate() error {
	if err := c.Gateway.validate(); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if err := c.AI.validate(); err != nil {
		return fmt.Errorf("ai: %w", err)
	}
	if c.Gateway.TailscaleName != "" && (!c.Gateway.Enabled || !c.TailscaleEnabled()) {
		return errors.New("gateway: tailscale_name puts the portal on the tailnet, so it needs both the gateway " +
			"([gateway] enabled = true) and Tailscale ([tailscale] enabled = true); turn them on or remove tailscale_name")
	}
	return nil
}

// Save writes the configuration, creating the directory if needed.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// The file may hold a Tailscale auth key, so keep it owner-only. The
	// mode WriteFile would give applies only to a new file, and one written
	// by hand is often world-readable, so it is set on every save, before
	// anything is written. Truncated only after the chmod succeeds, so a
	// file this user may write but not chmod keeps its contents.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close() // the chmod failure is the one to report
		return err
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close() // the truncate failure is the one to report
		return err
	}
	if _, err := f.WriteString(cfg.Render()); err != nil {
		_ = f.Close() // the write failure is the one to report
		return err
	}
	return f.Close()
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
	b.WriteString("\n")
	c.Gateway.render(&b)
	b.WriteString("\n")
	c.AI.render(&b)
	return b.String()
}

// ResolveAuthKey returns the auth key to use, preferring the environment.
func (c Config) ResolveAuthKey() string {
	if key := os.Getenv("TS_AUTHKEY"); key != "" {
		return key
	}
	return c.Tailscale.AuthKey
}

// Fingerprint identifies the settings, so a running daemon can be compared
// with the file on disk. It covers what the file means, not how it is
// written: comments, ordering and a setting left unset versus written out
// as its default don't change it, and nor do gateway settings while the
// gateway is off, since none of them is read then.
func (c Config) Fingerprint() string {
	enabled := c.TailscaleEnabled()
	c.Tailscale.Enabled = &enabled
	if c.Gateway.Enabled {
		c.Gateway.Listen = c.Gateway.ListenAddr()
		c.Gateway.Name = c.Gateway.PortalName()
	} else {
		c.Gateway = Gateway{}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "" // can't happen: Config is plain data
	}
	ai := c.AI
	ai.Listen, ai.OllamaURL = c.AI.ListenAddr(), c.AI.OllamaBase()
	if ai != (AI{Listen: DefaultAIListen, OllamaURL: DefaultOllamaURL}) {
		more, err := json.Marshal(ai)
		if err != nil {
			return "" // can't happen: AI is plain data
		}
		body = append(body, more...)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}
