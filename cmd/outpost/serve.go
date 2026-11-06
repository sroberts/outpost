package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/exposure"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/secrets"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/stats"
	"github.com/sroberts/outpost/internal/supervisor"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
	"github.com/sroberts/outpost/internal/version"
)

// The portal's Manage menu drives the supervisor directly, through its API.
var _ gateway.Control = (*supervisor.Supervisor)(nil)

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

	watch.at("opening the secrets store")
	secretStore, err := secrets.Open(a.secretsPath(), a.secretsKeyPath())
	if err != nil {
		return err
	}
	defer secretStore.Close()

	watch.at("opening the exposure store")
	exposureStore, err := exposure.Open(a.exposurePath())
	if err != nil {
		return err
	}
	defer exposureStore.Close()

	watch.at("opening the AI store")
	aiService, err := ai.Open(ai.Options{
		Path:      a.aiPath(),
		OllamaURL: a.config.AI.OllamaBase(),
		Listen:    a.config.AI.ListenAddr(),
		LLM:       a.config.AI.LLM,
		System1:   a.config.AI.System1,
		Logf:      func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
	})
	if err != nil {
		return err
	}
	defer aiService.Close()
	// Bound before any app starts, since every app is told this address. A
	// port already taken is reported, not fatal: the apps still run, and
	// their AI requests fail until the daemon restarts on a free one.
	aiLn, err := net.Listen("tcp", a.config.AI.ListenAddr())
	if err != nil {
		fmt.Fprintf(a.err, "outpost: AI endpoint: %v; apps can't reach their models until [ai] listen names a free port and the daemon restarts\n", err)
		aiLn = nil
	} else {
		defer func() { _ = aiLn.Close() }() // Serve has stopped by then, or never started
	}

	// config.toml decides whether the tailnet is used at all; an explicit
	// --tailnet on the command line overrides it for this run.
	useTailnet := a.config.TailscaleEnabled()
	if tailnetSet {
		useTailnet = *tailnet
	}

	// Shown by the tailnet nodes when they ask the gateway about a tcp
	// forward's caller. New each run: both ends are this process's own, so
	// nothing needs it to last.
	var nodeSecret string
	if a.config.Gateway.Enabled {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("node secret: %w", err)
		}
		nodeSecret = hex.EncodeToString(b)
	}
	// The gateway's public listener, where nodes with Funnel send what
	// arrives from the internet. Bound now, on loopback, so the router can be
	// told where it is before any node starts.
	var publicLn net.Listener
	if a.config.Gateway.Enabled {
		if publicLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return fmt.Errorf("gateway public listener: %w", err)
		}
		defer func() { _ = publicLn.Close() }() // ServePublic has stopped by then, or never started
	}

	watch.at("starting the tailnet router")
	var router supervisor.TailnetRouter
	var tailnetHosts func() map[string]string
	var tsRouter *outposttsnet.Router
	if useTailnet {
		authKey := a.config.ResolveAuthKey()
		opts := outposttsnet.Options{
			StateDir: a.tsnetDir(),
			AuthKey:  authKey,
			Verbose:  *tsVerbose,
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
		}
		// With the gateway on, a service's tailnet address goes through its
		// login like every other way in, rather than straight to the service.
		if a.config.Gateway.Enabled {
			opts.Upstream, opts.UpstreamServerName = gateway.LocalUpstream(a.config.Gateway)
			opts.AccessSecret = nodeSecret
			opts.PublicUpstream = "http://" + publicLn.Addr().String()
		}
		r, err := outposttsnet.New(opts)
		if err != nil {
			return err
		}
		defer r.Close()
		router = reportingRouter{r}
		tailnetHosts = r.Hosts
		tsRouter = r

		if authKey == "" {
			fmt.Fprintln(a.out, "outpost: no Tailscale auth key; a service's node that needs a login shows the URL in `outpost status <service>`")
			fmt.Fprintln(a.out, "outpost: set one with `outpost config --authkey`, or run without Tailscale via `outpost config --tailscale off`")
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
		ManifestOptions: a.manifestOptions(useTailnet),
		Store:           store,
		ProcessRuntime:  procs,
		Interval:        *interval,
		CheckRelease:    (&releaseChecks{a: a}).check,
		Secrets:         withAI{secrets: secretStore, ai: aiService},
		Exposure:        exposureStore,
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
	srv.UseSecrets(secretStore)
	srv.UseAI(aiService)
	srv.UseConfig(a.config.Fingerprint())

	var gw *gateway.Gateway
	// Only the gateway's catalog shows usage, so without it there is nothing
	// to sample for and no ps or storage walks to pay for.
	var sampler *stats.Sampler
	if a.config.Gateway.Enabled {
		// Reads the service list like any other client; traffic comes from
		// the gateway, which exists by the time Run starts the sampler.
		sampler = stats.New(stats.Options{
			Services: sup,
			Traffic:  func() map[string]stats.Bytes { return gw.Traffic() },
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },
		})
		watch.at("starting the gateway")
		gw, err = gateway.New(gateway.Options{
			Config:   a.config.Gateway,
			Auth:     authStore,
			Services: sup,
			Events:   sup,
			Logf:     func(format string, v ...any) { fmt.Fprintf(a.out, format+"\n", v...) },

			TailnetHosts: tailnetHosts,
			PortalNode:   portalNodeStatus(tsRouter),
			Updates:      supervisedUpdates{daemonUpdates{a}, sup},
			SelfUpdates:  &selfWeb{a: a},
			Adds:         daemonAdds{a},
			Secrets:      secretStore,
			Exposure:     exposureWeb{a: a, store: exposureStore, sup: sup, opts: a.manifestOptions(useTailnet)},
			AI:           aiService,
			Control:      sup,
			Stats:        sampler,
			NodeSecret:   nodeSecret,
		})
		if err != nil {
			return err
		}
		if err := gw.Listen(); err != nil {
			return err
		}
		srv.UseGateway(gw)

		// The portal isn't a service, so its node is attached here once,
		// not by the reconcile loop. Non-blocking, like a service's.
		if name := a.config.Gateway.TailscaleName; name != "" {
			if tsRouter == nil {
				fmt.Fprintf(a.out, "outpost: [gateway] tailscale_name = %q, but this daemon runs without Tailscale, so the portal stays off the tailnet\n", name)
			} else if err := tsRouter.AttachPortal(context.Background(), name, gw.PortalHost()); err != nil {
				fmt.Fprintf(a.err, "outpost: portal: %v\n", err)
			}
		}
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
		// Ends with ctx, as Serve does; nothing it serves outlives the stores.
		go func() {
			if err := gw.ServePublic(ctx, publicLn); err != nil {
				fmt.Fprintf(a.err, "outpost: gateway public listener: %v\n", err)
			}
		}()
		go func() {
			defer close(gwDone)
			if err := gw.Serve(ctx); err != nil {
				fmt.Fprintf(a.err, "outpost: gateway: %v\n", err)
			}
		}()
	} else {
		close(gwDone)
	}

	aiDone := make(chan struct{})
	if aiLn != nil {
		fmt.Fprintf(a.out, "outpost: AI endpoint at %s, models from Ollama at %s\n", aiService.URL(), aiService.OllamaURL())
		go func() {
			defer close(aiDone)
			if err := aiService.Serve(ctx, aiLn); err != nil {
				fmt.Fprintf(a.err, "outpost: AI endpoint: %v\n", err)
			}
		}()
		// Said once at start, off the startup path: a model config.toml
		// names that Ollama doesn't have, or an Ollama that isn't running.
		go warnAIModels(ctx, aiService, a.out)
	} else {
		close(aiDone)
	}

	// Off the loop, which must never wait on the network, and over by the time
	// the store closes.
	checksDone := make(chan struct{})
	go func() {
		defer close(checksDone)
		sup.RunReleaseChecks(ctx)
	}()
	// Also off the loop; it lists services, so it too must stop before the
	// stores close.
	statsDone := make(chan struct{})
	if sampler != nil {
		go func() {
			defer close(statsDone)
			sampler.Run(ctx)
		}()
	} else {
		close(statsDone)
	}

	// Run reloads manifests each tick and stops every service on the way out.
	runErr := sup.Run(ctx)
	<-checksDone
	<-statsDone
	srv.Close()
	if err := <-serveErr; err != nil {
		fmt.Fprintf(a.err, "outpost: socket server: %v\n", err)
	}
	<-gwDone
	<-aiDone

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

// manifestOptions are the manifest rules this configuration implies.
// useTailnet is whether this run puts services on the tailnet.
func (a *app) manifestOptions(useTailnet bool) manifest.Options {
	opts := manifest.Options{RequireTailscale: useTailnet, RequireDNSName: a.config.Gateway.Enabled}
	if a.config.Gateway.Enabled {
		opts.PortalTailscaleName = a.config.Gateway.TailscaleName
		opts.Gateway = true
	}
	return opts
}

// portalNodeStatus reports the portal's node to the gateway, or nil without
// a router, in which case the portal has no node.
func portalNodeStatus(r *outposttsnet.Router) func() (gateway.PortalNode, bool) {
	if r == nil {
		return nil
	}
	return func() (gateway.PortalNode, bool) {
		ns, ok := r.NodeStatus(outposttsnet.PortalNode)
		return gateway.PortalNode{State: ns.State, Host: ns.Host, LoginURL: ns.LoginURL, Error: ns.Error}, ok
	}
}

// reportingRouter lets the supervisor ask the router how each node is doing,
// for `outpost list` and `status`.
type reportingRouter struct{ *outposttsnet.Router }

func (r reportingRouter) TailnetStatus(name string) (supervisor.TailnetStatus, bool) {
	ns, ok := r.NodeStatus(name)
	// Mapped by name rather than copied, so the two packages' states can't
	// drift apart unnoticed: the strings happen to match, the constants are
	// what each side switches on.
	state := supervisor.TailnetStarting
	switch ns.State {
	case outposttsnet.NodeOn:
		state = supervisor.TailnetOn
	case outposttsnet.NodeNeedsLogin:
		state = supervisor.TailnetNeedsLogin
	}
	funnel := ""
	switch ns.Funnel {
	case outposttsnet.NodeFunnelOn:
		funnel = supervisor.FunnelOn
	case outposttsnet.NodeFunnelUnavailable:
		funnel = supervisor.FunnelUnavailable
	case outposttsnet.NodeFunnelStarting:
		funnel = supervisor.FunnelStarting
	}
	return supervisor.TailnetStatus{State: state, Host: ns.Host, LoginURL: ns.LoginURL, Error: ns.Error,
		Funnel: funnel, FunnelError: ns.FunnelError}, ok
}
