package main

import (
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/size"

	"github.com/sroberts/outpost/internal/autostart"
)

// cmdBackup archives one service's storage, or every service's (§12).
func (a *app) cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(a.err)
	list := fs.Bool("list", false, "list existing backups instead of making one")
	schedule := fs.String("schedule", "", "install a recurring backup: hourly, daily or weekly")
	at := fs.String("at", "03:00", "time of day for a daily or weekly schedule (HH:MM)")
	system := fs.Bool("system", false, "install the schedule as a boot-time job (needs root)")
	enable := fs.Bool("enable", false, "run the commands that activate the schedule")
	unschedule := fs.Bool("unschedule", false, "remove a previously installed schedule")
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

	// Scheduling writes an init-system job; it does not need a running daemon.
	if *schedule != "" || *unschedule {
		return a.manageSchedule(name, *schedule, *at, *system, *enable, *unschedule)
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

// manageSchedule installs or removes a recurring backup, using launchd or
// systemd rather than a scheduler inside Outpost (§10).
func (a *app) manageSchedule(service, cadence, at string, system, enable, remove bool) error {
	sched, err := autostart.SchedulerFor(runtime.GOOS)
	if err != nil {
		return err
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the outpost binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	home, err := filepath.Abs(a.home)
	if err != nil {
		return err
	}

	job := autostart.BackupSchedule{
		Config:  autostart.Config{Label: autostart.DefaultLabel, Binary: binary, Home: home},
		Service: service,
	}
	if system {
		job.Scope = autostart.System
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("determine the user to run as: %w", err)
		}
		job.User = u.Username
	}

	unitPath, err := sched.BackupUnitPath(job)
	if err != nil {
		return err
	}

	legacy, err := autostart.LegacyBackupUnits(runtime.GOOS, job)
	if err != nil {
		return err
	}

	if remove {
		if _, err := os.Stat(unitPath); os.IsNotExist(err) {
			if len(legacy) > 0 {
				return a.clearLegacyBackup(job, legacy, true)
			}
			return fmt.Errorf("no backup schedule installed at %s", unitPath)
		}
		if err := a.clearLegacyBackup(job, legacy, true); err != nil {
			return err
		}
		if err := a.runAll(autostart.BackupDisableCommands(runtime.GOOS, unitPath, job)); err != nil {
			fmt.Fprintf(a.err, "outpost: could not stop the schedule (expected if it was never enabled): %v\n", err)
		}
		// systemd writes a service beside the timer; remove both.
		for _, p := range []string{unitPath, strings.TrimSuffix(unitPath, ".timer") + ".service"} {
			if p == unitPath || strings.HasSuffix(unitPath, ".timer") {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove %s: %w", p, err)
				}
				fmt.Fprintf(a.out, "removed %s\n", p)
			}
		}
		return nil
	}

	parsed, err := autostart.ParseCadence(cadence)
	if err != nil {
		return err
	}
	job.Cadence = parsed
	if job.Hour, job.Minute, err = parseTimeOfDay(at); err != nil {
		return err
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	written, err := autostart.WriteBackupUnits(runtime.GOOS, job)
	if err != nil {
		return err
	}
	for _, p := range written {
		fmt.Fprintf(a.out, "wrote %s\n", p)
	}
	fmt.Fprintf(a.out, "  %s\n", job.Describe())
	fmt.Fprintln(a.out, "  Each run stops the service, archives its storage, and starts it again.")

	commands := autostart.BackupEnableCommands(runtime.GOOS, unitPath, job)
	if !enable {
		fmt.Fprintln(a.out, "\nTo activate it:")
		for _, cmd := range commands {
			fmt.Fprintf(a.out, "  %s\n", strings.Join(cmd, " "))
		}
		fmt.Fprintln(a.out, "\nOr re-run with --enable. To undo: outpost backup --unschedule")
		return a.clearLegacyBackup(job, legacy, false)
	}
	if err := a.runAll(commands); err != nil {
		return err
	}
	// Only once the new timer is running: clearing the old one first would
	// leave no schedule at all if writing or enabling the new one failed.
	return a.clearLegacyBackup(job, legacy, true)
}

// clearLegacyBackup deals with the backup.service and backup.timer an older
// Outpost wrote on systemd (#20), so an upgrade does not leave a second timer
// running the same backup. With stop it disables the old timer and removes
// both files. Without it -- the operator asked us not to touch systemctl -- or
// when disabling fails, it leaves them and prints the commands, since a timer
// whose file is gone can stay enabled behind a dangling symlink.
func (a *app) clearLegacyBackup(job autostart.BackupSchedule, legacy []string, stop bool) error {
	if len(legacy) == 0 {
		return nil
	}
	// Only a timer can be enabled; with just the service left there is
	// nothing to disable, and asking systemctl would fail on the missing unit.
	var commands [][]string
	for _, p := range legacy {
		if strings.HasSuffix(p, ".timer") {
			commands = autostart.LegacyBackupDisableCommands(job)
		}
	}
	if stop {
		err := a.runAll(commands)
		if err == nil {
			for _, p := range legacy {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove the old %s: %w", p, err)
				}
				fmt.Fprintf(a.out, "removed %s (the old, unnamespaced name)\n", p)
			}
			return nil
		}
		fmt.Fprintf(a.err, "outpost: could not stop the old backup timer, so its files are left in place: %v\n", err)
	}
	fmt.Fprintln(a.out, "An older Outpost scheduled backups under an unnamespaced name. Remove it with:")
	for _, cmd := range commands {
		fmt.Fprintf(a.out, "  %s\n", strings.Join(cmd, " "))
	}
	rm := []string{"rm"}
	if job.Scope == autostart.System {
		rm = []string{"sudo", "rm"}
	}
	fmt.Fprintf(a.out, "  %s\n", strings.Join(append(rm, legacy...), " "))
	return nil
}

// parseTimeOfDay reads an HH:MM value.
func parseTimeOfDay(s string) (hour, minute int, err error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("--at must be HH:MM; got %q", s)
	}
	if hour, err = strconv.Atoi(h); err != nil {
		return 0, 0, fmt.Errorf("--at hour %q is not a number", h)
	}
	if minute, err = strconv.Atoi(m); err != nil {
		return 0, 0, fmt.Errorf("--at minute %q is not a number", m)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("--at %q is not a valid time of day", s)
	}
	return hour, minute, nil
}

// humanSize renders a byte count in the units the portal uses.
func humanSize(n int64) string {
	if n < 0 {
		n = 0
	}
	return size.Bytes(uint64(n))
}
