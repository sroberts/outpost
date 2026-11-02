// Package supervisor reconciles running state to the manifests on disk.
//
// The manifest is the source of truth for which services exist; the state store
// records what was observed and carries the user's start/stop intent back from
// the CLI (§6, §8). Everything that decides *whether* something should run
// lives here — the runtime layer only knows how to start and stop things.
package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
)

const (
	desiredRunning = state.DesiredRunning
	desiredStopped = state.DesiredStopped
	statusStopped  = runtime.StatusStopped

	defaultInterval            = 3 * time.Second
	defaultHealthFailThreshold = 3
	maxBackoff                 = 300 * time.Second
	// stableAfter is how long a service must run healthily before its restart
	// backoff is forgiven; without it a service that crashes once an hour would
	// creep up to the cap.
	stableAfter = 60 * time.Second

	eventPruneAge   = 14 * 24 * time.Hour
	eventPruneRows  = 5000
	eventPruneEvery = 100 // ticks
)

// TailnetRouter puts services on the tailnet. It is an interface so the
// supervisor can be tested without Tailscale credentials (§13); the real
// implementation is internal/tsnet.Router.
type TailnetRouter interface {
	Attach(ctx context.Context, svc *manifest.Service) error
	Detach(ctx context.Context, name string) error
}

// TailnetReporter is a TailnetRouter that can say how a service's node is
// doing, rather than only what was asked of it. Optional, so a router that
// can't is still a router.
type TailnetReporter interface {
	TailnetStatus(name string) (TailnetStatus, bool)
}

// Options configures a Supervisor.
type Options struct {
	// ServicesDir holds the per-service TOML manifests.
	ServicesDir string
	Store       *state.Store
	// ProcessRuntime runs runtime = "process" services.
	ProcessRuntime runtime.Runtime
	// Interval is the reconcile period; defaults to 3s.
	Interval time.Duration
	// HealthFailThreshold is how many consecutive failed healthchecks force a
	// restart; defaults to 3.
	HealthFailThreshold int
	// BackupDir holds per-service archive directories (§10).
	BackupDir string
	// Tailnet, when set, gives each running service a Tailscale identity (§7).
	Tailnet TailnetRouter
	// ManifestOptions controls validation rules that depend on global config,
	// such as whether tailscale_name is required.
	ManifestOptions manifest.Options
	// CheckRelease, when set, asks a service's source about its latest release.
	// RunReleaseChecks calls it, never the reconcile loop, since it goes over
	// the network.
	CheckRelease func(ctx context.Context, svc *manifest.Service) (ReleaseCheck, error)
	// Now is injectable so tests can control backoff without sleeping.
	Now func() time.Time
	// Logf receives operator-facing progress lines; defaults to discarding.
	Logf func(format string, args ...any)
}

// Supervisor owns the reconcile loop. It is not safe for concurrent use by
// multiple goroutines; Run drives it from one.
type Supervisor struct {
	servicesDir  string
	backupDir    string
	store        *state.Store
	procRT       runtime.Runtime
	tailnet      TailnetRouter
	manifestOpts manifest.Options

	Interval            time.Duration
	healthFailThreshold int
	now                 func() time.Time
	logf                func(format string, args ...any)

	// desired is the last successfully applied manifest set, keyed by name.
	desired map[string]*manifest.Service
	// tracked is per-service supervision state.
	tracked map[string]*svcState

	// cmds carries API calls into the reconcile loop; stopped is closed when
	// the loop exits so pending callers fail fast instead of hanging.
	cmds    chan command
	stopped chan struct{}
	// loopRunning is true while Run is draining commands, so the exported API
	// knows whether to hand work to the loop or run it inline.
	loopRunning atomic.Bool
	// loopMu serializes inline API calls against the loop. loopRunning alone is
	// not enough: a caller can read it as false in the instant before Run sets
	// it, and would then execute on its own goroutine while the loop ticks.
	// The loop holds this while working, so an inline caller either gets there
	// first or waits and re-checks.
	loopMu sync.Mutex

	ticks int
	// probes holds this tick's healthchecks, taken concurrently before the
	// services are reconciled. Reconcile-loop goroutine only.
	probes map[string]probeResult
	// heartbeat tells a failed healthcheck caused by the daemon stalling apart
	// from one caused by the service. It runs only while Run does.
	heartbeat *heartbeat
	// fingerprint of the manifest set at the last successful Apply, so serve
	// can re-apply only when something on disk actually changed.
	fingerprint string

	checkRelease func(ctx context.Context, svc *manifest.Service) (ReleaseCheck, error)
	// releases is what's known about each service's latest release. It is a
	// cache that a restart simply fills again, so it stays out of the state
	// store. Reconcile-loop goroutine only.
	releases map[string]*knownRelease
}

