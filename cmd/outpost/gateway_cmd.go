package main

import (
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/sroberts/outpost/internal/gateway"
)

// cmdGateway handles `outpost gateway status` and `outpost gateway activity`.
func (a *app) cmdGateway(args []string) error {
	if len(args) > 0 && args[0] == "activity" {
		return a.gatewayActivity(args[1:])
	}
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: outpost gateway status | outpost gateway activity [-n N]")
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	// Before the status, which comes from the daemon's settings: with the
	// gateway turned on since it started, there is no status to show.
	a.warnConfigChanged(c)
	st, err := c.GatewayStatus()
	if err != nil {
		return err
	}
	tls := "terminated in front"
	if st.TLS {
		tls = "served directly"
	}
	fmt.Fprintf(a.out, "Portal:  %s\n", st.PublicURL)
	fmt.Fprintf(a.out, "Listen:  %s (TLS %s)\n", st.Listen, tls)
	if n := st.Node; n != nil {
		a.printPortalNode(*n, st.PublicURL)
	}
	if st.Tailnet {
		fmt.Fprintf(a.out, "Apps:    their own tailnet address, else <name>.%s\n", st.Domain)
	} else {
		fmt.Fprintf(a.out, "Apps:    <name>.%s\n", st.Domain)
	}
	if len(st.Routes) == 0 {
		fmt.Fprintln(a.out, "\nNo services routed yet.")
		return nil
	}
	fmt.Fprintln(a.out)
	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tURL\tPORT\tSTATUS")
	for _, r := range st.Routes {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", r.Name, r.URL, r.Port, r.Status)
	}
	return tw.Flush()
}

// printPortalNode reports the portal's own tailnet node.
func (a *app) printPortalNode(n gateway.PortalNode, publicURL string) {
	switch n.State {
	case "on":
		fmt.Fprintf(a.out, "Tailnet: on, as %s\n", n.Host)
	case "needs-login":
		fmt.Fprintln(a.out, "Tailnet: needs a login")
	default:
		fmt.Fprintf(a.out, "Tailnet: %s\n", n.State)
	}
	if n.LoginURL != "" {
		fmt.Fprintf(a.out, "Login:   %s\n", n.LoginURL)
		fmt.Fprintln(a.out, "         open it to add the portal's node to your tailnet, or set an auth key with `outpost config --authkey`")
	}
	if n.Error != "" {
		fmt.Fprintf(a.out, "Error:   %s\n", n.Error)
	}
	if n.Renamed {
		fmt.Fprintf(a.out, "Warning: Tailscale named the portal's node %s, so nothing answers at %s.\n", n.Host, publicURL)
		fmt.Fprintln(a.out, "         Free the name in the Tailscale admin console, or point public_url and tailscale_name")
		fmt.Fprintln(a.out, "         under [gateway] in config.toml at the new name; then run `outpost daemon restart`.")
	}
}

// gatewayActivity shows what the gateway has recorded: sign-ins, failures,
// refusals and account changes, oldest first like `logs --events`.
func (a *app) gatewayActivity(args []string) error {
	fs := flag.NewFlagSet("gateway activity", flag.ContinueOnError)
	fs.SetOutput(a.err)
	limit := fs.Int("n", 20, "number of events to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("gateway activity: unexpected argument %q", fs.Arg(0))
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
	events, err := c.Events(gateway.ActivityService, *limit)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintln(a.out, "no gateway activity recorded")
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		fmt.Fprintf(a.out, "%s  %-13s %s\n", e.At.Format("2006-01-02 15:04:05"), e.Kind, e.Message)
	}
	return nil
}
