package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/supervisor"
	"github.com/sroberts/outpost/internal/tailnet"
)

// client returns an IPC client for this Outpost root.
func (a *app) client() *ipc.Client { return ipc.NewClient(a.socketPath()) }

// errNoDaemon explains how to get one running.
func errNoDaemon() error {
	return errors.New("no outpost daemon is running; start one with `outpost serve` (or `outpost install` to run it under launchd)")
}

// cmdApply reconciles running state to the manifests (§12).
//
// Manifest validation is a pure function of the files on disk, so it runs
// locally either way; everything about *running* state goes through the daemon.
func (a *app) cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(a.err)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	services, err := a.validateManifests()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%d service(s) valid:\n", len(services))
	for _, svc := range services {
		fmt.Fprintf(a.out, "  %-20s %-9s port %d\n", svc.Name, svc.Runtime, svc.Port)
	}

	c := a.client()
	if !c.Available() {
		fmt.Fprintln(a.out, "\nNo daemon is running: nothing is being supervised.")
		fmt.Fprintln(a.out, "Start one with `outpost serve`.")
		return nil
	}

	applied, err := c.Apply()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "\nreconciled %d service(s):\n", len(applied))
	return writeTable(a.out, applied)
}

// validateManifests loads and checks the manifest set, failing loudly rather
// than half-applying (§5).
func (a *app) validateManifests() ([]*manifest.Service, error) {
	services, err := manifest.LoadDir(a.servicesDir())
	if err != nil {
		return nil, err
	}
	opts := manifest.Options{RequireTailscale: a.config.TailscaleEnabled()}
	if errs := manifest.ValidateWith(services, opts); len(errs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d manifest error(s):", len(errs))
		for _, e := range errs {
			fmt.Fprintf(&b, "\n  - %v", e)
		}
		return nil, errors.New(b.String())
	}
	for _, svc := range services {
		if svc.Runtime == "container" {
			return nil, fmt.Errorf("%s: runtime = \"container\" is not implemented yet (spec §9)", svc.Name)
		}
		if err := svc.EnsureStorage(); err != nil {
			return nil, err
		}
	}
	return services, nil
}

// cmdList prints the status table.
func (a *app) cmdList(args []string) error {
	c := a.client()
	if !c.Available() {
		// No daemon means no running state to report. Show what is declared so
		// the command is still useful, clearly marked as not-observed.
		services, err := manifest.LoadDir(a.servicesDir())
		if err != nil {
			return err
		}
		if len(services) == 0 {
			fmt.Fprintf(a.out, "No services declared in %s\n", a.servicesDir())
			return nil
		}
		fmt.Fprintln(a.err, "warning: no daemon running; showing declared services only")
		rows := make([]supervisor.ServiceStatus, 0, len(services))
		for _, svc := range services {
			rows = append(rows, supervisor.ServiceStatus{
				Name: svc.Name, Runtime: svc.Runtime, Status: "unknown",
				Port: svc.Port, TailscaleName: svc.TailscaleName,
			})
		}
		return writeTable(a.out, rows)
	}

	services, err := c.List()
	if err != nil {
		return err
	}
	if len(services) == 0 {
		fmt.Fprintf(a.out, "No services declared in %s\n", a.servicesDir())
		return nil
	}
	return writeTable(a.out, services)
}

func writeTable(w io.Writer, services []supervisor.ServiceStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tRUNTIME\tSTATUS\tUPTIME\tRESTARTS\tTAILSCALE")
	for _, s := range services {
		status := s.Status
		if s.Halted {
			status += " (halted)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			s.Name, orDash(s.Runtime), status, uptime(s), s.RestartCount, tailnet.Column(s.Tailnet, s.TailscaleName))
	}
	return tw.Flush()
}

// writeTailnetDetail writes the `outpost status` tailscale line, plus — only
// while the node awaits authorisation — the login URL and a reminder of the
// other way to unblock it. Every other state gains nothing here, because
// tailnet.Line already says what happened.
func writeTailnetDetail(w io.Writer, st supervisor.ServiceStatus) {
	fmt.Fprintf(w, "  tailscale:     %s\n", tailnet.Line(st.Tailnet, st.TailscaleName))
	if st.Tailnet.State == tailnet.StateNeedsLogin {
		fmt.Fprintf(w, "                 %s\n", st.Tailnet.LoginURL)
		fmt.Fprintln(w, "                 authorise the node at that URL, or set an auth key")
	}
}

// cmdStatus prints the detail view for one service.
func (a *app) cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.err)
	limit := fs.Int("events", 10, "number of recent events to show")
	name, err := parseServiceCommand(fs, "status", args)
	if err != nil {
		return err
	}

	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}
	st, err := c.Status(name)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "%s\n", st.Name)
	fmt.Fprintf(a.out, "  status:        %s\n", st.Status)
	if st.Halted {
		fmt.Fprintf(a.out, "  halted:        yes (not being restarted; `outpost start %s` resumes it)\n", name)
	}
	if st.Desired != "" && st.Desired != "running" {
		fmt.Fprintf(a.out, "  desired:       %s\n", st.Desired)
	}
	if st.Runtime != "" {
		fmt.Fprintf(a.out, "  runtime:       %s\n", st.Runtime)
		fmt.Fprintf(a.out, "  port:          %d\n", st.Port)
		fmt.Fprintf(a.out, "  healthcheck:   %s\n", st.Healthcheck)
		fmt.Fprintf(a.out, "  storage:       %s\n", st.Storage)
		writeTailnetDetail(a.out, st)
		fmt.Fprintf(a.out, "  restart:       %s (backoff %ds)\n", st.RestartPolicy, st.Backoff)
	} else {
		fmt.Fprintln(a.out, "  (no manifest: this service is no longer declared)")
	}
	if st.InstanceID != "" {
		fmt.Fprintf(a.out, "  instance:      %s\n", st.InstanceID)
	}
	fmt.Fprintf(a.out, "  restarts:      %d\n", st.RestartCount)
	if !st.LastStartedAt.IsZero() {
		fmt.Fprintf(a.out, "  last started:  %s (%s ago)\n",
			st.LastStartedAt.Format(time.RFC3339), truncateDur(time.Since(st.LastStartedAt)))
	}
	if !st.NextAttempt.IsZero() && st.NextAttempt.After(time.Now()) {
		fmt.Fprintf(a.out, "  next attempt:  in %s\n", truncateDur(time.Until(st.NextAttempt)))
	}

	events, err := c.Events(name, *limit)
	if err != nil {
		return err
	}
	if len(events) > 0 {
		fmt.Fprintln(a.out, "\n  recent events:")
		for _, e := range events {
			fmt.Fprintf(a.out, "    %s  %-12s %s\n", e.At.Format("15:04:05"), e.Kind, e.Message)
		}
	}
	return nil
}

