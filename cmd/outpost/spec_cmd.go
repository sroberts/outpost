package main

import (
	"flag"
	"fmt"

	"github.com/sroberts/outpost"
)

// cmdSpec prints the service contract this binary carries, for building an
// app on a machine where the gateway is off or out of reach. With the gateway
// on, the portal serves the running daemon's copy, which is the one to trust
// when the binary on disk has been upgraded and the daemon not yet restarted.
func (a *app) cmdSpec(args []string) error {
	fs := flag.NewFlagSet("spec", flag.ContinueOnError)
	fs.SetOutput(a.err)
	revision := fs.Bool("revision", false, "print just the contract's revision")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errUsage
	}
	if *revision {
		fmt.Fprintln(a.out, outpost.ContractRevision())
		return nil
	}
	fmt.Fprint(a.out, outpost.ServiceSpec())
	return nil
}