// svcState is what the supervisor remembers about one service between ticks.
type svcState struct {
	svc    *manifest.Service
	handle runtime.Handle
	status runtime.Status

	healthFails  int
	restartCount int
	backoff      time.Duration
	nextAttempt  time.Time
	startedAt    time.Time
	// lastRestartEpoch is the restart request this supervisor has already acted on.
	lastRestartEpoch int64
	// reaped guards against handling the same exit twice.
	reaped bool
	// everHealthy is true once this instance has passed a healthcheck; until
	// then the service is inside its startup grace window.
	everHealthy bool
	// halted mirrors the store's halt flag: the supervisor has stopped trying
	// to keep this service running, per its restart_policy.
	halted bool
}

// New builds a Supervisor. It does not touch the runtime or load manifests;
// call Apply for that.
func New(opts Options) (*Supervisor, error) {
	if opts.ServicesDir == "" {
		return nil, errors.New("supervisor: ServicesDir is required")
	}
	if opts.Store == nil {
		return nil, errors.New("supervisor: Store is required")
	}
	if opts.ProcessRuntime == nil {
		return nil, errors.New("supervisor: ProcessRuntime is required")
	}

	s := &Supervisor{
		servicesDir:         opts.ServicesDir,
		backupDir:           opts.BackupDir,
		store:               opts.Store,
		procRT:              opts.ProcessRuntime,
		tailnet:             opts.Tailnet,
		manifestOpts:        opts.ManifestOptions,
		Interval:            opts.Interval,
		healthFailThreshold: opts.HealthFailThreshold,
		now:                 opts.Now,
		logf:                opts.Logf,
		desired:             map[string]*manifest.Service{},
		tracked:             map[string]*svcState{},
		cmds:                make(chan command, 64),
		stopped:             make(chan struct{}),
		heartbeat:           newHeartbeat(heartbeatPeriod, stallThreshold),
		checkRelease:        opts.CheckRelease,
		releases:            map[string]*knownRelease{},
	}
	if s.Interval <= 0 {
		s.Interval = defaultInterval
	}
	if s.healthFailThreshold <= 0 {
		s.healthFailThreshold = defaultHealthFailThreshold
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// applyLocked is Apply's body. Reconcile-loop goroutine only.
func (s *Supervisor) applyLocked(ctx context.Context) error {
	services, err := manifest.LoadDir(s.servicesDir)
	if err != nil {
		return fmt.Errorf("load manifests: %w", err)
	}
	if errs := manifest.ValidateWith(services, s.manifestOpts); len(errs) > 0 {
		return fmt.Errorf("invalid manifests:\n%s", indentErrors(errs))
	}
	// Here rather than only in `outpost apply`, so a manifest edited and
	// picked up by Reload, or read at startup, is pointed out too.
	for _, w := range manifest.Warnings(services, s.manifestOpts) {
		s.logf("warning: %v", w)
	}

	next := make(map[string]*manifest.Service, len(services))
	for _, svc := range services {
		// Fail before starting anything rather than mid-way through the set.
		if err := svc.EnsureStorage(); err != nil {
			return err
		}
		next[svc.Name] = svc
	}

	s.desired = next
	if fp, err := s.manifestFingerprint(); err == nil {
		s.fingerprint = fp
	}
	s.event("", state.EventApply, fmt.Sprintf("applied %d service(s)", len(next)))
	return nil
}

// Reload re-applies the manifests if they have changed on disk, and reports
// whether it did. A failed reload leaves the previous set in force.
//
// Polling a hash of the directory keeps this dependency-free; §6 calls for
// fsnotify, which would only reduce the reload latency below one tick.
func (s *Supervisor) reloadLocked(ctx context.Context) (bool, error) {
	fp, err := s.manifestFingerprint()
	if err != nil {
		return false, err
	}
	if fp == s.fingerprint {
		return false, nil
	}
	if err := s.applyLocked(ctx); err != nil {
		// Record the fingerprint anyway so a broken manifest is reported once
		// per edit rather than on every tick.
		s.fingerprint = fp
		return false, err
	}
	return true, nil
}

// manifestFingerprint hashes the name, size and mtime of every manifest file.
func (s *Supervisor) manifestFingerprint() (string, error) {
	entries, err := os.ReadDir(s.servicesDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", s.servicesDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		fi, err := os.Stat(filepath.Join(s.servicesDir, name))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", name, fi.Size(), fi.ModTime().UnixNano())
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// servicesLocked returns the applied manifest set. Reconcile-loop goroutine only.
func (s *Supervisor) servicesLocked() []*manifest.Service {
	out := make([]*manifest.Service, 0, len(s.desired))
	for _, svc := range s.desired {
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Apply loads and validates every manifest, replacing the desired set. It is
// all-or-nothing: an invalid manifest set leaves the previous one in force
// rather than half-applying (§5).
//
// Safe to call whether or not the reconcile loop is running: while it is, the
// work is handed to the loop rather than racing it.
func (s *Supervisor) Apply(ctx context.Context) error {
	_, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		return nil, s.applyLocked(ctx)
	})
	return err
}

// Reload re-applies the manifests if they changed on disk, reporting whether
// it did. Safe to call from any goroutine.
func (s *Supervisor) Reload(ctx context.Context) (bool, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		return s.reloadLocked(ctx)
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// Tick performs one reconcile pass. Safe to call from any goroutine.
//
// The running daemon does not call this -- Run ticks on its own timer. It
// exists so tests can drive reconciliation a step at a time instead of waiting
// on wall-clock intervals, which is also why `deadcode` reports it unreachable
// from main. It is not dead.
func (s *Supervisor) Tick(ctx context.Context) {
	// Tick has no error to report; a stopped loop simply has nothing to do.
	_, _ = s.submit(ctx, func(s *Supervisor) (any, error) {
		s.tickLocked(ctx)
		return nil, nil
	})
}

// Services returns the currently applied manifest set, ordered by name. Safe to
// call from any goroutine.
func (s *Supervisor) Services() []*manifest.Service {
	v, err := s.submit(context.Background(), func(s *Supervisor) (any, error) {
		return s.servicesLocked(), nil
	})
	if err != nil {
		return nil
	}
	return v.([]*manifest.Service)
}

// Run reconciles on a ticker until ctx is cancelled, then stops everything it
// started. A foreground supervisor that left orphans behind would make the next
// `outpost apply` fight for ports.
//
// Manifest reloading happens here rather than in a watcher goroutine: the
// Supervisor's state is deliberately single-threaded, so reload and reconcile
// must share one goroutine.
func (s *Supervisor) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	defer close(s.stopped)

	s.loopRunning.Store(true)
	defer s.loopRunning.Store(false)

	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go s.heartbeat.run(beatCtx)

	s.withLoopLock(func() { s.tickLocked(ctx) })
	for {
		select {
		case <-ctx.Done():
			// Shut down against a live context; ctx is already cancelled.
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			// Stop routing API calls through the queue before shutting down —
			// nothing will drain it after this point — but hold the lock across
			// the change so an inline caller cannot slip in mid-shutdown.
			var err error
			s.withLoopLock(func() {
				s.loopRunning.Store(false)
				err = s.shutdownLocked(stopCtx)
			})
			return err
		case cmd := <-s.cmds:
			s.withLoopLock(func() {
				value, cmdErr := cmd.fn(s)
				cmd.resp <- commandResult{value: value, err: cmdErr}
			})
		case <-ticker.C:
			s.withLoopLock(func() { s.reloadAndTick(ctx) })
		}
	}
}

// withLoopLock runs fn holding the lock that keeps inline API calls out.
func (s *Supervisor) withLoopLock(fn func()) {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	fn()
}

// reloadAndTick picks up manifest edits, then reconciles.
func (s *Supervisor) reloadAndTick(ctx context.Context) {
	changed, err := s.reloadLocked(ctx)
	switch {
	case err != nil:
		s.logf("outpost: manifests not reloaded, previous set still in force: %v", err)
	case changed:
		s.logf("outpost: reloaded manifests (%d service(s))", len(s.desired))
	}
	s.tickLocked(ctx)
}

// tickLocked is Tick's body. Reconcile-loop goroutine only.
func (s *Supervisor) tickLocked(ctx context.Context) {
	s.ticks++

	// Stop anything no longer declared.
	for name, st := range s.tracked {
		if _, ok := s.desired[name]; !ok {
			s.retireLocked(ctx, name, st)
		}
	}

	s.probes = s.probeAll(ctx)
	for _, svc := range s.servicesLocked() {
		s.reconcileService(ctx, svc)
	}
	s.probes = nil

	if s.ticks%eventPruneEvery == 0 {
		if err := s.store.PruneEvents(eventPruneAge, eventPruneRows); err != nil {
			s.logf("outpost: prune events: %v", err)
		}
	}
}

// retireLocked stops a service whose manifest is gone. Its row and events
// stay, marked removed, so the history is still there to read (#32); `outpost
// remove` clears them, and restoring the manifest brings the service back.
// Reconcile-loop goroutine only.
func (s *Supervisor) retireLocked(ctx context.Context, name string, st *svcState) {
	s.stopUndeclaredLocked(ctx, name, st)
	s.event(name, state.EventStopped, "removed from manifest")
	if err := s.store.UpsertService(state.ServiceRecord{
		Name:          name,
		Runtime:       st.svc.Runtime,
		Status:        StatusRemoved,
		RestartCount:  st.restartCount,
		LastStartedAt: st.startedAt,
	}); err != nil {
		s.logf("outpost: %v", err)
	}
}

// stopUndeclaredLocked stops a service whose manifest is gone and stops
// tracking it, leaving its row and events for the caller to keep or clear.
// Reconcile-loop goroutine only.
func (s *Supervisor) stopUndeclaredLocked(ctx context.Context, name string, st *svcState) {
	s.logf("stopping %s: removed from manifest", name)
	if st.handle != nil {
		if err := s.runtimeFor(st.svc).Stop(ctx, st.handle); err != nil {
			s.recordError(name, fmt.Sprintf("stop: %v", err))
		}
	}
	if s.tailnet != nil {
		_ = s.tailnet.Detach(ctx, name)
	}
	delete(s.tracked, name)
}

// reconcileService drives one service toward its desired state.
func (s *Supervisor) reconcileService(ctx context.Context, svc *manifest.Service) {
	st, ok := s.tracked[svc.Name]
	if !ok {
		st = &svcState{svc: svc, status: runtime.StatusStopped, backoff: initialBackoff(svc)}
		// Adopt the restart epoch already in the store so a request made before
		// this supervisor existed is not replayed as a spurious restart.
		if rec, found, err := s.store.GetService(svc.Name); err == nil && found {
			st.lastRestartEpoch = rec.RestartEpoch
			st.restartCount = rec.RestartCount
			st.halted = rec.Halted
		}
		s.tracked[svc.Name] = st
	}
	// spec §6.1: a service whose declaration changed must be restarted, or it
	// keeps running the old configuration indefinitely. Policy-only edits
	// (restart_policy, backoff, healthcheck path) take effect without one.
	if st.handle != nil && execFingerprint(st.svc) != execFingerprint(svc) {
		s.logf("%s: manifest changed, restarting", svc.Name)
		s.event(svc.Name, state.EventRestarted, "manifest changed")
		s.stopInstance(ctx, st, "manifest changed")
		st.backoff = initialBackoff(svc)
		st.nextAttempt = time.Time{}
		st.healthFails = 0
	}
	st.svc = svc

	rec, _, err := s.store.GetService(svc.Name)
	if err != nil {
		// The zero-valued record would read as desired=running, halted=false,
		// silently resurrecting a service the user stopped. Leave the previous
		// intent in force and try again next tick.
		s.logf("outpost: read state for %s: %v (leaving current intent in force)", svc.Name, err)
		return
	}
	desired := rec.Desired
	if desired == "" {
		desired = state.DesiredRunning
	}
	// The store is authoritative for the halt: `outpost start` clears it there.
	st.halted = rec.Halted

	// An explicit restart request takes precedence over everything else.
	if rec.RestartEpoch > st.lastRestartEpoch {
		st.lastRestartEpoch = rec.RestartEpoch
		s.stopInstance(ctx, st, "restart requested")
		st.backoff = initialBackoff(svc)
		st.nextAttempt = time.Time{}
		st.healthFails = 0
		st.halted = false
		s.event(svc.Name, state.EventRestarted, "restart requested")
		desired = state.DesiredRunning
	}

	if desired == state.DesiredStopped {
		if st.handle != nil {
			s.stopInstance(ctx, st, "stopped by request")
			s.event(svc.Name, state.EventStopped, "stopped by request")
		}
		st.status = runtime.StatusStopped
		s.persist(st)
		return
	}

	// Reap an instance that has exited since the last tick.
	if st.handle != nil && exited(st.handle) {
		s.handleExit(st)
	}

	if st.handle == nil {
		// Halted: the restart policy said stop trying. Only an explicit start
		// or restart from the CLI resumes it.
		if st.halted {
			s.persist(st)
			return
		}
		s.maybeStart(ctx, st)
		return
	}

	s.checkHealth(ctx, st)
}

// handleExit applies the restart policy to a service that stopped on its own.
func (s *Supervisor) handleExit(st *svcState) {
	if st.reaped {
		return
	}
	st.reaped = true

	code := st.handle.ExitCode()
	clean := st.handle.CleanStop()
	st.handle = nil
	st.healthFails = 0

	// The service is gone; take its tailnet node down so callers get a clean
	// failure rather than a proxy to nothing.
	if s.tailnet != nil {
		if err := s.tailnet.Detach(context.Background(), st.svc.Name); err != nil {
			s.recordError(st.svc.Name, fmt.Sprintf("tailnet detach: %v", err))
		}
	}

	if clean {
		// We asked for this exit (a restart); start again immediately.
		st.status = runtime.StatusStopped
		st.nextAttempt = time.Time{}
		s.persist(st)
		return
	}

	crashed := code != 0
	msg := fmt.Sprintf("exited with code %d", code)
	s.event(st.svc.Name, state.EventExited, msg)
	s.logf("%s %s", st.svc.Name, msg)

	if !s.shouldRestart(st.svc, crashed) {
		if crashed {
			st.status = runtime.StatusFailed
		} else {
			st.status = runtime.StatusStopped
		}
		s.halt(st, fmt.Sprintf("not restarting: restart_policy = %q", st.svc.RestartPolicy))
		s.persist(st)
		return
	}

	st.status = runtime.StatusFailed
	s.scheduleRestart(st)
	s.persist(st)
}

// shouldRestart applies restart_policy to an exit (§5).
func (s *Supervisor) shouldRestart(svc *manifest.Service, crashed bool) bool {
	switch svc.RestartPolicy {
	case "never":
		return false
	case "always":
		return true
	default: // "on-failure"
		return crashed
	}
}

// scheduleRestart arms the next start attempt and grows the backoff.
func (s *Supervisor) scheduleRestart(st *svcState) {
	if st.backoff <= 0 {
		st.backoff = initialBackoff(st.svc)
	}
	st.nextAttempt = s.now().Add(st.backoff)
	st.restartCount++

	next := st.backoff * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	st.backoff = next
}

// maybeStart starts a service whose backoff has elapsed.
func (s *Supervisor) maybeStart(ctx context.Context, st *svcState) {
	if !st.nextAttempt.IsZero() && s.now().Before(st.nextAttempt) {
		s.persist(st)
		return
	}

	h, err := s.runtimeFor(st.svc).Start(ctx, st.svc)
	if err != nil {
		st.status = runtime.StatusFailed
		s.recordError(st.svc.Name, fmt.Sprintf("start: %v", err))
		s.scheduleRestart(st)
		s.persist(st)
		return
	}

	st.handle = h
	st.reaped = false
	st.status = runtime.StatusRunning
	st.startedAt = s.now()
	st.nextAttempt = time.Time{}
	st.healthFails = 0
	st.everHealthy = false

	s.event(st.svc.Name, state.EventStarted, fmt.Sprintf("started (id %s)", h.ID()))
	s.logf("started %s (id %s)", st.svc.Name, h.ID())

	// Join the tailnet once the service is up. A failure here leaves the
	// service running and reachable on loopback rather than tearing it down:
	// losing tailnet exposure is not a reason to take a working service off.
	s.attachToTailnet(ctx, st.svc)
	s.persist(st)
}

// attachToTailnet asks the router to put a service on the tailnet. The router
// returns immediately and retries in the background, so this is safe to call
// from the reconcile loop on every tick.
func (s *Supervisor) attachToTailnet(ctx context.Context, svc *manifest.Service) {
	if s.tailnet == nil {
		return
	}
	if err := s.tailnet.Attach(ctx, svc); err != nil {
		s.recordError(svc.Name, fmt.Sprintf("tailnet: %v", err))
	}
}

// checkHealth probes a running service and restarts it once it has failed the
// healthcheck HealthFailThreshold times in a row (§6.2).
func (s *Supervisor) checkHealth(ctx context.Context, st *svcState) {
	r := s.probe(ctx, st)
	ok, err := r.ok, r.err
	if ok {
		if st.healthFails > 0 {
			s.event(st.svc.Name, state.EventHealthOK, "healthcheck recovered")
		}
		st.healthFails = 0
		st.everHealthy = true
		st.status = runtime.StatusRunning

		// Re-assert tailnet membership for a running service. Attach is a
		// cheap no-op once a node is up, and without this an attach that failed
		// at start time would never be retried at the supervisor level.
		s.attachToTailnet(ctx, st.svc)
		// A service that has stayed up is no longer in a crash loop.
		if !st.startedAt.IsZero() && s.now().Sub(st.startedAt) >= stableAfter {
			st.backoff = initialBackoff(st.svc)
		}
		s.persist(st)
		return
	}

	// Inside the startup grace window a failing healthcheck is expected: the
	// service may still be binding its port, running migrations, or warming up.
	// The window ends at the first success, or when it expires.
	if !st.everHealthy && s.withinStartGrace(st) {
		st.status = runtime.StatusStarting
		s.persist(st)
		return
	}

	// A probe that failed while the daemon could not run says nothing about
	// the service: every probe in flight times out together, and counting
	// them restarted healthy services in bulk (#29).
	if stall, stalled := s.heartbeat.stalledSince(r.started); stalled {
		s.logf("%s: healthcheck failed while outpost itself was stalled for %s; not counting it",
			st.svc.Name, stall.Round(100*time.Millisecond))
		s.persist(st)
		return
	}

	st.healthFails++
	st.status = runtime.StatusUnhealthy
	reason := "healthcheck failed"
	if err != nil {
		reason = err.Error()
	}
	s.event(st.svc.Name, state.EventHealthFail,
		fmt.Sprintf("%s (%d/%d)", reason, st.healthFails, s.healthFailThreshold))

	if st.healthFails < s.healthFailThreshold {
		s.persist(st)
		return
	}

	s.logf("%s: unhealthy %d times, restarting", st.svc.Name, st.healthFails)
	s.stopInstance(ctx, st, "unhealthy")
	st.healthFails = 0
	st.status = runtime.StatusUnhealthy
	if s.shouldRestart(st.svc, true) {
		s.scheduleRestart(st)
	} else {
		st.status = runtime.StatusFailed
		s.halt(st, fmt.Sprintf("not restarting: restart_policy = %q", st.svc.RestartPolicy))
	}
	s.persist(st)
}

// stopInstance stops the running instance, if any, and clears the handle.
func (s *Supervisor) stopInstance(ctx context.Context, st *svcState, reason string) {
	if st.handle == nil {
		return
	}
	if s.tailnet != nil {
		if err := s.tailnet.Detach(ctx, st.svc.Name); err != nil {
			s.recordError(st.svc.Name, fmt.Sprintf("tailnet detach: %v", err))
		}
	}
	if err := s.runtimeFor(st.svc).Stop(ctx, st.handle); err != nil {
		s.recordError(st.svc.Name, fmt.Sprintf("stop: %v", err))
	}
	s.logf("stopped %s: %s", st.svc.Name, reason)
	st.handle = nil
	st.reaped = false
	st.startedAt = time.Time{}
}

// Shutdown stops every running service, so nothing is orphaned holding a port.
//
// The daemon does not reach it this way: Run watches its context and calls
// shutdownLocked directly when a signal cancels it. This is the entry point
// for a caller outside the loop, which today means the tests.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	if s.loopRunning.Load() {
		_, err := s.submit(ctx, func(s *Supervisor) (any, error) {
			return nil, s.shutdownLocked(ctx)
		})
		return err
	}
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	return s.shutdownLocked(ctx)
}

// shutdownLocked is Shutdown's body. Reconcile-loop goroutine only.
func (s *Supervisor) shutdownLocked(ctx context.Context) error {
	var failures []string
	for _, name := range sortedKeys(s.tracked) {
		st := s.tracked[name]
		if st.handle == nil {
			continue
		}
		if s.tailnet != nil {
			_ = s.tailnet.Detach(ctx, name)
		}
		if err := s.runtimeFor(st.svc).Stop(ctx, st.handle); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
		st.handle = nil
		st.status = runtime.StatusStopped
		s.event(name, state.EventStopped, "supervisor shutting down")
		s.persist(st)
	}
	if len(failures) > 0 {
		return fmt.Errorf("shutdown: %s", strings.Join(failures, "; "))
	}
	return nil
}

// runtimeFor selects the runtime backing a service. Outpost supervises native
// processes only, so there is exactly one.
func (s *Supervisor) runtimeFor(svc *manifest.Service) runtime.Runtime {
	return s.procRT
}

// persist writes observed state back to the store, which is what `outpost list`
// and `outpost status` read.
func (s *Supervisor) persist(st *svcState) {
	rec := state.ServiceRecord{
		Name:          st.svc.Name,
		Runtime:       st.svc.Runtime,
		Status:        string(st.status),
		RestartCount:  st.restartCount,
		LastStartedAt: st.startedAt,
	}
	if st.handle != nil {
		rec.InstanceID = st.handle.ID()
	}
	if err := s.store.UpsertService(rec); err != nil {
		s.logf("outpost: persist %s: %v", st.svc.Name, err)
	}
}

// halt records that the supervisor has stopped trying to keep a service up.
func (s *Supervisor) halt(st *svcState, reason string) {
	st.halted = true
	st.nextAttempt = time.Time{}
	if err := s.store.SetHalted(st.svc.Name, true); err != nil {
		s.logf("outpost: %v", err)
	}
	s.event(st.svc.Name, state.EventError, reason)
	s.logf("%s: %s", st.svc.Name, reason)
}

// event appends to the activity feed. Recording is best-effort by design: the
// feed is what `outpost logs --events` and the TUI read, and losing a row must
// never interrupt supervision. A failed write is logged rather than swallowed.
func (s *Supervisor) event(service, kind, message string) {
	if err := s.store.AppendEvent(service, kind, message); err != nil {
		s.logf("outpost: record %s event for %q: %v", kind, service, err)
	}
}

func (s *Supervisor) recordError(service, msg string) {
	s.logf("%s: %s", service, msg)
	s.event(service, state.EventError, msg)
}

// withinStartGrace reports whether a freshly started service is still inside
// its start_grace_seconds window.
func (s *Supervisor) withinStartGrace(st *svcState) bool {
	if st.startedAt.IsZero() {
		return false
	}
	grace := time.Duration(st.svc.StartGraceSeconds) * time.Second
	if grace <= 0 {
		return false
	}
	return s.now().Sub(st.startedAt) < grace
}

// execFingerprint covers the fields that determine what actually runs, so a
// change to any of them forces a restart. Fields that only affect supervision
// policy or probing are deliberately excluded: changing restart_policy should
// not bounce a healthy service.
func execFingerprint(svc *manifest.Service) string {
	if svc == nil {
		return ""
	}
	h := sha256.New()
	fmt.Fprintf(h, "runtime=%s\x00binary=%s\x00port=%d\x00storage=%s\x00tailscale=%s\x00",
		svc.Runtime, svc.Binary, svc.Port, svc.ExpandStorage(), svc.TailscaleName)
	for _, arg := range svc.Args {
		fmt.Fprintf(h, "arg=%s\x00", arg)
	}
	// Map iteration order is random, so sort before hashing.
	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env=%s=%s\x00", k, svc.Env[k])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func initialBackoff(svc *manifest.Service) time.Duration {
	if svc.BackoffSeconds == 0 {
		return 5 * time.Second
	}
	return time.Duration(svc.BackoffSeconds) * time.Second
}

// exited reports whether a handle's instance has already terminated.
func exited(h runtime.Handle) bool {
	select {
	case <-h.Done():
		return true
	default:
		return false
	}
}

func sortedKeys(m map[string]*svcState) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func indentErrors(errs []error) string {
	var b strings.Builder
	for i, err := range errs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  - %v", err)
	}
	return b.String()
}
