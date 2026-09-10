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
	"github.com/sroberts/outpost/internal/manifest"
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
	tailnet := fs.Bool("tailnet", true, "put each service on the tailnet via tsnet (§7); --tailnet=false to run without it")
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

	// config.toml decides whether the tailnet is used at all; an explicit
	// --tailnet on the command line overrides it for this run.
	useTailnet := a.config.TailscaleEnabled()
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "tailnet" {
			useTailnet = *tailnet
		}
	})

	var router supervisor.TailnetRouter
	if useTailnet {
		authKey := a.config.ResolveAuthKey()
		r, err := outposttsnet.New(outposttsnet.Options{
			StateDir: a.tsnetDir(),
			AuthKey:  authKey,
			Verbose:  *tsVerbose,
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
		})
		if err != nil {
			return err
		}
		defer r.Close()
		router = r

		if authKey == "" {
			fmt.Fprintln(a.out, "outpost: no Tailscale auth key; each new service will log a one-time login URL")
			fmt.Fprintln(a.out, "outpost: set TS_AUTHKEY, or run without Tailscale via `outpost config --init` and tailscale.enabled = false")
		}
	} else {
		fmt.Fprintln(a.out, "outpost: Tailscale is disabled; services stay on loopback")
	}

	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:     a.servicesDir(),
		BackupDir:       a.backupsDir(),
		Tailnet:         router,
		ManifestOptions: manifest.Options{RequireTailscale: useTailnet},
		Store:           store,
		ProcessRuntime:  &runtime.ProcessRuntime{LogDir: a.logsDir()},
		Interval:        *interval,
		Logf:            func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
	})
	if err != nil {
		return err
	}
	if err := sup.Apply(context.Background()); err != nil {
		return err
	}

	// A daemon killed with SIGKILL — OOM, power cut, `kill -9` — leaves its
	// services running and holding their ports. Clear them before starting
	// replacements, or every one of them restart-loops on a bound port.
	if reclaimed, err := sup.ReclaimOrphans(context.Background()); err != nil {
		fmt.Fprintf(a.err, "outpost: orphan reclaim: %v\n", err)
	} else if len(reclaimed) > 0 {
		fmt.Fprintf(a.out, "outpost: reclaimed %d service(s) from a previous run: %s\n",
			len(reclaimed), strings.Join(reclaimed, ", "))
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
