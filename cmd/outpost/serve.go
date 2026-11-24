package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/exposure"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/logging"
	"github.com/sroberts/outpost/internal/mail"
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
	logLevel := fs.String("log-level", "",
		"least severe line the daemon logs: debug, info, warn or error; overrides [log] level and $OUTPOST_LOG_LEVEL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	level, err := a.daemonLogLevel(*logLevel)
	if err != nil {
		return err
	}

	// Everything up to the socket accepting connections runs under a
	// deadline (#30). Its stage names are what the log reports if it fires.
	watch := watchStartup(*startupTimeout, a.err, os.Exit)
	defer watch.finished()

	tailnetSet := false
	fs.Visit(func(f *flag.Flag) { tailnetSet = tailnetSet || f.Name == "tailnet" })

	// Ahead of the log file, which would otherwise make every home look
	// like one that already existed.
	watch.at("preparing " + a.home)
	fresh, err := a.createLayout()
	if err != nil {
		return err
	}

	// The daemon's own log: leveled and timestamped, in [log]'s format, on
	// stdout and in logs/outpost.log, which is there to read whatever
	// launchd or systemd does with stdout. Trouble from here also goes to
	// stderr, as it did, which launchd keeps in a file of its own.
	var logFile io.Writer
	if f, ferr := runtime.OpenLog(a.daemonLogPath()); ferr != nil {
		fmt.Fprintf(a.err, "outpost: %v; logging to stdout and stderr only\n", ferr)
	} else {
		defer f.Close()
		logFile = f
	}
	format := a.config.Log.FormatName()
	lg := logging.NewTee(a.out, logFile, level, format)
	lgErr := logging.NewTee(a.err, logFile, level, format)
	// Each part of the daemon logs under its own name, at its own level if
	// [log.levels] gives it one.
	component := func(name string) *log.Logger {
		return logging.Component(lg, name, a.config.Log.LevelFor(name, level))
	}
	say := logging.To(lg, nil).Infof
	sayErr := logging.To(lgErr, nil).Errorf

	// Before anything else the daemon says, so ~/.outpost/logs opens by
	// recording which build produced everything underneath it.
	say("%s starting", version.Short())

	if fresh {
		// An explicit --tailnet is the choice for a new home, not just for
		// this run. Detection is skipped: it can take seconds, and this is
		// inside the startup deadline.
		if tailnetSet {
			err = a.saveTailscale(*tailnet)
			say("new Outpost home at %s; Tailscale is %s, from --tailnet", a.home, onOff(*tailnet))
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
	aiOpts := aiOptions(a.config.AI, a.aiPath(), secretKeys{secretStore}, nil)
	aiLog := component("ai")
	aiOpts.Logger = aiLog
	aiService, err := ai.Open(aiOpts)
	if err != nil {
		return err
	}
	defer aiService.Close()
	// Bound before any app starts, since every app is told this address. A
	// port already taken is reported, not fatal: the apps still run, and
	// their AI requests fail until the daemon restarts on a free one.
	aiLn, err := net.Listen("tcp", a.config.AI.ListenAddr())
	if err != nil {
		sayErr("AI endpoint: %v; apps can't reach their models until [ai] listen names a free port and the daemon restarts", err)
		aiLn = nil
	} else {
		defer func() { _ = aiLn.Close() }() // Serve has stopped by then, or never started
	}

	watch.at("opening the mail store")
	mailOpts := mailOptions(a.config.Mail, a.mailPath(), secretKeys{secretStore}, nil)
	mailOpts.Logger = component("mail")
	mailService, err := mail.Open(mailOpts)
	if err != nil {
		return err
	}
	defer mailService.Close()
	// Bound before any app starts, as the AI endpoint is, and for the same
	// reason a port already taken isn't fatal.
	mailLn, err := net.Listen("tcp", a.config.Mail.ListenAddr())
	if err != nil {
		sayErr("mail endpoint: %v; apps can't reach their mail until [mail] listen names a free port and the daemon restarts", err)
		mailLn = nil
	} else {
		defer func() { _ = mailLn.Close() }() // Serve has stopped by then, or never started
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
			Logger:   component("tsnet"),
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
			say("no Tailscale auth key; a service's node that needs a login shows the URL in `outpost status <service>`")
			say("set one with `outpost config --authkey`, or run without Tailscale via `outpost config --tailscale off`")
		}
	} else {
		say("Tailscale is disabled; services stay on loopback")
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
		Secrets:         withAI{secrets: secretStore, ai: aiService, mail: mailService},
		Exposure:        exposureStore,
		Logger:          component("supervisor"),
	})
	if err != nil {
		return err
	}
	if err := sup.Apply(context.Background()); err != nil {
		return err
	}
	// An app's name is one of its mail routes.
	mailService.SetServices(func() []string {
		var names []string
		for _, svc := range sup.Services() {
			names = append(names, svc.Name)
		}
		return names
	})

	// A daemon killed with SIGKILL — OOM, power cut, `kill -9` — leaves its
	// services running and holding their ports. Clear them before starting
	// replacements, or every one of them restart-loops on a bound port.
	watch.at("reclaiming orphans from a previous run")
	if reclaimed, err := sup.ReclaimOrphans(context.Background()); err != nil {
		sayErr("orphan reclaim: %v", err)
	} else if len(reclaimed) > 0 {
		say("reclaimed %d service(s) from a previous run: %s",
			len(reclaimed), strings.Join(reclaimed, ", "))
	}

	watch.at("opening the control socket")
	srv, err := ipc.Listen(a.socketPath(), sup, nil)
	if err != nil {
		return err
	}
	defer srv.Close()
	srv.UseLogger(component("ipc"))
	srv.UseAuth(authStore)
	srv.UseSecrets(secretStore)
	srv.UseAI(aiService)
	srv.UseConfig(a.config.Fingerprint())
	srv.UseLogs(a.logsDir())

	var gw *gateway.Gateway
	// The TUI shows usage as well as the gateway's catalog, so the sampler
	// exists whether or not the gateway does; without the gateway it only
	// runs once a client asks for usage (demandStats). It reads the service
	// list like any other client; traffic comes from the gateway when there
	// is one, which exists by the time the sampler starts.
	sampler := stats.New(stats.Options{
		Services: sup,
		Traffic: func() map[string]stats.Bytes {
			if gw == nil {
				return nil
			}
			return gw.Traffic()
		},
		Logger: component("stats"),
	})
	if a.config.Gateway.Enabled {
		watch.at("starting the gateway")
		// Every request, always, whatever the daemon's level: rotated like
		// the rest, and closed only once the gateway has stopped writing.
		var accessLog io.Writer
		if f, ferr := runtime.OpenLog(filepath.Join(a.logsDir(), applog.AccessLogFile)); ferr != nil {
			sayErr("gateway access log: %v; requests won't be recorded", ferr)
		} else {
			defer f.Close()
			accessLog = f
		}
		gw, err = gateway.New(gateway.Options{
			Config:   a.config.Gateway,
			Auth:     authStore,
			Services: sup,
			Events:   sup,
			Logger:   component("gateway"),

			TailnetHosts: tailnetHosts,
			PortalNode:   portalNodeStatus(tsRouter),
			Updates:      supervisedUpdates{daemonUpdates{a}, sup},
			SelfUpdates:  &selfWeb{a: a},
			Adds:         daemonAdds{a},
			Secrets:      secretStore,
			Exposure:     exposureWeb{a: a, store: exposureStore, sup: sup, opts: a.manifestOptions(useTailnet)},
			AI:           aiService,
			Mail:         mailService,
			Control:      sup,
			Stats:        sampler,
			Logs:         logsSource{dir: a.logsDir(), services: sup, events: sup.Events},
			AccessLog:    accessLog,
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
				logging.To(lg, nil).Warnf("[gateway] tailscale_name = %q, but this daemon runs without Tailscale, so the portal stays off the tailnet", name)
			} else if err := tsRouter.AttachPortal(context.Background(), name, gw.PortalHost()); err != nil {
				sayErr("portal: %v", err)
			}
		}
	}

	if users, err := authStore.ListUsers(); err != nil {
		sayErr("auth store: %v", err)
	} else if len(users) == 0 {
		if gw != nil && gw.SetupURL() != "" {
			// The link carries a secret, but only to this log, and it's
			// dead once an admin exists or the daemon restarts.
			say("no users yet; create the first admin at %s", gw.SetupURL())
			say(`or run "outpost user add <name> --admin"`)
		} else {
			say(`no users yet; create the first admin with "outpost user add <name> --admin"`)
		}
	}

	if err := a.writePID(); err != nil {
		return err
	}
	defer os.Remove(a.pidPath())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	say("supervising %d service(s) from %s", len(sup.Services()), a.servicesDir())
	say("listening on %s", srv.Addr())
	watch.finished()

	// Registered before Serve, since the stats command starts it. The
	// gateway's catalog wants usage from the start, so with the gateway on it
	// starts now.
	usage := newDemandStats(ctx, sampler)
	srv.UseStats(usage)
	if gw != nil {
		usage.start()
	}

	// The socket server runs alongside the loop; its handlers reach the
	// supervisor through the command queue, so the loop stays single-threaded.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	gwDone := make(chan struct{})
	if gw != nil {
		say("gateway at %s, listening on %s", gw.PortalURL(), gw.Addr())
		// Ends with ctx, as Serve does; nothing it serves outlives the stores.
		go func() {
			if err := gw.ServePublic(ctx, publicLn); err != nil {
				sayErr("gateway public listener: %v", err)
			}
		}()
		go func() {
			defer close(gwDone)
			if err := gw.Serve(ctx); err != nil {
				sayErr("gateway: %v", err)
			}
		}()
	} else {
		close(gwDone)
	}

	aiDone := make(chan struct{})
	if aiLn != nil {
		say("AI endpoint at %s, models from Ollama at %s", aiService.URL(), aiService.OllamaURL())
		go func() {
			defer close(aiDone)
			if err := aiService.Serve(ctx, aiLn); err != nil {
				sayErr("AI endpoint: %v", err)
			}
		}()
		// Said once at start, off the startup path: a model config.toml
		// names that Ollama doesn't have, or an Ollama that isn't running.
		go warnAIModels(ctx, aiService, logging.Writer(aiLog, log.WarnLevel))
	} else {
		close(aiDone)
	}

	mailDone := make(chan struct{})
	// The endpoint reads and writes mail.db too, so it is waited for as well.
	mailServeDone := make(chan struct{})
	if mailLn != nil {
		say("mail endpoint at %s", mailService.URL())
		go func() {
			defer close(mailServeDone)
			if err := mailService.Serve(ctx, mailLn); err != nil {
				sayErr("mail endpoint: %v", err)
			}
		}()
	} else {
		close(mailServeDone)
	}
	// Reading the inbox waits on Gmail, so it runs on its own, and is over
	// before mail.db closes.
	go func() {
		defer close(mailDone)
		mailService.Run(ctx)
	}()

	// Off the loop, which must never wait on the network, and over by the time
	// the store closes.
	checksDone := make(chan struct{})
	go func() {
		defer close(checksDone)
		sup.RunReleaseChecks(ctx)
	}()

	// Run reloads manifests each tick and stops every service on the way out.
	runErr := sup.Run(ctx)
	<-checksDone
	// The sampler is off the loop too, and lists services, so it must stop
	// before the stores close.
	usage.stop()
	srv.Close()
	if err := <-serveErr; err != nil {
		sayErr("socket server: %v", err)
	}
	<-gwDone
	<-aiDone
	<-mailDone
	<-mailServeDone

	say("stopped")
	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}

// daemonLogLevel is the level the daemon logs at: --log-level if given,
// otherwise $OUTPOST_LOG_LEVEL, otherwise [log] level. An override that
// isn't a level is refused rather than ignored, as a bad [log] level is.
func (a *app) daemonLogLevel(flagValue string) (string, error) {
	for _, v := range []struct{ from, level string }{
		{"--log-level", flagValue},
		{"OUTPOST_LOG_LEVEL", os.Getenv("OUTPOST_LOG_LEVEL")},
	} {
		if v.level == "" {
			continue
		}
		if err := config.ValidLogLevel(v.level); err != nil {
			return "", fmt.Errorf("%s: %w", v.from, err)
		}
		return strings.ToLower(strings.TrimSpace(v.level)), nil
	}
	return a.config.Log.LevelName(), nil
}

// daemonLogPath is the daemon's own log, beside the services' logs.
func (a *app) daemonLogPath() string { return filepath.Join(a.logsDir(), applog.DaemonLogFile) }

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
