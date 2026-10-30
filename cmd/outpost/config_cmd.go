package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/ipc"
)

// cmdConfig shows the global configuration, or writes a starter file (§9).
func (a *app) cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(a.err)
	initialise := fs.Bool("init", false, "write a starter config.toml if none exists")
	path := fs.Bool("path", false, "print the config file's path and nothing else")
	tailscale := fs.String("tailscale", "", "on or off: whether each service gets its own Tailscale identity")
	authKey := fs.Bool("authkey", false, "save a Tailscale auth key, read from the terminal or one line of stdin")
	noAuthKey := fs.Bool("no-authkey", false, "remove the Tailscale auth key from config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *authKey || *noAuthKey {
		if *authKey && *noAuthKey || *tailscale != "" || *initialise || *path {
			return fmt.Errorf("config: --authkey and --no-authkey each go on their own")
		}
		if *noAuthKey {
			return a.saveAuthKey("")
		}
		key, err := a.readAuthKey()
		if err != nil {
			return err
		}
		return a.saveAuthKey(key)
	}

	if *tailscale != "" {
		if *initialise || *path {
			return fmt.Errorf("config: --tailscale can't be combined with --init or --path")
		}
		switch strings.ToLower(*tailscale) {
		case "on":
			return a.setTailscale(true)
		case "off":
			return a.setTailscale(false)
		default:
			return fmt.Errorf("config: --tailscale takes on or off, not %q", *tailscale)
		}
	}

	if *path {
		fmt.Fprintln(a.out, a.configPath())
		return nil
	}

	if *initialise {
		if _, err := os.Stat(a.configPath()); err == nil {
			return fmt.Errorf("%s already exists", a.configPath())
		}
		cfg := a.config
		// A home that doesn't exist yet is a new install, which starts with
		// Tailscale off. An existing home with no file has been running with
		// it on, and the starter file records that.
		if _, err := os.Stat(a.home); os.IsNotExist(err) {
			off := false
			cfg.Tailscale.Enabled = &off
		}
		if err := config.Save(a.configPath(), cfg); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "wrote %s\n", a.configPath())
		return nil
	}

	if _, err := os.Stat(a.configPath()); os.IsNotExist(err) {
		fmt.Fprintf(a.out, "# no config file at %s; these are the defaults\n", a.configPath())
		fmt.Fprintf(a.out, "# write them with `outpost config --init`\n\n")
	} else {
		fmt.Fprintf(a.out, "# %s\n\n", a.configPath())
	}
	shown := a.config
	if shown.Tailscale.AuthKey != "" {
		// Shown as set, not shown: this output gets pasted into issues.
		shown.Tailscale.AuthKey = "(set; hidden)"
	}
	fmt.Fprint(a.out, shown.Render())
	// No daemon is not a change: configChanged treats the failed ping as no.
	if a.configChanged(a.client()) {
		fmt.Fprintf(a.out, "\n# %s\n", restartToApply)
	}
	return nil
}

// readAuthKey gets an auth key from the terminal, without echoing it, or
// from one line of stdin. There's deliberately no flag value for it: a key
// on the command line ends up in shell history and the process table.
func (a *app) readAuthKey() (string, error) {
	var key string
	if stdinIsTerminal() {
		fmt.Fprint(a.err, "Tailscale auth key: ")
		k, err := readSecret()
		fmt.Fprintln(a.err)
		if err != nil {
			return "", fmt.Errorf("read auth key: %w", err)
		}
		key = k
	} else {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read auth key: %w", err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return "", errors.New("config: no auth key given; nothing was changed (--no-authkey removes one)")
	case !strings.HasPrefix(key, "tskey-"):
		// The commonest slip is pasting the wrong thing, and the daemon would
		// otherwise only say so after a restart, in its log.
		return "", errors.New("config: that isn't a Tailscale auth key, which starts with tskey-; nothing was changed")
	}
	return key, nil
}

// saveAuthKey writes the auth key to config.toml, or removes it when key is
// "", keeping everything else in the file.
func (a *app) saveAuthKey(key string) error {
	cfg := a.config
	cfg.Tailscale.AuthKey = key
	if err := config.Save(a.configPath(), cfg); err != nil {
		return err
	}
	a.config = cfg
	if key == "" {
		fmt.Fprintf(a.out, "Tailscale auth key: removed from %s\n", a.configPath())
	} else {
		fmt.Fprintf(a.out, "Tailscale auth key: saved to %s, readable only by you\n", a.configPath())
		fmt.Fprintln(a.out, "  nodes waiting for a login use it once the daemon restarts")
		if !cfg.TailscaleEnabled() {
			fmt.Fprintln(a.out, "  Tailscale is off, so nothing uses it yet; turn it on with `outpost config --tailscale on`")
		}
	}
	if os.Getenv("TS_AUTHKEY") != "" {
		fmt.Fprintln(a.out, "  note: TS_AUTHKEY is set in this environment; where it is set for the daemon too, it wins over config.toml")
	}
	a.hintRestart()
	return nil
}

// restartToApply is what the CLI says when the daemon is running settings
// other than config.toml's. The daemon reads the file only at startup.
const restartToApply = "config.toml has changed since the daemon started; run `outpost daemon restart` to apply it"

// configChanged reports whether the running daemon started with settings
// other than the ones in config.toml now. A daemon too old to say is given
// the benefit of the doubt.
func (a *app) configChanged(c *ipc.Client) bool {
	running, err := c.DaemonConfig()
	return err == nil && running != "" && running != a.config.Fingerprint()
}

// warnConfigChanged puts restartToApply on stderr, where it can't get in the
// way of output a script reads.
func (a *app) warnConfigChanged(c *ipc.Client) {
	if a.configChanged(c) {
		fmt.Fprintf(a.err, "warning: %s\n", restartToApply)
	}
}
