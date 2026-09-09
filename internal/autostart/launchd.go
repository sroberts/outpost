package autostart

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// launchd renders a macOS launchd job.
//
// User scope writes a LaunchAgent, which starts at login. System scope writes a
// LaunchDaemon, which starts at boot without anyone logging in — the right
// choice for a headless machine, at the cost of needing root.
type launchd struct{}

func (launchd) Name() string { return "launchd" }

func (launchd) FileMode() os.FileMode { return 0o644 }

func (launchd) UnitPath(cfg Config) (string, error) {
	if cfg.Scope == System {
		return filepath.Join("/Library/LaunchDaemons", cfg.Label+".plist"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", cfg.Label+".plist"), nil
}

func (l launchd) Render(cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")

	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", plistEscape(k)) }
	str := func(v string) { fmt.Fprintf(&b, "\t<string>%s</string>\n", plistEscape(v)) }

	key("Label")
	str(cfg.Label)

	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, arg := range []string{cfg.Binary, "--home", cfg.Home, "serve"} {
		fmt.Fprintf(&b, "\t\t<string>%s</string>\n", plistEscape(arg))
	}
	b.WriteString("\t</array>\n")

	// Start now and at every login/boot, and bring it back if it dies.
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")

	// launchd throttles restarts to one per ThrottleInterval seconds. Without
	// this a daemon that fails immediately spins as fast as launchd allows.
	key("ThrottleInterval")
	b.WriteString("\t<integer>10</integer>\n")

	// Give Outpost time to stop its services before launchd escalates to
	// SIGKILL. Each service gets up to 10s, and shutdown is sequential.
	key("ExitTimeOut")
	b.WriteString("\t<integer>60</integer>\n")

	if env := cfg.sortedEnv(); len(env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, kv := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n",
				plistEscape(kv[0]), plistEscape(kv[1]))
		}
		b.WriteString("\t</dict>\n")
	}

	if cfg.Scope == System {
		// A LaunchDaemon runs as root unless told otherwise; Outpost's state
		// lives in a user's home, so run as that user.
		key("UserName")
		str(cfg.User)
	}

	key("WorkingDirectory")
	str(cfg.Home)
	key("StandardOutPath")
	str(filepath.Join(cfg.Home, "logs", "outpost.out.log"))
	key("StandardErrorPath")
	str(filepath.Join(cfg.Home, "logs", "outpost.err.log"))
	key("ProcessType")
	str("Background")

	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

func (launchd) EnableCommands(unitPath string, cfg Config) [][]string {
	if cfg.Scope == System {
		return [][]string{
			{"sudo", "launchctl", "load", "-w", unitPath},
		}
	}
	return [][]string{
		{"launchctl", "load", "-w", unitPath},
	}
}

func (launchd) DisableCommands(unitPath string, cfg Config) [][]string {
	if cfg.Scope == System {
		return [][]string{{"sudo", "launchctl", "unload", "-w", unitPath}}
	}
	return [][]string{{"launchctl", "unload", "-w", unitPath}}
}

func (launchd) Notes(cfg Config) []string {
	if cfg.Scope == System {
		return []string{
			"This is a LaunchDaemon: it starts at boot, with no one logged in.",
			"Writing it and loading it both need root.",
		}
	}
	return []string{
		"This is a LaunchAgent: it starts when you log in, not when the machine boots.",
		"On a headless machine that reboots unattended, either enable automatic login",
		"or install with --system to start Outpost at boot instead.",
	}
}

// plistEscape escapes text for an XML element body.
//
// The plist embeds paths chosen by the user. Without escaping, a home directory
// containing "&" produces malformed XML that launchd rejects outright, and one
// containing "</string>" could inject arbitrary plist keys.
func plistEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails, which strings.Builder never does.
		return ""
	}
	return b.String()
}
