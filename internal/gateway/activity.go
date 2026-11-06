package gateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ActivityService is the name gateway events are recorded under in the
// activity feed. A service can be called anything, but nothing would be
// called this, so the gateway's events neither mix with a service's nor go
// when one is removed. `outpost gateway activity` reads them.
const ActivityService = "@gateway"

// Activity-feed kinds the gateway records.
const (
	EventSignIn     = "sign-in"
	EventSignInFail = "sign-in-fail"
	// EventPasswordFail is a wrong current password at the account page:
	// someone already signed in, so not a failed sign-in.
	EventPasswordFail = "password-fail"
	EventSignOut      = "sign-out"
	EventDenied       = "denied"
	EventPassword     = "password"
	EventSetup        = "setup"
	EventUpdate       = "update"
	// EventControl is an app started, stopped, restarted or backed up from
	// the portal.
	EventControl = "control"
	// Access tokens: the CLI's create and revoke, and the gateway's refusals.
	EventTokenCreate = "token-create"
	EventTokenRevoke = "token-revoke"
	EventTokenFail   = "token-fail"
	// EventSecret is a secret created, rotated, bound, unbound or removed,
	// never with its value.
	EventSecret = "secret"
)

// Events is where the gateway's activity goes: the supervisor's feed in the
// daemon. Writes reach it through the supervisor's own command queue, which
// keeps its loop single-threaded.
type Events interface {
	RecordEvent(ctx context.Context, service, kind, message string) error
}

const (
	// activityQueue bounds events waiting to be written. A request never
	// waits on the feed; past this many, events are dropped and logged.
	activityQueue = 256
	// activityWindow is how long repeats of a failure are counted rather
	// than each recorded: the first is recorded at once, the rest as one
	// line when the window closes.
	activityWindow = 10 * time.Minute
	// activityRepeats bounds the repeats being counted, which anyone can
	// grow by guessing names.
	activityRepeats = 1000
	// maxQuoted bounds a client-chosen name in a message.
	maxQuoted = 64
)

type activityEvent struct{ kind, message string }

// activity records what the gateway decides, without making a request wait.
type activity struct {
	sink  Events
	queue chan activityEvent
	logf  func(string, ...any)
	now   func() time.Time

	mu      sync.Mutex
	repeats map[string]*repeat
}

// repeat counts a failure seen again inside its window.
type repeat struct {
	kind, subject string // subject reads "for alice from 192.0.2.1"
	since         time.Time
	count         int // after the first, which was recorded on its own
	throttled     int
}

func newActivity(sink Events, logf func(string, ...any), now func() time.Time) *activity {
	return &activity{
		sink:    sink,
		queue:   make(chan activityEvent, activityQueue),
		logf:    logf,
		now:     now,
		repeats: map[string]*repeat{},
	}
}

// record queues one event. With no sink it does nothing.
func (a *activity) record(kind, format string, v ...any) {
	if a == nil || a.sink == nil {
		return
	}
	ev := activityEvent{kind: kind, message: fmt.Sprintf(format, v...)}
	select {
	case a.queue <- ev:
	default:
		a.logf("gateway: activity feed is backed up; dropped %s: %s", ev.kind, ev.message)
	}
}

// failed records a failure, counting repeats of the same one (kind and
// subject) inside the window instead of recording each. throttled marks an
// attempt turned away before its password was checked.
func (a *activity) failed(kind, subject string, throttled bool) {
	if a == nil || a.sink == nil {
		return
	}
	now := a.now()
	key := kind + " " + subject
	a.mu.Lock()
	rp, ok := a.repeats[key]
	if ok && now.Sub(rp.since) <= activityWindow {
		if throttled {
			rp.throttled++
		} else {
			rp.count++
		}
		a.mu.Unlock()
		return
	}
	var done *repeat
	if ok {
		done = rp
	}
	if !ok && len(a.repeats) >= activityRepeats {
		a.flushLocked(now, false)
	}
	full := !ok && len(a.repeats) >= activityRepeats
	if !full {
		a.repeats[key] = &repeat{kind: kind, subject: subject, since: now}
	}
	a.mu.Unlock()
	if full {
		// A thousand different failures in ten minutes is a flood, and the
		// throttle is already turning it away; recording each would push
		// the services' own history out of the feed.
		a.logf("gateway: too many different failures to record; skipped %s %s", failureWords(kind), subject)
		return
	}

	if done != nil {
		a.summarise(done)
	}
	what := failureWords(kind)
	if throttled {
		what += " (throttled)"
	}
	a.record(kind, "%s %s", what, subject)
}

