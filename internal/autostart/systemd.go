package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemd renders a Linux systemd unit.
//
// User scope writes a user unit, which needs no privileges but by default only
// runs while the user has a session — `loginctl enable-linger` fixes that.
// System scope writes a system unit that starts at boot, and needs root.
type systemd struct{}

func (systemd) Name() string { return "systemd" }

func (systemd) FileMode() os.FileMode { return 0o644 }

// unitName is the systemd unit filename: "outpost.service" from the
// "com.sroberts.outpost" style label, so `systemctl status outpost` works.
func unitName(label string) string {
	name := label
	if i := strings.LastIndex(label, "."); i >= 0 && i < len(label)-1 {
		name = label[i+1:]
	}
	return name + ".service"
}

func (systemd) UnitPath(cfg Config) (string, error) {
	if cfg.Scope == System {
		return filepath.Join("/etc/systemd/system", unitName(cfg.Label)), nil
	}
	// Honour XDG_CONFIG_HOME, which is where systemd itself looks.
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName(cfg.Label)), nil
}

func (systemd) Render(cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Outpost service supervisor\n")
	b.WriteString("Documentation=https://github.com/sroberts/outpost\n")
	// Services bind loopback and Outpost may reach the tailnet, so wait for
	// the network to be configured rather than merely present.
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	// StartLimit* live in [Unit], not [Service], since systemd 229; in
	// [Service] they are ignored with a warning. 0 disables the rate limit so
	// Restart=always is never given up on.
	b.WriteString("StartLimitIntervalSec=0\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")

	fmt.Fprintf(&b, "ExecStart=%s --home %s serve\n",
		systemdQuote(cfg.Binary), systemdQuote(cfg.Home))
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdQuote(cfg.Home))

	// Always bring the daemon back, including after a clean exit, and throttle
	// so a daemon that fails immediately does not spin.
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")

	// SIGTERM only the daemon, so it can stop its services in order; systemd
	// SIGKILLs whatever is left once the timeout expires.
	b.WriteString("KillMode=mixed\n")
	b.WriteString("KillSignal=SIGTERM\n")
	b.WriteString("TimeoutStopSec=60\n")

	for _, kv := range cfg.sortedEnv() {
		fmt.Fprintf(&b, "Environment=%s=%s\n", kv[0], systemdQuote(kv[1]))
	}

	if cfg.Scope == System {
		fmt.Fprintf(&b, "User=%s\n", systemdQuote(cfg.User))
	}

	// Outpost's own output goes to the journal; per-service logs are files
	// under the Outpost root either way.
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	fmt.Fprintf(&b, "SyslogIdentifier=%s\n", strings.TrimSuffix(unitName(cfg.Label), ".service"))

	b.WriteString("\n[Install]\n")
	if cfg.Scope == System {
		b.WriteString("WantedBy=multi-user.target\n")
	} else {
		b.WriteString("WantedBy=default.target\n")
	}
	return b.String(), nil
}

func (systemd) EnableCommands(unitPath string, cfg Config) [][]string {
	unit := unitName(cfg.Label)
	if cfg.Scope == System {
		return [][]string{
			{"sudo", "systemctl", "daemon-reload"},
			{"sudo", "systemctl", "enable", "--now", unit},
		}
	}
	return [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", unit},
		// Without lingering, a user unit stops at logout and does not start at
		// boot — which is most of the point of installing it.
		{"sudo", "loginctl", "enable-linger", currentUser()},
	}
}

func (systemd) DisableCommands(unitPath string, cfg Config) [][]string {
	unit := unitName(cfg.Label)
	if cfg.Scope == System {
		return [][]string{
			{"sudo", "systemctl", "disable", "--now", unit},
			{"sudo", "systemctl", "daemon-reload"},
		}
	}
	return [][]string{
		{"systemctl", "--user", "disable", "--now", unit},
		{"systemctl", "--user", "daemon-reload"},
	}
}

func (systemd) Notes(cfg Config) []string {
	unit := strings.TrimSuffix(unitName(cfg.Label), ".service")
	if cfg.Scope == System {
		return []string{
			"This is a system unit: it starts at boot, with no one logged in.",
			"Writing it and enabling it both need root.",
			fmt.Sprintf("Logs:  journalctl -u %s -f", unit),
		}
	}
	return []string{
		"This is a user unit. By default it stops when you log out and does not",
		"start at boot; `loginctl enable-linger` above fixes both, and needs root.",
		"To start at boot without lingering, install with --system instead.",
		fmt.Sprintf("Logs:  journalctl --user -u %s -f", unit),
	}
}

// currentUser is the account a user unit belongs to.
func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "$USER"
}

// systemdQuote quotes a value for a unit file when it needs it.
//
// Unit parsing splits ExecStart on whitespace and treats several characters
// specially, so a path containing a space or a quote would otherwise be
// silently mis-parsed into multiple arguments.
func systemdQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'\\$%;\n") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '%':
			// % introduces a specifier in unit files; %% is a literal percent.
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
