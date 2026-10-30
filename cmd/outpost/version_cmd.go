package main

import (
	"flag"
	"fmt"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/version"
)

// cmdVersion reports this binary's build, and the running daemon's when there
// is one (#12). The pair matters: upgrading the binary on disk does not
// restart the daemon, so the two can silently diverge for months.
func (a *app) cmdVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(a.err)
	short := fs.Bool("short", false, "print just the version string")
	if err := fs.Parse(args); err != nil {
		return err
	}

	info := version.Get()
	if *short {
		fmt.Fprintln(a.out, info.Version)
		return nil
	}
	fmt.Fprint(a.out, info.Long())

	client := ipc.NewClient(a.socketPath())
	if !client.Available() {
		fmt.Fprintln(a.out, "\n  daemon:   not running")
		return nil
	}
	daemon, err := client.DaemonVersion()
	if err != nil {
		fmt.Fprintf(a.out, "\n  daemon:   running, but did not report a version (%v)\n", err)
		return nil
	}

	fmt.Fprintf(a.out, "\n  daemon:   %s\n", daemon)
	if daemon != info.String() {
		fmt.Fprintf(a.out, "\n  The daemon is running a different build from this CLI.\n"+
			"  Upgrading the binary does not restart it; `outpost daemon restart`,\n"+
			"  or restart it however you started it, to pick this one up.\n")
	}
	return nil
}
