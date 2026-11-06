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

	"github.com/sroberts/outpost/internal/config"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
	"github.com/sroberts/outpost/internal/version"
)

const usage = `outpost — supervise a declared set of local services

Usage:
  outpost <command> [flags]

Commands:
  serve [--tailnet=BOOL]   run the daemon (reconcile loop + control socket)
  add <service> --binary   declare a new service from a local binary
  add <github-url>         install a service from its latest GitHub release
  update <service>         install a newer release of it, rolling back if the
                           new one fails its healthcheck (--check, --to TAG,
                           --all for every service with a newer release)
  update --self            update Outpost itself, restarting the daemon and
                           every service on it (--check, --to TAG)
  remove <service>         undeclare a service (--purge deletes its data)
  apply                    validate manifests and reconcile
  list                     table of services and their status
  status <service>         health, restart count and recent events
  logs <service> [-f]      show or follow a service's logs
  logs <service> --events  show the service's recent activity
  start <service>          start a stopped service
  stop <service>           stop a running service
  restart <service>        restart a service
  backup [service]         archive a service's storage (all services if omitted)
  backup --list <service>  list a service's existing backups
  restore <service> [ts]   restore storage from a backup (newest if ts omitted)
  tui                      launch the dashboard
  version                  print the build identity of this binary
  config [--init]          show or create the global configuration
  config --tailscale on|off
                           give services their own Tailscale identity, or not
  install [--system]       install the launchd/systemd service that keeps
                           the daemon running across reboots; asks whether to
                           use Tailscale unless --tailscale/--no-tailscale
  uninstall [--system]     stop that service and remove it
  daemon restart           restart the daemon through that service and wait
                           for it to come back (--force, --wait D)

Users:
  user add <name>          add a user (--admin, --group G, --email E,
                           --display-name N, --no-password, --password-stdin)
  user passwd <name>       set a user's password and end their sessions
  user rm <name>           remove a user
  user list                table of users, their groups and sessions
  user groups <name> +g -g add to and remove from groups
  user disable <name>      block a user from logging in and end their sessions
  user enable <name>       let a disabled user log in again
  user sessions <name>     where a user is signed in
  user logout <name>       end every session and access token a user has
  user admin <name> on|off grant or revoke admin
  token create <user>      make an access token for scripts (--name N, --app A,
                           --expires 90d); the secret is printed once
  token list [user]        table of access tokens
  token revoke <id>        end an access token
  auth rotate-key          replace the key that signs identity tokens
  gateway status           the gateway's addresses and the apps it routes
  gateway activity [-n N]  sign-ins, failures and refusals at the gateway

Secrets (handed to services as environment variables; changing one restarts
the services that get it):
  secret set <name>        create a secret, or rotate its value, read hidden
                           (--env VAR, --service S, --global, --value-stdin)
  secret list              table of secrets and what gets them, never values
  secret bind <name> <service>|--global
                           hand a secret to a service, or to every service
  secret unbind <name> <service>|--global
                           take it away again
  secret rm <name>         delete a secret

Flags:
  --home <dir>             Outpost root (default $OUTPOST_HOME or ~/.outpost)
  --version                print the build identity of this binary
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
	home   string
	out    io.Writer
	err    io.Writer
	config config.Config
}

func run(args []string, stdout, stderr io.Writer) error {
	// Answered before the config is read: "which build is this?" has to work
	// on a machine where something else is broken, which is exactly when it
	// gets asked.
	for _, arg := range args {
		if arg == "--version" || arg == "-version" {
			fmt.Fprint(stdout, version.Get().Long())
			return nil
		}
	}

	// A node process is started by the daemon, not an operator, and needs
	// neither a home directory nor the config file.
	if len(args) > 0 && args[0] == outposttsnet.NodeCommand {
		return runTsnetNode(args[1:], stdout, stderr)
	}

	home, rest, err := parseGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errUsage
	}

	a := &app{home: home, out: stdout, err: stderr}

	// Global settings shape validation and whether the tailnet is used at all.
	// A missing file is normal; a malformed one is worth stopping for.
	cfg, err := config.Load(a.configPath())
	if err != nil {
		return err
	}
	a.config = cfg

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
	case "add":
		return a.cmdAdd(cmdArgs)
	case "update":
		return a.cmdUpdate(cmdArgs)
	case "remove", "rm":
		return a.cmdRemove(cmdArgs)
	case "tui":
		return a.cmdTUI(cmdArgs)
	case "config":
		return a.cmdConfig(cmdArgs)
	case "install":
		return a.cmdInstall(cmdArgs)
	case "uninstall":
		return a.cmdUninstall(cmdArgs)
	case "daemon":
		return a.cmdDaemon(cmdArgs)
	case "version":
		return a.cmdVersion(cmdArgs)
	case "user":
		return a.cmdUser(cmdArgs)
	case "token":
		return a.cmdToken(cmdArgs)
	case "secret", "secrets":
		return a.cmdSecret(cmdArgs)
	case "auth":
		return a.cmdAuth(cmdArgs)
	case "gateway":
		return a.cmdGateway(cmdArgs)
	case selfUpdateCommand:
		// Started by the gateway, not an operator, so it isn't in the usage.
		return a.cmdSelfUpdate(cmdArgs)

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
func (a *app) configPath() string  { return filepath.Join(a.home, "config.toml") }
func (a *app) authPath() string    { return filepath.Join(a.home, "auth.db") }

// secrets.db and the key its values are sealed with. Kept apart so a copy of
// the database alone doesn't give the values away; back them up together.
func (a *app) secretsPath() string    { return filepath.Join(a.home, "secrets.db") }
func (a *app) secretsKeyPath() string { return filepath.Join(a.home, "secrets.key") }

// ensureLayout creates the Outpost root on first use. A home created here is
// a new install, and gets the new-install Tailscale setting.
func (a *app) ensureLayout() error {
	fresh, err := a.createLayout()
	if err != nil || !fresh {
		return err
	}
	return a.firstRunTailscale(true)
}

// createLayout creates the Outpost root and reports whether the home itself
// is new, for the commands that settle a new home's Tailscale setting their
// own way.
func (a *app) createLayout() (fresh bool, err error) {
	_, statErr := os.Stat(a.home)
	fresh = os.IsNotExist(statErr)
	for _, dir := range []string{a.home, a.servicesDir(), a.logsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return fresh, nil
}