// settle ends the count for subject after a success, so what came before
// it is recorded now rather than when the window closes.
func (a *activity) settle(kind, subject string) {
	if a == nil || a.sink == nil {
		return
	}
	a.mu.Lock()
	rp, ok := a.repeats[kind+" "+subject]
	delete(a.repeats, kind+" "+subject)
	a.mu.Unlock()
	if ok {
		a.summarise(rp)
	}
}

// flushLocked records and drops counts whose window has closed, or all of
// them when everything is true. a.mu is held.
func (a *activity) flushLocked(now time.Time, everything bool) {
	for k, rp := range a.repeats {
		if everything || now.Sub(rp.since) > activityWindow {
			delete(a.repeats, k)
			// Queued, not written, so holding the lock is fine.
			a.summarise(rp)
		}
	}
}

func (a *activity) summarise(rp *repeat) {
	if rp.count+rp.throttled > 0 {
		a.record(rp.kind, "%s", summary(rp))
	}
}

// summary is the one line for a window's repeats.
func summary(rp *repeat) string {
	since := " since " + rp.since.Local().Format("15:04")
	if rp.kind == EventDenied {
		return fmt.Sprintf("access denied %d more %s %s%s", rp.count, plural(rp.count, "time"), rp.subject, since)
	}
	var parts []string
	if rp.count > 0 {
		parts = append(parts, fmt.Sprintf("%d more %s", rp.count, plural(rp.count, failureWords(rp.kind))))
	}
	if rp.throttled > 0 {
		parts = append(parts, fmt.Sprintf("%d throttled", rp.throttled))
	}
	return strings.Join(parts, " and ") + " " + rp.subject + since
}

func failureWords(kind string) string {
	switch kind {
	case EventDenied:
		return "access denied"
	case EventPasswordFail:
		return "wrong current password"
	case EventTokenFail:
		return "rejected access token"
	}
	return "failed sign-in"
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// run writes queued events to the sink and closes counting windows, until
// ctx ends.
func (a *activity) run(ctx context.Context) {
	if a == nil || a.sink == nil {
		return
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			a.drain()
			return
		case <-tick.C:
			a.mu.Lock()
			a.flushLocked(a.now(), false)
			a.mu.Unlock()
		case ev := <-a.queue:
			// A short bound of its own: the daemon's context is ending
			// anyway when this one does.
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := a.sink.RecordEvent(wctx, ActivityService, ev.kind, ev.message); err != nil {
				a.logf("gateway: record %s event: %v", ev.kind, err)
			}
			cancel()
		}
	}
}

// drain writes what is still queued or counted as the gateway stops, so a
// restart in the middle of a burst of failures doesn't lose its record.
func (a *activity) drain() {
	a.mu.Lock()
	a.flushLocked(a.now(), true)
	a.mu.Unlock()
	for {
		select {
		case ev := <-a.queue:
			wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := a.sink.RecordEvent(wctx, ActivityService, ev.kind, ev.message); err != nil {
				a.logf("gateway: record %s event: %v", ev.kind, err)
			}
			cancel()
		default:
			return
		}
	}
}

// quoted shows a name a client chose, such as one typed at the sign-in
// form, safely: quoted, with control characters escaped, and bounded.
func quoted(s string) string {
	if r := []rune(s); len(r) > maxQuoted {
		s = string(r[:maxQuoted]) + "…"
	}
	return fmt.Sprintf("%q", s)
}
