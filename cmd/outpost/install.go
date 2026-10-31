package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/autostart"
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
	authKey := fs.String("authkey", "", "Tailscale auth key to save to config.toml; visible in the process table and\n"+
		"shell history, so prefer outpost config --authkey, which reads it from the terminal")
	tsOn := fs.Bool("tailscale", false, "give each service its own Tailscale identity, without asking")
	tsOff := fs.Bool("no-tailscale", false, "keep services on loopback only, without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tsOn && *tsOff {
		return fmt.Errorf("install: --tailscale and --no-tailscale can't be used together")
	}

	mgr, cfg, err := a.autostartConfig(*label, *system)
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

	explicit := a.config.TailscaleSet()
	fresh, err := a.createLayout()
	if err != nil {
		return err
	}
	// A new home is off before the question is asked, so abandoning it
	// (Ctrl-C, no usable answer) leaves a new install rather than one that
	// looks like an old home running with Tailscale on.
	if fresh {
		if err := a.saveTailscale(false); err != nil {
			return err
		}
	}
	on := *tsOn
	key := *authKey
	// An auth key is only any use with Tailscale on, so giving one answers
	// the question, unless the home already has a setting.
	if key != "" && !on && !*tsOff && !explicit {
		fmt.Fprintln(a.out, "An auth key was given, so Tailscale goes on.")
		on = true
	}
	if err := a.chooseTailscale(on, *tsOff, fresh, explicit); err != nil {
		return err
	}
	if key != "" {
		// config.toml rather than the unit file: the unit is world-readable,
		// and systemd shows its environment to any local user.
		if err := a.storeAuthKey(key); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "Tailscale auth key: saved to %s, readable only by you\n", a.configPath())
		if !a.config.TailscaleEnabled() {
			fmt.Fprintln(a.out, "  note: with Tailscale off, the auth key won't be used")
		}
	} else if os.Getenv("TS_AUTHKEY") != "" {
		// The installed daemon doesn't inherit this shell's environment, and
		// copying the key into the unit would publish it.
		fmt.Fprintln(a.out, "  note: TS_AUTHKEY in this shell isn't passed to the service; save it with `outpost config --authkey`")
	}

	unitPath, err := mgr.UnitPath(cfg)
	if err != nil {
		return err
	}
	// Read before it is overwritten: a file that was already there is what
	// makes it worth asking the init system whether the job is loaded.
	previous, readErr := os.ReadFile(unitPath)
	mode := mgr.FileMode()
	if len(cfg.Env) > 0 {
		// An environment is where secrets go, so it isn't for other users.
		mode = 0o600
	}
	if err := writeUnit(unitPath, body, mode); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "wrote %s (%s, %s scope)\n", unitPath, mgr.Name(), cfg.Scope)

	for _, note := range mgr.Notes(cfg) {
		fmt.Fprintf(a.out, "  %s\n", note)
	}

	loaded := readErr == nil && initProbe(mgr.LoadedCommand(cfg, effectiveUID()))
	changed := string(previous) != body

	commands := mgr.EnableCommands(unitPath, cfg)
	if loaded && !*enable {
		a.sayAlreadyLoaded(mgr, cfg, unitPath, changed)
		fmt.Fprintf(a.out, "To undo: outpost uninstall%s\n", scopeFlag(cfg))
		return nil
	}
	if loaded && !mgr.EnableIsIdempotent() {
		a.sayAlreadyLoaded(mgr, cfg, unitPath, changed)
		return nil
	}
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
	if err := a.runAll(commands); err != nil {
		return err
	}
	if loaded {
		a.sayAlreadyLoaded(mgr, cfg, unitPath, changed)
	}
	return nil
}

// sayAlreadyLoaded replaces the enable instructions for a job that is already
// loaded: they would read as what's needed, and on launchd they do nothing.
func (a *app) sayAlreadyLoaded(mgr autostart.Manager, cfg autostart.Config, unitPath string, changed bool) {
	fmt.Fprintf(a.out, "\n%s is already loaded (%s).\n", cfg.Label, mgr.Name())
	if reload := mgr.ReloadCommands(unitPath, cfg, effectiveUID()); changed && len(reload) > 0 {
		fmt.Fprintf(a.out, "This changed its file, and %s keeps the definition it loaded until the job is\n"+
			"unloaded, so a restart alone won't apply it. These apply it, restarting the daemon:\n", mgr.Name())
		for _, cmd := range reload {
			fmt.Fprintf(a.out, "  %s\n", strings.Join(cmd, " "))
		}
		return
	}
	fmt.Fprintf(a.out, "To restart the daemon onto the binary on disk:\n  %s\n", a.restartHint(cfg))
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

	mgr, cfg, err := a.autostartConfig(*label, *system)
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
func (a *app) autostartConfig(label string, system bool) (autostart.Manager, autostart.Config, error) {
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
	return mgr, cfg, nil
}

// initCommand runs one init-system command where the operator can see it and
// answer sudo. A variable so that no test can reach a real launchd or systemd,
// which on a developer's machine is running their real daemon.
var initCommand = func(a *app, argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = a.out, a.err, os.Stdin
	return cmd.Run()
}

// initProbe runs a command that only asks a question of the init system.
var initProbe = func(argv []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run() == nil
}

// effectiveUID is who the init-system commands would run as; a variable so
// tests can be root without being root.
var effectiveUID = os.Geteuid

// runAll executes commands in order, reporting what it runs.
func (a *app) runAll(commands [][]string) error {
	for _, argv := range commands {
		fmt.Fprintf(a.out, "  $ %s\n", strings.Join(argv, " "))
		if err := initCommand(a, argv); err != nil {
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
	// WriteFile sets the mode only on a file it creates, and one written by
	// an older install may be more open than this one should be.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func scopeFlag(cfg autostart.Config) string {
	if cfg.Scope == autostart.System {
		return " --system"
	}
	return ""
}
