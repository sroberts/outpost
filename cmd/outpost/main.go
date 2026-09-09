// Command outpost supervises a declared set of local services.
//
// This is the step-4 shape from the spec's build order: the CLI drives an
// in-process supervisor, and `outpost serve` is the long-running loop. The
// daemon/Unix-socket split (§12) replaces the state-store coordination used by
// start/stop/restart here without changing these commands' behaviour.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const usage = `outpost — supervise a declared set of local services

Usage:
  outpost <command> [flags]

Commands:
  serve [--tailnet]        run the daemon (reconcile loop + control socket)
  apply                    validate manifests and reconcile
  list                     table of services and their status
  status <service>         health, restart count and recent events
  logs <service> [-f]      show or follow a service's logs
  start <service>          start a stopped service
  stop <service>           stop a running service
  restart <service>        restart a service
  backup [service]         archive a service's storage (all services if omitted)
  backup --list <service>  list a service's existing backups
  restore <service> [ts]   restore storage from a backup (newest if ts omitted)
  install [--system]       install the launchd/systemd service that keeps
                           the daemon running across reboots
  uninstall [--system]     stop that service and remove it

Flags:
  --home <dir>             Outpost root (default $OUTPOST_HOME or ~/.outpost)
  -h, --help               show this help
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "outpost: %v\n", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

// app carries everything a command needs, so commands stay testable.
type app struct {
	home string
	out  io.Writer
	err  io.Writer
}

func run(args []string, stdout, stderr io.Writer) error {
	home, rest, err := parseGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errUsage
	}

	a := &app{home: home, out: stdout, err: stderr}
	cmd, cmdArgs := rest[0], rest[1:]

	switch cmd {
	case "serve":
		return a.cmdServe(cmdArgs)
	case "apply":
		return a.cmdApply(cmdArgs)
	case "list", "ls":
		return a.cmdList(cmdArgs)
	case "status":
		return a.cmdStatus(cmdArgs)
	case "logs":
		return a.cmdLogs(cmdArgs)
	case "start":
		return a.cmdStart(cmdArgs)
	case "stop":
		return a.cmdStop(cmdArgs)
	case "restart":
		return a.cmdRestart(cmdArgs)
	case "backup":
		return a.cmdBackup(cmdArgs)
	case "restore":
		return a.cmdRestore(cmdArgs)
	case "install":
		return a.cmdInstall(cmdArgs)
	case "uninstall":
		return a.cmdUninstall(cmdArgs)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try `outpost help`)", cmd)
	}
}

// parseGlobals pulls --home out of the argument list ahead of the subcommand,
// so it can be given on either side of the command name.
func parseGlobals(args []string) (home string, rest []string, err error) {
	home = os.Getenv("OUTPOST_HOME")

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--home":
			if i+1 >= len(args) {
				return "", nil, errors.New("--home requires a directory")
			}
			home = args[i+1]
			i++
		case strings.HasPrefix(arg, "--home="):
			home = strings.TrimPrefix(arg, "--home=")
		default:
			rest = append(rest, arg)
		}
	}

	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("locate home directory: %w", err)
		}
		home = filepath.Join(h, ".outpost")
	}
	return home, rest, nil
}

// Layout of the Outpost root.
func (a *app) servicesDir() string { return filepath.Join(a.home, "services") }
func (a *app) statePath() string   { return filepath.Join(a.home, "state.db") }
func (a *app) logsDir() string     { return filepath.Join(a.home, "logs") }
func (a *app) pidPath() string     { return filepath.Join(a.home, "outpost.pid") }
func (a *app) socketPath() string  { return filepath.Join(a.home, "outpost.sock") }
func (a *app) backupsDir() string  { return filepath.Join(a.home, "backups") }
func (a *app) tsnetDir() string    { return filepath.Join(a.home, "tsnet") }

// ensureLayout creates the Outpost root on first use.
func (a *app) ensureLayout() error {
	for _, dir := range []string{a.home, a.servicesDir(), a.logsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}
