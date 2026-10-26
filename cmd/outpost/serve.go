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

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
	"github.com/sroberts/outpost/internal/version"
)

// cmdServe runs the daemon: the reconcile loop plus the Unix socket the CLI
// talks to (§12). launchd keeps this alive across reboots; see `outpost install`.
func (a *app) cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(a.err)
	interval := fs.Duration("interval", 3*time.Second, "reconcile interval")
	tailnet := fs.Bool("tailnet", true, "put each service on the tailnet via tsnet (§7); --tailnet=false to run without it")
	tsVerbose := fs.Bool("tailnet-verbose", false, "pass tsnet's own logging through")
	startupTimeout := fs.Duration("startup-timeout", defaultStartupTimeout,
		"exit if bring-up has not finished within this long, so the service manager restarts the daemon; 0 disables")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Before anything else the daemon says, so ~/.outpost/logs opens by
	// recording which build produced everything underneath it.
	fmt.Fprintf(a.out, "outpost: %s starting\n", version.Short())

	// Everything up to the socket accepting connections runs under a
	// deadline (#30). Its stage names are what the log reports if it fires.
	watch := watchStartup(*startupTimeout, a.err, os.Exit)
	defer watch.finished()

	tailnetSet := false
	fs.Visit(func(f *flag.Flag) { tailnetSet = tailnetSet || f.Name == "tailnet" })

	watch.at("preparing " + a.home)
	fresh, err := a.createLayout()
	if err != nil {
		return err
	}
	if fresh {
		// An explicit --tailnet is the choice for a new home, not just for
		// this run. Detection is skipped: it can take seconds, and this is
		// inside the startup deadline.
		if tailnetSet {
			err = a.saveTailscale(*tailnet)
			fmt.Fprintf(a.out, "outpost: new Outpost home at %s; Tailscale is %s, from --tailnet\n", a.home, onOff(*tailnet))
		} else {
			err = a.firstRunTailscale(false)
		}
		if err != nil {
			return err
		}
	}
	if pid, running := a.supervisorPID(); running {
		return fmt.Errorf("an outpost daemon is already running (pid %d)", pid)
	}

	watch.at("opening the state store")
	store, err := state.Open(a.statePath())
	if err != nil {
		return err
	}
	defer store.Close()

	watch.at("opening the auth store")
	authStore, err := auth.Open(a.authPath())
	if err != nil {
		return err
	}
	defer authStore.Close()

	// config.toml decides whether the tailnet is used at all; an explicit
	// --tailnet on the command line overrides it for this run.
	useTailnet := a.config.TailscaleEnabled()
	if tailnetSet {
		useTailnet = *tailnet
	}

	watch.at("starting the tailnet router")
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
			fmt.Fprintln(a.out, "outpost: set TS_AUTHKEY, or run without Tailscale via `outpost config --tailscale off`")
		}
	} else {
		fmt.Fprintln(a.out, "outpost: Tailscale is disabled; services stay on loopback")
	}

	watch.at("loading manifests")
	procs := &runtime.ProcessRuntime{
		LogDir: a.logsDir(),
		// Tells each service whether identity headers can be trusted, and
		// where the keys that verify them are.
		Env: gateway.ServiceEnv(a.config.Gateway),
	}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:     a.servicesDir(),
		BackupDir:       a.backupsDir(),
		Tailnet:         router,
		ManifestOptions: manifest.Options{RequireTailscale: useTailnet},
		Store:           store,
		ProcessRuntime:  procs,
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
	watch.at("reclaiming orphans from a previous run")
	if reclaimed, err := sup.ReclaimOrphans(context.Background()); err != nil {
		fmt.Fprintf(a.err, "outpost: orphan reclaim: %v\n", err)
	} else if len(reclaimed) > 0 {
		fmt.Fprintf(a.out, "outpost: reclaimed %d service(s) from a previous run: %s\n",
			len(reclaimed), strings.Join(reclaimed, ", "))
	}

	watch.at("opening the control socket")
	srv, err := ipc.Listen(a.socketPath(), sup, func(format string, v ...any) {
		fmt.Fprintf(a.err, format+"\n", v...)
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.UseAuth(authStore)

	var gw *gateway.Gateway
	if a.config.Gateway.Enabled {
		watch.at("starting the gateway")
		gw, err = gateway.New(gateway.Options{
			Config:   a.config.Gateway,
			Auth:     authStore,
			Services: sup,
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
		})
		if err != nil {
			return err
		}
		if err := gw.Listen(); err != nil {
			return err
		}
		srv.UseGateway(gw)
	}

	if users, err := authStore.ListUsers(); err != nil {
		fmt.Fprintf(a.err, "outpost: auth store: %v\n", err)
	} else if len(users) == 0 {
		if gw != nil && gw.SetupURL() != "" {
			// The link carries a secret, but only to this log, and it's
			// dead once an admin exists or the daemon restarts.
			fmt.Fprintf(a.out, "outpost: no users yet; create the first admin at %s\n", gw.SetupURL())
			fmt.Fprintln(a.out, `outpost: or run "outpost user add <name> --admin"`)
		} else {
			fmt.Fprintln(a.out, `outpost: no users yet; create the first admin with "outpost user add <name> --admin"`)
		}
	}

	if err := a.writePID(); err != nil {
		return err
	}
	defer os.Remove(a.pidPath())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(a.out, "outpost: supervising %d service(s) from %s\n", len(sup.Services()), a.servicesDir())
	fmt.Fprintf(a.out, "outpost: listening on %s\n", srv.Addr())
	watch.finished()

	// The socket server runs alongside the loop; its handlers reach the
	// supervisor through the command queue, so the loop stays single-threaded.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	gwDone := make(chan struct{})
	if gw != nil {
		fmt.Fprintf(a.out, "outpost: gateway at %s, listening on %s\n", gw.PortalURL(), gw.Addr())
		go func() {
			defer close(gwDone)
			if err := gw.Serve(ctx); err != nil {
				fmt.Fprintf(a.err, "outpost: gateway: %v\n", err)
			}
		}()
	} else {
		close(gwDone)
	}

	// Run reloads manifests each tick and stops every service on the way out.
	runErr := sup.Run(ctx)
	srv.Close()
	if err := <-serveErr; err != nil {
		fmt.Fprintf(a.err, "outpost: socket server: %v\n", err)
	}
	<-gwDone

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
