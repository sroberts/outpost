package main

import (
	"flag"
	"fmt"
	"text/tabwriter"
	"time"
)

// cmdBackup archives one service's storage, or every service's (§12).
func (a *app) cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(a.err)
	list := fs.Bool("list", false, "list existing backups instead of making one")
	positional, err := parseOptionalService(fs, "backup", args)
	if err != nil {
		return err
	}
	// The service name is optional here: no name means every service.
	var name string
	if len(positional) > 0 {
		name = positional[0]
	}
	if len(positional) > 1 {
		return fmt.Errorf("backup: unexpected argument %q", positional[1])
	}

	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}

	if *list {
		if name == "" {
			return fmt.Errorf("backup --list: a service name is required")
		}
		archives, err := c.Backups(name)
		if err != nil {
			return err
		}
		if len(archives) == 0 {
			fmt.Fprintf(a.out, "no backups for %s\n", name)
			return nil
		}
		tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TIMESTAMP\tAGE\tSIZE\tPATH")
		for _, ar := range archives {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				ar.Timestamp, truncateDur(time.Since(ar.CreatedAt)), humanSize(ar.Size), ar.Path)
		}
		return tw.Flush()
	}

	if name == "" {
		fmt.Fprintln(a.out, "backing up every service (each is stopped and restarted in turn)")
	}
	archives, msg, err := c.Backup(name)
	if err != nil {
		return err
	}
	for _, ar := range archives {
		fmt.Fprintf(a.out, "  %-20s %s (%s)\n", ar.Service, ar.Timestamp, humanSize(ar.Size))
	}
	if msg != "" {
		fmt.Fprintln(a.out, msg)
	}
	return nil
}

// cmdRestore replaces a service's storage from an archive.
func (a *app) cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(a.err)
	rest, err := parseOptionalService(fs, "restore", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("restore: a service name is required (`outpost restore <service> [timestamp]`)")
	}
	name := rest[0]
	timestamp := ""
	if len(rest) > 1 {
		timestamp = rest[1]
	}
	if len(rest) > 2 {
		return fmt.Errorf("restore: unexpected argument %q", rest[2])
	}

	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}

	// Restoring discards whatever the service has written since the backup, so
	// name what is about to happen before it happens.
	target := timestamp
	if target == "" {
		target = "the most recent backup"
	}
	fmt.Fprintf(a.out, "restoring %s from %s; current contents of its storage directory will be replaced\n", name, target)

	msg, err := c.Restore(name, timestamp)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// humanSize renders a byte count compactly.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
