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
