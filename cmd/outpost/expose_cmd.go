package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/supervisor"
)

const exposeUsage = `usage:
  outpost expose <service>                      what the internet and the tailnet reach without a login
  outpost expose <service> --path P... [--funnel]
                                                set the paths that skip the login, and whether Funnel
                                                puts them on the internet (no --path: none)
  outpost expose <service> --reset              go back to the manifest's
  outpost expose <service> --mcp on|off|reset   whether the internet reaches its MCP endpoint`

// cmdExpose sets what of a service skips the login and what the internet
// reaches, as each app's Manage menu in the portal does. The daemon holds a
// setting to the manifest's rules and applies it before answering.
func (a *app) cmdExpose(args []string) error {
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	fs.SetOutput(a.err)
	var paths repeated
	fs.Var(&paths, "path", "a path that skips the login, such as /public/* (repeatable)")
	funnel := fs.Bool("funnel", false, "put the public paths on the internet with Funnel")
	reset := fs.Bool("reset", false, "go back to the manifest's public paths and Funnel")
	mcp := fs.String("mcp", "", "on, off or reset: whether the internet reaches the MCP endpoint")
	service, err := parseServiceCommand(fs, "expose", args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(a.out, exposeUsage)
			return nil
		}
		return err
	}
	setting := len(paths) > 0 || *funnel
	switch {
	case *mcp != "" && (setting || *reset):
		return errors.New("expose: --mcp is set on its own")
	case *reset && setting:
		return errors.New("expose: --reset takes no --path or --funnel")
	}
	switch *mcp {
	case "":
	case "on", "off":
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.ExposureMCP(service, *mcp == "on") })
	case "reset":
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.ExposureMCPReset(service) })
	default:
		return fmt.Errorf("expose: --mcp is on, off or reset, not %q", *mcp)
	}
	if *reset {
		return a.secretDo(func(c *ipc.Client) (string, error) { return c.ExposureReset(service) })
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	if !setting {
		st, err := c.Status(service)
		if err != nil {
			return err
		}
		fmt.Fprint(a.out, describeReach(st))
		return nil
	}
	msg, err := c.ExposureSet(service, *funnel, paths)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// describeReach says what of s skips the login, and where it reaches.
func describeReach(s supervisor.ServiceStatus) string {
	var b strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&b, "%-10s%s\n", k, v) }
	kv("public", orDash(strings.Join(s.PublicPaths, " ")))
	funnel := "off: only the tailnet reaches the public paths"
	if s.Funnel {
		funnel = "on"
		if t := s.Tailnet; t != nil && t.Funnel != "" {
			funnel += " (" + t.Funnel
			if t.FunnelError != "" {
				funnel += ": " + t.FunnelError
			}
			funnel += ")"
		}
	}
	kv("funnel", funnel)
	if s.FromPortal {
		was := "off"
		if s.ManifestFunnel {
			was = "on"
		}
		kv("", fmt.Sprintf("set over the manifest, which has Funnel %s and public paths %s",
			was, orDash(strings.Join(s.ManifestPublicPaths, " "))))
	}
	if s.MCPPath != "" {
		mcp := s.MCPPath + " on the tailnet only"
		if s.MCPFunnel {
			mcp = s.MCPPath + " on the internet, for internet access tokens"
		}
		if s.MCPFromPortal {
			mcp += "; set over the manifest"
		}
		kv("mcp", mcp)
	}
	return b.String()
}
