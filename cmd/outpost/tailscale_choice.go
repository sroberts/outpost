package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/tsdetect"
)

// Seams for tests: detection runs the real tailscale CLI, and the install
// question needs a terminal.
var (
	detectTailscale            = tsdetect.Detect
	answerIn         io.Reader = os.Stdin
	answerIsTerminal           = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

// firstRunTailscale settles the Tailscale setting for a home this command has
// just created. New installs start with Tailscale off; the built-in default
// stays on only so that existing homes with no setting keep the behaviour
// they've been running with. With detect false the hint is given without
// looking for Tailscale first, which can take seconds.
func (a *app) firstRunTailscale(detect bool) error {
	if err := a.saveTailscale(false); err != nil {
		return err
	}
	fmt.Fprintf(a.err, "outpost: new Outpost home at %s; Tailscale is off, so services stay on loopback\n", a.home)
	const turnOn = "to give each service its own Tailscale identity, run `outpost config --tailscale on`"
	if !detect {
		fmt.Fprintf(a.err, "outpost: %s\n", turnOn)
	} else if res := detectTailscale(context.Background()); res.Found {
		fmt.Fprintf(a.err, "outpost: %s; %s\n", runningHere(res), turnOn)
	}
	return nil
}

// chooseTailscale is install's part in the decision. An explicit flag always
// wins. Otherwise a home that already has a setting keeps it, and one without
// is asked, once, when there's a terminal to ask on.
func (a *app) chooseTailscale(on, off, fresh, explicit bool) error {
	switch {
	case on || off:
		return a.setTailscale(on)
	case explicit:
		fmt.Fprintf(a.out, "Tailscale: %s (from config.toml; change it with `outpost config --tailscale on|off`)\n",
			onOff(a.config.TailscaleEnabled()))
		return nil
	case answerIsTerminal():
		yes, err := a.askTailscale()
		if err != nil {
			return err
		}
		return a.setTailscale(yes)
	case fresh:
		return a.setTailscale(false)
	default:
		// An existing home with no setting has been running with Tailscale on.
		// Turning it off unasked would take every service off the tailnet.
		fmt.Fprintln(a.out, "Tailscale: on (this home has no setting, and on is what it has been running with)")
		fmt.Fprintln(a.out, "  record a choice with --tailscale or --no-tailscale, or `outpost config --tailscale on|off`")
		return nil
	}
}

// askTailscale reports what detection found and asks. The default is yes
// when this home already has Tailscale state, since its services are on a
// tailnet now, and no otherwise.
func (a *app) askTailscale() (bool, error) {
	res := detectTailscale(context.Background())
	if res.Found {
		fmt.Fprintf(a.out, "%s.\n", capitalise(runningHere(res)))
	} else {
		fmt.Fprintln(a.out, "Tailscale doesn't appear to be running here. Outpost doesn't need it to put services on a tailnet, but you'll need a tailnet account.")
	}

	def := false
	if fi, err := os.Stat(a.tsnetDir()); err == nil && fi.IsDir() {
		def = true
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}

	in := bufio.NewReader(answerIn)
	for range 3 {
		fmt.Fprintf(a.out, "Give each service its own Tailscale identity? %s ", hint)
		line, err := in.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		switch {
		case answer == "" && (err == nil || errors.Is(err, io.EOF)):
			if err != nil {
				fmt.Fprintln(a.out)
			}
			return def, nil
		case answer == "y" || answer == "yes":
			return true, nil
		case answer == "n" || answer == "no":
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read answer: %w", err)
		}
		fmt.Fprintln(a.out, "Please answer y or n.")
	}
	return false, errors.New("no answer to the Tailscale question; re-run with --tailscale or --no-tailscale")
}

// setTailscale records an explicit choice and says what it means.
func (a *app) setTailscale(on bool) error {
	if err := a.saveTailscale(on); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "Tailscale: %s (written to %s)\n", onOff(on), a.configPath())
	if on {
		a.warnMissingTailscaleNames()
	}
	// The daemon's setting can't be asked for, and config.toml's previous
	// value needn't be it: the file may have changed since the daemon started.
	if a.client().Available() {
		fmt.Fprintln(a.out, "  a daemon is running and reads config.toml only at startup; restart it if this changed its setting")
	}
	return nil
}

// saveTailscale writes the setting, keeping everything else in the file.
func (a *app) saveTailscale(on bool) error {
	cfg := a.config
	cfg.Tailscale.Enabled = &on
	if err := config.Save(a.configPath(), cfg); err != nil {
		return err
	}
	a.config = cfg
	return nil
}

// warnMissingTailscaleNames points out manifests that turning Tailscale on
// makes invalid, before a daemon restart finds out the hard way.
func (a *app) warnMissingTailscaleNames() {
	services, err := manifest.LoadDir(a.servicesDir())
	if err != nil || len(services) == 0 {
		return
	}
	errs := manifest.ValidateWith(services, manifest.Options{RequireTailscale: true})
	if len(errs) == 0 {
		return
	}
	fmt.Fprintln(a.out, "  warning: with Tailscale on, these manifests don't validate; fix them before restarting the daemon:")
	for _, e := range errs {
		fmt.Fprintf(a.out, "    - %v\n", e)
	}
}

func runningHere(res tsdetect.Result) string {
	if res.Tailnet != "" {
		return "Tailscale is running here, tailnet " + res.Tailnet
	}
	return "Tailscale is running here"
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
