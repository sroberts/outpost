package autostart

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// privileged prefixes a command with sudo unless it is already running as
// root. It takes the uid rather than asking the OS so both cases can be tested
// without being root, and so these commands don't add to #19, where a root
// install fails on a machine with no sudo.
func privileged(uid int, argv ...string) []string {
	if uid == 0 {
		return argv
	}
	return append([]string{"sudo"}, argv...)
}

// homeFromArgs finds the Outpost root in a daemon's command line.
func homeFromArgs(args []string) (string, error) {
	for i, arg := range args {
		if arg == "--home" && i+1 < len(args) {
			return args[i+1], nil
		}
		if home, ok := strings.CutPrefix(arg, "--home="); ok {
			return home, nil
		}
	}
	return "", errors.New("its command line has no --home")
}

// launchdTarget names a job the way launchctl's newer verbs want it. The gui
// domain is the logged-in user's, where a LaunchAgent loads.
func launchdTarget(cfg Config, uid int) string {
	if cfg.Scope == System {
		return "system/" + cfg.Label
	}
	return fmt.Sprintf("gui/%d/%s", uid, cfg.Label)
}

// RestartCommands uses kickstart because `launchctl load` does nothing for a
// job that is already loaded. -k is what makes it replace a running daemon.
func (launchd) RestartCommands(cfg Config, uid int) [][]string {
	argv := []string{"launchctl", "kickstart", "-k", launchdTarget(cfg, uid)}
	if cfg.Scope == System {
		argv = privileged(uid, argv...)
	}
	return [][]string{argv}
}

func (launchd) LoadedCommand(cfg Config, uid int) []string {
	return []string{"launchctl", "print", launchdTarget(cfg, uid)}
}

// ReloadCommands exists because kickstart restarts the definition launchd
// loaded, not the file: only unloading makes it read a rewritten plist.
func (launchd) ReloadCommands(unitPath string, cfg Config, uid int) [][]string {
	unload := []string{"launchctl", "unload", "-w", unitPath}
	load := []string{"launchctl", "load", "-w", unitPath}
	if cfg.Scope == System {
		unload, load = privileged(uid, unload...), privileged(uid, load...)
	}
	return [][]string{unload, load}
}

// EnableIsIdempotent is false: `launchctl load` on a loaded job fails or does
// nothing, depending on the macOS version.
func (launchd) EnableIsIdempotent() bool { return false }

func (launchd) LogHint(cfg Config) string {
	return filepath.Join(cfg.Home, "logs", "outpost.err.log")
}

func (launchd) UnitHome(unit string) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(unit))
	var key string
	var args []string
	inArgs := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return homeFromArgs(args)
		}
		if err != nil {
			return "", fmt.Errorf("parse plist: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "key":
				if err := dec.DecodeElement(&key, &t); err != nil {
					return "", fmt.Errorf("parse plist: %w", err)
				}
			case t.Name.Local == "array" && key == "ProgramArguments":
				inArgs = true
			case t.Name.Local == "string" && inArgs:
				var arg string
				if err := dec.DecodeElement(&arg, &t); err != nil {
					return "", fmt.Errorf("parse plist: %w", err)
				}
				args = append(args, arg)
			}
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				return homeFromArgs(args)
			}
		}
	}
}

// systemctl builds a systemctl command for the job's scope.
func systemctl(cfg Config, uid int, args ...string) []string {
	if cfg.Scope == System {
		return privileged(uid, append([]string{"systemctl"}, args...)...)
	}
	return append([]string{"systemctl", "--user"}, args...)
}

// RestartCommands reloads first because systemd otherwise restarts the unit
// as it last read it, and the upgrade steps have just rewritten the file.
func (systemd) RestartCommands(cfg Config, uid int) [][]string {
	return [][]string{
		systemctl(cfg, uid, "daemon-reload"),
		systemctl(cfg, uid, "restart", unitName(cfg.Label)),
	}
}

// LoadedCommand asks is-enabled rather than is-active: a daemon that is
// crash-looping reads as inactive between attempts, and that is a daemon
// someone may well be trying to restart. It needs no root at either scope.
func (systemd) LoadedCommand(cfg Config, uid int) []string {
	if cfg.Scope == System {
		return []string{"systemctl", "is-enabled", "--quiet", unitName(cfg.Label)}
	}
	return []string{"systemctl", "--user", "is-enabled", "--quiet", unitName(cfg.Label)}
}

// ReloadCommands is empty: RestartCommands already reads a rewritten unit.
func (systemd) ReloadCommands(string, Config, int) [][]string { return nil }

// EnableIsIdempotent is true: `enable --now` on an enabled unit is harmless,
// and it still does something for one that was stopped by hand.
func (systemd) EnableIsIdempotent() bool { return true }

func (systemd) LogHint(cfg Config) string {
	unit := strings.TrimSuffix(unitName(cfg.Label), ".service")
	if cfg.Scope == System {
		return "journalctl -u " + unit + " -e"
	}
	return "journalctl --user -u " + unit + " -e"
}

func (systemd) UnitHome(unit string) (string, error) {
	for line := range strings.SplitSeq(unit, "\n") {
		if exec, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart="); ok {
			return homeFromArgs(systemdFields(exec))
		}
	}
	return "", errors.New("it has no ExecStart")
}

// systemdFields splits a command line written by systemdQuote back into its
// arguments.
func systemdFields(line string) []string {
	var fields []string
	var cur strings.Builder
	inField, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\\' && i+1 < len(line):
			i++
			if line[i] == 'n' {
				cur.WriteByte('\n')
			} else {
				cur.WriteByte(line[i])
			}
		case c == '"':
			quoted, inField = !quoted, true
		case c == '%' && i+1 < len(line) && line[i+1] == '%':
			i++
			cur.WriteByte('%')
			inField = true
		case !quoted && (c == ' ' || c == '\t'):
			if inField {
				fields = append(fields, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteByte(c)
			inField = true
		}
	}
	if inField {
		fields = append(fields, cur.String())
	}
	return fields
}
