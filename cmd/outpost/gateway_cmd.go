package main

import (
	"errors"
	"fmt"
	"text/tabwriter"
)

// cmdGateway handles `outpost gateway status`.
func (a *app) cmdGateway(args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: outpost gateway status")
	}
	c, err := a.daemon()
	if err != nil {
		return err
	}
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
