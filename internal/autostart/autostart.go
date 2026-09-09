// Package autostart generates and installs the OS integration that keeps the
// Outpost daemon running: a launchd job on macOS, a systemd unit on Linux.
//
// The init system supervises Outpost itself and nothing else; Outpost
// supervises the services (spec §3). Both backends are always compiled, so the
// Linux generator can be tested from macOS and vice versa.
package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Scope decides when the daemon starts.
type Scope int

const (
	// User installs into the logged-in user's session. No privileges needed,
	// but on macOS the daemon starts at login rather than at boot.
	User Scope = iota
	// System installs a boot-time service. Needs root, and starts without
	// anyone logging in.
	System
)

func (s Scope) String() string {
	if s == System {
		return "system"
	}
	return "user"
}

// Config describes the daemon to be kept alive.
type Config struct {
	// Label identifies the job: a reverse-DNS label for launchd, a unit name
	// for systemd.
	Label string
	// Binary is the absolute path to the outpost executable.
	Binary string
	// Home is the Outpost root passed as --home.
	Home string
	// Scope decides user-level versus boot-level installation.
	Scope Scope
	// User is the account a system-scope unit runs as. Ignored for user scope.
	User string
	// Env is injected into the daemon's environment, for values like
	// TS_AUTHKEY that should not live in a manifest.
	Env map[string]string
}

// DefaultLabel is the job name used unless overridden.
const DefaultLabel = "com.sroberts.outpost"

// Manager renders and locates the init-system integration for one platform.
type Manager interface {
	// Name is the init system: "launchd" or "systemd".
	Name() string
	// UnitPath is where the generated file belongs.
	UnitPath(cfg Config) (string, error)
	// Render produces the file's contents.
	Render(cfg Config) (string, error)
	// FileMode is the mode the generated file should have.
	FileMode() os.FileMode
	// EnableCommands are the commands that start the job and enable it at
	// boot or login, in order.
	EnableCommands(unitPath string, cfg Config) [][]string
	// DisableCommands stop the job and remove it from startup.
	DisableCommands(unitPath string, cfg Config) [][]string
	// Notes are platform caveats worth printing after installation.
	Notes(cfg Config) []string
}

// For returns the Manager for a GOOS value.
func For(goos string) (Manager, error) {
	switch goos {
	case "darwin":
		return launchd{}, nil
	case "linux":
		return systemd{}, nil
	default:
		return nil, fmt.Errorf("no autostart support for %s: run `outpost serve` under whatever supervises services on this platform", goos)
	}
}

// ForHost returns the Manager for the running platform.
func ForHost() (Manager, error) { return For(runtime.GOOS) }

// Validate checks a Config before anything is written.
func (c Config) Validate() error {
	if c.Label == "" {
		return fmt.Errorf("label is required")
	}
	if c.Binary == "" {
		return fmt.Errorf("binary path is required")
	}
	if !filepath.IsAbs(c.Binary) {
		return fmt.Errorf("binary path %q must be absolute", c.Binary)
	}
	if c.Home == "" {
		return fmt.Errorf("home directory is required")
	}
	if !filepath.IsAbs(c.Home) {
		return fmt.Errorf("home directory %q must be absolute", c.Home)
	}
	if c.Scope == System && c.User == "" {
		return fmt.Errorf("a system-scope service must name the user it runs as")
	}
	return nil
}

// sortedEnv returns Env as ordered key/value pairs, so generated files are
// byte-identical across runs and diff cleanly.
func (c Config) sortedEnv() [][2]string {
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([][2]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]string{k, c.Env[k]})
	}
	return out
}
