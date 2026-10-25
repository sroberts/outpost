package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sroberts/outpost/internal/config"
)

// cmdConfig shows the global configuration, or writes a starter file (§9).
func (a *app) cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(a.err)
	initialise := fs.Bool("init", false, "write a starter config.toml if none exists")
	path := fs.Bool("path", false, "print the config file's path and nothing else")
	tailscale := fs.String("tailscale", "", "on or off: whether each service gets its own Tailscale identity")
	if err := fs.Parse(args); err != nil {
		return err
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
		if err := config.Save(a.configPath(), a.config); err != nil {
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
	fmt.Fprint(a.out, a.config.Render())
	return nil
}
