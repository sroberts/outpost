package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// This file is the Supervisor's public API — the one code path the CLI, the
// TUI and any future integration go through (§12).
//
// The Supervisor's state is single-threaded by design, so every method here
// hands a closure to the reconcile loop rather than touching state directly.
// That keeps the daemon's socket handlers, which run on their own goroutines,
// from racing the loop.

// ServiceStatus is the observed state of one service, as reported to clients.
type ServiceStatus struct {
	Name          string    `json:"name"`
	Runtime       string    `json:"runtime"`
	Status        string    `json:"status"`
	Desired       string    `json:"desired"`
	Halted        bool      `json:"halted"`
	InstanceID    string    `json:"instance_id"`
	RestartCount  int       `json:"restart_count"`
	LastStartedAt time.Time `json:"last_started_at"`
	Port          int       `json:"port"`
	TailscaleName string    `json:"tailscale_name"`
	Healthcheck   string    `json:"healthcheck"`
	Storage       string    `json:"storage"`
	RestartPolicy string    `json:"restart_policy"`
	Backoff       int       `json:"backoff_seconds"`
	// NextAttempt is set while a service is waiting out its restart backoff.
	NextAttempt time.Time `json:"next_attempt,omitempty"`
	// Tailnet is the observed state of the service's node, where
	// TailscaleName is only what the manifest asks for. Nil for a service
	// with no tailscale_name, and from a daemon too old to say.
	Tailnet *TailnetStatus `json:"tailnet,omitempty"`

	// Catalog fields from the manifest, for the gateway.
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Subdomain   string   `json:"subdomain,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	Hidden      bool     `json:"hidden,omitempty"`
	PublicPaths []string `json:"public_paths,omitempty"`

	// Where the binary came from, for the gateway's update page.
	Source  string `json:"source,omitempty"`
	Release string `json:"release,omitempty"`

	// What the daemon last learned about the latest release, for what's
	// installed now. All empty until a check has answered for that release.
	LatestRelease    string    `json:"latest_release,omitempty"`
	UpdateAvailable  bool      `json:"update_available,omitempty"`
	ReleaseNote      string    `json:"release_note,omitempty"`
	ReleaseCheckedAt time.Time `json:"release_checked_at,omitempty"`
	// ReleaseCheckError is why the most recent check failed. Any answer above
	// is from the check before it.
	ReleaseCheckError string `json:"release_check_error,omitempty"`
}

// ErrNotRunning is returned when the reconcile loop is not draining commands.
var ErrNotRunning = errors.New("supervisor is not running")

// command is a closure for the reconcile loop to run between ticks.
type command struct {
	fn   func(*Supervisor) (any, error)
	resp chan commandResult
}

type commandResult struct {
	value any
	err   error
}

// submit hands fn to the reconcile loop and waits for its result.
//
// With no loop running there is nothing to drain s.cmds — the buffered channel
// would accept the command and the caller would then block until its context
// expired — so run fn inline instead. That keeps every API method usable
// against a supervisor that has not been started, and closes the window in
// serve.go between binding the socket and entering Run.
func (s *Supervisor) submit(ctx context.Context, fn func(*Supervisor) (any, error)) (any, error) {
	if !s.loopRunning.Load() {
		// Re-check under the lock: the loop may have started between the check
		// above and here, in which case the work belongs on the queue.
		s.loopMu.Lock()
		if !s.loopRunning.Load() {
			defer s.loopMu.Unlock()
			return fn(s)
		}
		s.loopMu.Unlock()
	}

	cmd := command{fn: fn, resp: make(chan commandResult, 1)}

	select {
	case s.cmds <- cmd:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopped:
		return nil, ErrNotRunning
	}

	select {
	case res := <-cmd.resp:
		return res.value, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopped:
		return nil, ErrNotRunning
	}
}

// List reports every declared service, plus anything still recorded in the
// state store but no longer declared.
func (s *Supervisor) List(ctx context.Context) ([]ServiceStatus, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		return s.listLocked(), nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]ServiceStatus), nil
}

// listLocked builds the status list. Reconcile-loop goroutine only.
func (s *Supervisor) listLocked() []ServiceStatus {
	var out []ServiceStatus
	declared := map[string]bool{}

	for _, svc := range s.servicesLocked() {
		declared[svc.Name] = true
		out = append(out, s.statusOf(svc.Name))
	}

	// Surface stale rows so a service removed from the manifest but still
	// recorded is visible rather than silently missing.
	records, err := s.store.ListServices()
	if err != nil {
		return out
	}
	for _, rec := range records {
		if rec.Name == "" || declared[rec.Name] {
			continue
		}
		out = append(out, ServiceStatus{
			Name:         rec.Name,
			Runtime:      rec.Runtime,
			Status:       rec.Status,
			Desired:      rec.Desired,
			Halted:       rec.Halted,
			InstanceID:   rec.InstanceID,
			RestartCount: rec.RestartCount,
		})
	}
	return out
}

// Tailnet states. The first three are the node's own; the last two say why
// a service has none.
const (
	TailnetOn         = "on"
	TailnetStarting   = "starting"
	TailnetNeedsLogin = "needs-login"
	// TailnetNone is a service with no node right now: one that isn't
	// running, or that the daemon couldn't put on the tailnet.
	TailnetNone = "none"
	// TailnetOff is Tailscale turned off in config.toml.
	TailnetOff = "off"
)

// TailnetStatus is how a service's Tailscale node is doing.
type TailnetStatus struct {
	State string `json:"state"`
	// Host is the node's DNS name once it is on, which can differ from the
	// tailscale_name asked for when that name was taken.
	Host string `json:"host,omitempty"`
	// LoginURL is where to authorise a node that needs a login.
	LoginURL string `json:"login_url,omitempty"`
	// Error is why the last attempt to bring the node up failed.
	Error string `json:"error,omitempty"`
}

// TailnetSummary says how s is doing on the tailnet, in a few words for a
// table: the name it has there, or what its node is doing. "" when it has
// no node, which tables show as "-".
func (s ServiceStatus) TailnetSummary() string {
	t := s.Tailnet
	if t == nil {
		// No tailscale_name, or a daemon too old to say more.
		return s.TailscaleName
	}
	switch t.State {
	case TailnetOn:
		// The node's own name, which is tailscale_name with -1 appended if
		// that was taken.
		if host, _, _ := strings.Cut(t.Host, "."); host != "" {
			return host
		}
		return s.TailscaleName
	case TailnetNeedsLogin:
		return "needs login"
	case TailnetStarting:
		if t.Error != "" {
			return "retrying"
		}
		return "starting"
	}
	return ""
}

// TailnetDetail says the same at more length, for a detail view.
func (s ServiceStatus) TailnetDetail() string {
	t, name := s.Tailnet, s.TailscaleName
	if t == nil {
		return name
	}
	switch t.State {
	case TailnetOn:
		if t.Host != "" {
			return t.Host
		}
		return name
	case TailnetNeedsLogin:
		return name + " (needs login)"
	case TailnetStarting:
		if t.Error != "" {
			return name + " (starting; last attempt: " + t.Error + ")"
		}
		return name + " (starting)"
	case TailnetOff:
		return name + " (Tailscale is off in config.toml)"
	}
	// No node: the service isn't running, or attaching it failed, which
	// the recent events say.
	return name + " (not on the tailnet)"
}

// Status reports one service's detail.
func (s *Supervisor) Status(ctx context.Context, name string) (ServiceStatus, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		if _, ok := s.desired[name]; !ok {
			if _, found, _ := s.store.GetService(name); !found {
				return nil, fmt.Errorf("no such service %q", name)
			}
		}
		return s.statusOf(name), nil
	})
	if err != nil {
		return ServiceStatus{}, err
	}
	return v.(ServiceStatus), nil
}

// tailnetOf asks the router how svc's node is doing. A router that can't
// say leaves it nil, as for a service with no node to ask about.
func (s *Supervisor) tailnetOf(svc *manifest.Service) *TailnetStatus {
	if svc.TailscaleName == "" {
		return nil
	}
	if s.tailnet == nil {
		return &TailnetStatus{State: TailnetOff}
	}
	r, ok := s.tailnet.(TailnetReporter)
	if !ok {
		return nil
	}
	if ts, ok := r.TailnetStatus(svc.Name); ok {
		return &ts
	}
	return &TailnetStatus{State: TailnetNone}
}

// statusOf merges the manifest, the in-memory supervision state and the store.
// Reconcile-loop goroutine only.
func (s *Supervisor) statusOf(name string) ServiceStatus {
	st := ServiceStatus{Name: name}

	if svc, ok := s.desired[name]; ok {
		st.Runtime = svc.Runtime
		st.Port = svc.Port
		st.TailscaleName = svc.TailscaleName
		st.Tailnet = s.tailnetOf(svc)
		st.Healthcheck = svc.Healthcheck
		st.Storage = svc.ExpandStorage()
		st.RestartPolicy = svc.RestartPolicy
		st.Backoff = int(svc.BackoffSeconds)
		st.Title = svc.Title
		st.Description = svc.Description
		st.Subdomain = svc.Subdomain
		st.Hostname = svc.Hostname
		st.Allow = svc.Allow
		st.Hidden = svc.Hidden
		st.Source = svc.Source
		st.Release = svc.Release
		st.PublicPaths = svc.PublicPaths
		s.addRelease(&st, svc)
	}
	if rec, found, err := s.store.GetService(name); err == nil && found {
		st.Status = rec.Status
		st.Desired = rec.Desired
		st.Halted = rec.Halted
		st.InstanceID = rec.InstanceID
		st.RestartCount = rec.RestartCount
		st.LastStartedAt = rec.LastStartedAt
	}
	if tracked, ok := s.tracked[name]; ok {
		st.NextAttempt = tracked.nextAttempt
	}
	if st.Status == "" {
		st.Status = "unknown"
	}
	if st.Desired == "" {
		st.Desired = "running"
	}
	return st
}

// StartService clears any halt and reconciles the service up.
func (s *Supervisor) StartService(ctx context.Context, name string) error {
	return s.mutate(ctx, name, func(s *Supervisor) error {
		return s.store.SetDesired(name, desiredRunning)
	})
}

// StopService stops a service and records that it should stay stopped.
func (s *Supervisor) StopService(ctx context.Context, name string) error {
	return s.mutate(ctx, name, func(s *Supervisor) error {
		return s.store.SetDesired(name, desiredStopped)
	})
}

// RestartService stops and starts a service.
func (s *Supervisor) RestartService(ctx context.Context, name string) error {
	return s.mutate(ctx, name, func(s *Supervisor) error {
		return s.store.RequestRestart(name)
	})
}

// mutate records intent for a declared service, then reconciles immediately so
// the caller sees the effect rather than waiting out a tick.
func (s *Supervisor) mutate(ctx context.Context, name string, fn func(*Supervisor) error) error {
	_, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		if _, ok := s.desired[name]; !ok {
			return nil, fmt.Errorf("no such service %q", name)
		}
		if err := fn(s); err != nil {
			return nil, err
		}
		s.reconcileService(ctx, s.desired[name])
		return nil, nil
	})
	return err
}

// ApplyNow reloads the manifests and reconciles, reporting the resulting set.
func (s *Supervisor) ApplyNow(ctx context.Context) ([]ServiceStatus, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		if err := s.applyLocked(ctx); err != nil {
			return nil, err
		}
		s.tickLocked(ctx)
		return s.listLocked(), nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]ServiceStatus), nil
}

// Events returns recent activity, newest first. An empty service means all.
func (s *Supervisor) Events(ctx context.Context, service string, limit int) ([]EventRecord, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		events, err := s.store.Events(service, limit)
		if err != nil {
			return nil, err
		}
		out := make([]EventRecord, 0, len(events))
		for _, e := range events {
			out = append(out, EventRecord{Service: e.Service, Kind: e.Kind, Message: e.Message, At: e.At})
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]EventRecord), nil
}

// RecordEvent adds an entry to the activity feed from outside the loop, as
// the gateway does for sign-ins. Like every write here, it runs on the loop,
// and a failed write is logged there rather than returned.
func (s *Supervisor) RecordEvent(ctx context.Context, service, kind, message string) error {
	_, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		s.event(service, kind, message)
		return nil, nil
	})
	return err
}

// EventRecord is one activity-feed entry as reported to clients.
type EventRecord struct {
	Service string    `json:"service"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}
