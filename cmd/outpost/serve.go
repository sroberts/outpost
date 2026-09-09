package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// cmdServe runs the daemon: the reconcile loop plus the Unix socket the CLI
// talks to (§12). launchd keeps this alive across reboots; see `outpost install`.
func (a *app) cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(a.err)
	interval := fs.Duration("interval", 3*time.Second, "reconcile interval")
	tailnet := fs.Bool("tailnet", false, "put each service on the tailnet via tsnet (§7)")
	tsVerbose := fs.Bool("tailnet-verbose", false, "pass tsnet's own logging through")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := a.ensureLayout(); err != nil {
		return err
	}
	if pid, running := a.supervisorPID(); running {
		return fmt.Errorf("an outpost daemon is already running (pid %d)", pid)
	}

	store, err := state.Open(a.statePath())
	if err != nil {
		return err
	}
	defer store.Close()

	// Tailscale is opt-in: the first run of a node registers it on your tailnet,
	// which should not happen as a side effect of starting the daemon.
	var router supervisor.TailnetRouter
	if *tailnet {
		r, err := outposttsnet.New(outposttsnet.Options{
			StateDir: a.tsnetDir(),
			AuthKey:  os.Getenv("TS_AUTHKEY"),
			Verbose:  *tsVerbose,
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
		})
		if err != nil {
			return err
		}
		defer r.Close()
		router = r

		if os.Getenv("TS_AUTHKEY") == "" {
			fmt.Fprintln(a.out, "outpost: TS_AUTHKEY is not set; each new service will log a one-time login URL")
		}
	}

	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    a.servicesDir(),
		BackupDir:      a.backupsDir(),
		Tailnet:        router,
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: a.logsDir()},
		Interval:       *interval,
		Logf:           func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
	})
	if err != nil {
		return err
	}
	if err := sup.Apply(context.Background()); err != nil {
		return err
	}

	srv, err := ipc.Listen(a.socketPath(), sup, func(format string, v ...any) {
		fmt.Fprintf(a.err, format+"\n", v...)
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	if err := a.writePID(); err != nil {
		return err
	}
	defer os.Remove(a.pidPath())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(a.out, "outpost: supervising %d service(s) from %s\n", len(sup.Services()), a.servicesDir())
	fmt.Fprintf(a.out, "outpost: listening on %s\n", srv.Addr())

	// The socket server runs alongside the loop; its handlers reach the
	// supervisor through the command queue, so the loop stays single-threaded.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// Run reloads manifests each tick and stops every service on the way out.
	runErr := sup.Run(ctx)
	srv.Close()
	if err := <-serveErr; err != nil {
		fmt.Fprintf(a.err, "outpost: socket server: %v\n", err)
	}

	fmt.Fprintln(a.out, "outpost: stopped")
	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}

// writePID records the running daemon so the CLI can report whether one is up.
func (a *app) writePID() error {
	return os.WriteFile(a.pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// supervisorPID reports the running daemon's PID, if there is one. A stale
// pidfile (process gone) reads as not running.
func (a *app) supervisorPID() (int, bool) {
	body, err := os.ReadFile(a.pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	// Signal 0 tests for existence without delivering a signal.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return pid, false
	}
	return pid, true
}