// cmdLogs shows or follows a service's log file.
//
// Unlike the control commands this reads the log file directly: the file is
// local, and streaming a follow over the socket would need a framed protocol
// for no practical gain.
func (a *app) cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(a.err)
	follow := fs.Bool("f", false, "follow the log as it grows")
	events := fs.Bool("events", false, "show the activity feed instead of the log file")
	limit := fs.Int("n", 20, "number of events to show with --events")
	name, err := parseServiceCommand(fs, "logs", args)
	if err != nil {
		return err
	}

	// The activity feed lives in the state store and comes from the daemon,
	// unlike the log file, which is read directly (§8).
	if *events {
		return a.showEvents(name, *limit)
	}

	path := filepath.Join(a.logsDir(), name+".log")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no logs for %s yet (expected %s)", name, path)
		}
		return err
	}
	defer f.Close()

	if _, err := io.Copy(a.out, f); err != nil {
		return err
	}
	if !*follow {
		return nil
	}
	return a.followLog(f, path)
}

// showEvents prints a service's recent lifecycle events.
func (a *app) showEvents(name string, limit int) error {
	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}
	events, err := c.Events(name, limit)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintf(a.out, "no recorded activity for %s\n", name)
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		fmt.Fprintf(a.out, "%s  %-12s %s\n",
			e.At.Format("2006-01-02 15:04:05"), e.Kind, e.Message)
	}
	return nil
}

// followLog polls the open file for growth, reopening it if it is rotated or
// replaced underneath us.
func (a *app) followLog(f *os.File, path string) error {
	const poll = 300 * time.Millisecond
	current := f

	for {
		time.Sleep(poll)

		if _, err := io.Copy(a.out, current); err != nil {
			return err
		}

		onDisk, err := os.Stat(path)
		if err != nil {
			continue
		}
		here, err := current.Stat()
		if err != nil {
			continue
		}
		if !os.SameFile(onDisk, here) || onDisk.Size() < here.Size() {
			reopened, err := os.Open(path)
			if err != nil {
				continue
			}
			if current != f {
				current.Close()
			}
			current = reopened
		}
	}
}

// cmdStart, cmdStop and cmdRestart act on the running daemon.
func (a *app) cmdStart(args []string) error {
	return a.control("start", args, (*ipc.Client).Start)
}

func (a *app) cmdStop(args []string) error {
	return a.control("stop", args, (*ipc.Client).Stop)
}

func (a *app) cmdRestart(args []string) error {
	return a.control("restart", args, (*ipc.Client).Restart)
}

func (a *app) control(verb string, args []string, fn func(*ipc.Client, string) (string, error)) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(a.err)
	name, err := parseServiceCommand(fs, verb, args)
	if err != nil {
		return err
	}

	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}
	msg, err := fn(c, name)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, msg)
	return nil
}

// --- helpers ---

// parseServiceCommand parses a command that takes one service name, allowing
// flags on either side of it.
//
// Go's flag package stops at the first non-flag argument, and simply taking the
// first argument that does not start with "-" would swallow a flag's value
// ("status --events 20 web" would treat 20 as the service). Parsing in rounds
// handles both orders correctly.
func parseServiceCommand(fs *flag.FlagSet, verb string, args []string) (string, error) {
	var positional []string
	rest := args

	for {
		if err := fs.Parse(rest); err != nil {
			return "", err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}

	switch len(positional) {
	case 0:
		return "", fmt.Errorf("%s: a service name is required", verb)
	case 1:
		return positional[0], nil
	default:
		return "", fmt.Errorf("%s: unexpected argument %q", verb, positional[1])
	}
}

// parseOptionalService is parseServiceCommand for commands where the service
// name may be omitted to mean "all services".
func parseOptionalService(fs *flag.FlagSet, verb string, args []string) ([]string, error) {
	var positional []string
	rest := args

	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return positional, nil
}

func uptime(s supervisor.ServiceStatus) string {
	if s.Status != "running" || s.LastStartedAt.IsZero() {
		return "-"
	}
	return truncateDur(time.Since(s.LastStartedAt))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncateDur(d time.Duration) string {
	if d < time.Minute {
		return d.Truncate(time.Second).String()
	}
	if d < time.Hour {
		return d.Truncate(time.Minute).String()
	}
	return d.Truncate(time.Hour).String()
}
