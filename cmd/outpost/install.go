package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/config"
)

// cmdInstall writes the init-system integration that keeps the daemon running:
// a launchd job on macOS, a systemd unit on Linux (§15 step 5).
func (a *app) cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.err)
	system := fs.Bool("system", false, "install a boot-time service instead of a per-user one (needs root)")
	enable := fs.Bool("enable", false, "run the commands that start and enable the service")
	printOnly := fs.Bool("print", false, "write nothing; print the generated file to stdout")
	label := fs.String("label", autostart.DefaultLabel, "job label / unit name")
	authKey := fs.String("authkey", "", "TS_AUTHKEY to put in the service environment")
	tsOn := fs.Bool("tailscale", false, "give each service its own Tailscale identity, without asking")
	tsOff := fs.Bool("no-tailscale", false, "keep services on loopback only, without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tsOn && *tsOff {
		return fmt.Errorf("install: --tailscale and --no-tailscale can't be used together")
	}

	mgr, cfg, err := a.autostartConfig(*label, *system, *authKey)
	if err != nil {
		return err
	}
	body, err := mgr.Render(cfg)
	if err != nil {
		return err
	}

	if *printOnly {
		fmt.Fprint(a.out, body)
		return nil
	}

	// Decided before ensureLayout creates anything, which would otherwise
	// make every home look like an existing one.
	_, statErr := os.Stat(a.home)
	fresh := os.IsNotExist(statErr)
	explicit := false
	if !fresh {
		if explicit, err = config.HasTailscaleSetting(a.configPath()); err != nil {
			return err
		}
	}
	a.deferTailscaleChoice = true
	if err := a.ensureLayout(); err != nil {
		return err
	}
	if err := a.chooseTailscale(*tsOn, *tsOff, fresh, explicit); err != nil {
		return err
	}

	unitPath, err := mgr.UnitPath(cfg)
	if err != nil {
		return err
	}
	if err := writeUnit(unitPath, body, mgr.FileMode()); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "wrote %s (%s, %s scope)\n", unitPath, mgr.Name(), cfg.Scope)

	for _, note := range mgr.Notes(cfg) {
		fmt.Fprintf(a.out, "  %s\n", note)
	}

	commands := mgr.EnableCommands(unitPath, cfg)
	if !*enable {
		// Registering a background service is the user's call, not a side
		// effect of generating a file.
		fmt.Fprintln(a.out, "\nTo start it now and keep it running:")
		for _, cmd := range commands {
			fmt.Fprintf(a.out, "  %s\n", strings.Join(cmd, " "))
		}
		fmt.Fprintf(a.out, "\nOr re-run with --enable to have Outpost run those for you.\n")
		fmt.Fprintf(a.out, "To undo: outpost uninstall%s\n", scopeFlag(cfg))
		return nil
	}
	return a.runAll(commands)
}

// cmdUninstall stops the service and removes its unit file.
func (a *app) cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(a.err)
	system := fs.Bool("system", false, "remove the boot-time service instead of the per-user one")
	keepFile := fs.Bool("keep-file", false, "stop the service but leave the unit file in place")
	label := fs.String("label", autostart.DefaultLabel, "job label / unit name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mgr, cfg, err := a.autostartConfig(*label, *system, "")
	if err != nil {
		return err
	}
	unitPath, err := mgr.UnitPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("nothing installed at %s", unitPath)
	}

	if err := a.runAll(mgr.DisableCommands(unitPath, cfg)); err != nil {
		// A service that was never started cannot be stopped, and that is the
		// normal case when undoing an install that was never enabled. Say so,
		// and carry on to remove the file either way.
		fmt.Fprintf(a.err, "outpost: could not stop the service (this is expected if it was never enabled): %v\n", err)
	}
	if *keepFile {
		fmt.Fprintf(a.out, "left %s in place\n", unitPath)
		return nil
	}
	if err := os.Remove(unitPath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("remove %s: %w (try sudo for a system-scope install)", unitPath, err)
		}
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}
	fmt.Fprintf(a.out, "removed %s\n", unitPath)
	return nil
}

// autostartConfig resolves the platform manager and the job description.
func (a *app) autostartConfig(label string, system bool, authKey string) (autostart.Manager, autostart.Config, error) {
	mgr, err := autostart.ForHost()
	if err != nil {
		return nil, autostart.Config{}, err
	}

	binary, err := os.Executable()
	if err != nil {
		return nil, autostart.Config{}, fmt.Errorf("locate the outpost binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	home, err := filepath.Abs(a.home)
	if err != nil {
		return nil, autostart.Config{}, err
	}

	cfg := autostart.Config{Label: label, Binary: binary, Home: home}
	if system {
		cfg.Scope = autostart.System
		u, err := user.Current()
		if err != nil {
			return nil, autostart.Config{}, fmt.Errorf("determine the user to run as: %w", err)
		}
		cfg.User = u.Username
	}
	// An auth key in the unit file keeps it out of the manifests, which are
	// world-readable to anyone who can read the Outpost root.
	if authKey == "" {
		authKey = os.Getenv("TS_AUTHKEY")
	}
	if authKey != "" {
		cfg.Env = map[string]string{"TS_AUTHKEY": authKey}
	}
	return mgr, cfg, nil
}

// runAll executes commands in order, reporting what it runs.
func (a *app) runAll(commands [][]string) error {
	for _, argv := range commands {
		fmt.Fprintf(a.out, "  $ %s\n", strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr, cmd.Stdin = a.out, a.err, os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// writeUnit writes the generated file, with a clearer error when the location
// needs privileges.
func writeUnit(path, body string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("create %s: %w (a system-scope install needs sudo)", filepath.Dir(path), err)
		}
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("write %s: %w (a system-scope install needs sudo)", path, err)
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func scopeFlag(cfg autostart.Config) string {
	if cfg.Scope == autostart.System {
		return " --system"
	}
	return ""
}
