package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/backup"
)

// Control is what the gateway needs in order to start, stop, restart and back
// up an app from the portal. The daemon passes its supervisor, so the portal's
// buttons go through the same public API as `outpost stop` and the rest.
type Control interface {
	StartService(ctx context.Context, name string) error
	StopService(ctx context.Context, name string) error
	RestartService(ctx context.Context, name string) error
	Backup(ctx context.Context, name string) (backup.Archive, error)
}

// What the portal can do to an app.
const (
	actStart   = "start"
	actStop    = "stop"
	actRestart = "restart"
	actBackup  = "backup"
)

const (
	// controlTimeout bounds a start, stop or restart. The supervisor answers
	// once it has reconciled, which waits on a stop's grace period at most,
	// but the request queues behind whatever the loop is doing, and a backup
	// holds it for as long as archiving takes. A shorter bound would report
	// as failed a stop that the loop goes on to carry out.
	controlTimeout = backupTimeout + time.Minute
	// backupTimeout bounds a backup, which archives however much storage an
	// app has.
	backupTimeout = 30 * time.Minute
	// alertFor is how long a failure stays an alert. The catalog reloads
	// while anything is busy, and a screen reader would otherwise announce
	// the same old failure on every reload.
	alertFor = 4 * time.Second
	// resultShown is how long the outcome of an action stays on its row: long
	// enough to come back and read, short enough not to become furniture.
	resultShown = 5 * time.Minute
)

// verbs are the words for each action: what it's called, and what it's
// called while under way and once done.
var verbs = map[string]struct{ do, doing, done, did string }{
	actStart:   {"start", "Starting…", "Started", "started"},
	actStop:    {"stop", "Stopping…", "Stopped", "stopped"},
	actRestart: {"restart", "Restarting…", "Restarted", "restarted"},
	actBackup:  {"back up", "Backing up…", "Backed up", "backed up"},
}

// opResult is how an action from the portal ended.
type opResult struct {
	Action string
	By     string
	Err    string
	At     time.Time
}

// appOps is what's being done to apps from the portal, and how it last went.
// Like the update state it lives in memory: the supervisor has the real
// answer, and this is only so the one who asked can see it.
type appOps struct {
	mu      sync.Mutex
	busy    map[string]string // service to the action under way
	results map[string]opResult
}

// begin claims a service for an action, and reports false if one is already
// under way, so a double tap is one action rather than two.
func (o *appOps) begin(service, action string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.busy[service]; ok {
		return false
	}
	if o.busy == nil {
		o.busy = map[string]string{}
	}
	o.busy[service] = action
	delete(o.results, service)
	return true
}

func (o *appOps) finish(service string, r opResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.busy, service)
	if o.results == nil {
		o.results = map[string]opResult{}
	}
	o.results[service] = r
}

// anyBusy names an app with an action under way, and the action, or "" for
// none.
func (o *appOps) anyBusy() (service, action string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for s, a := range o.busy {
		return s, a
	}
	return "", ""
}

func (o *appOps) state(service string) (busy string, res opResult, done bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	busy = o.busy[service]
	res, done = o.results[service]
	return busy, res, done
}

// appAction is one item in an app's Manage menu. Every one but Start asks
// first, since each takes the app away for a while.
type appAction struct {
	Action  string
	Path    string // where the form goes: /apps, or /updates for an update
	Verb    string // "Stop"; the app's title follows it for a screen reader
	Suffix  string // what follows the title, such as " to v2.0.0"
	Confirm string // the consequence, asked before acting; empty acts at once
	Danger  bool
}

// offered lists what can be done to an app in the state it's in. The handler
// checks against the same list, so a stale page can't ask for something the
// app can no longer do.
func (g *Gateway) offered(rt route) []string {
	state, _, _ := g.describe(rt)
	switch {
	case state == stateUp:
		return []string{actRestart, actBackup, actStop}
	case state == stateStarting:
		return []string{actRestart, actStop}
	case rt.Desired == "stopped":
		return []string{actStart, actBackup}
	case rt.Halted:
		// Start clears the halt, which is all a halted app needs, so
		// Restart beside it would only be a second name for the same thing.
		return []string{actStart, actStop}
	default:
		// Still meant to be running, so Start would change nothing; Restart
		// is what brings back one that's crashed or not answering.
		return []string{actRestart, actStop}
	}
}

func (g *Gateway) offers(rt route, action string) bool {
	return slices.Contains(g.offered(rt), action)
}

// menu is an app's Manage menu, in the order offered, with an update last
// when one is waiting.
func (g *Gateway) menu(rt route, title string) []appAction {
	var acts []appAction
	for _, a := range g.offered(rt) {
		switch a {
		case actStart:
			acts = append(acts, appAction{Action: a, Path: "/apps", Verb: "Start"})
		case actRestart:
			acts = append(acts, appAction{Action: a, Path: "/apps", Verb: "Restart",
				Confirm: "Restart " + title + "? It's briefly unavailable while it stops and starts again."})
		case actBackup:
			what := title + " is unavailable while its storage is archived, then starts again."
			if rt.Desired == "stopped" {
				what = title + " stays stopped while its storage is archived."
			}
			acts = append(acts, appAction{Action: a, Path: "/apps", Verb: "Back up", Suffix: " now",
				Confirm: "Back up " + title + " now? " + what})
		case actStop:
			acts = append(acts, appAction{Action: a, Path: "/apps", Verb: "Stop", Danger: true,
				Confirm: "Stop " + title + "? It stays stopped until someone starts it."})
		}
	}
	if latest, ok := g.waitingFor(rt); ok {
		acts = append(acts, appAction{Action: "update", Path: "/updates", Verb: "Update", Suffix: " to " + latest,
			Confirm: "Update " + title + " to " + latest + "? Outpost backs it up, installs " + latest +
				" and restarts it, and puts the old release back if the new one isn't healthy."})
	}
	return acts
}

// manage gives each station an admin's menu, or what's under way instead, and
// reports whether anything is, so the page can refresh until it's done.
func (g *Gateway) manage(sts []station) (busy bool) {
	now := g.now()
	for i := range sts {
		st := &sts[i]
		rt, ok := g.routes.get(st.Name)
		if !ok {
			continue
		}
		doing, res, done := g.ops.state(st.Name)
		switch {
		case doing != "":
			st.Busy = verbs[doing].doing
			st.Label = strings.TrimSuffix(st.Busy, "…")
		case g.updateBusy(st.Name):
			st.Busy, st.Label = "Updating…", "Updating"
		default:
			st.Acts = g.menu(rt, st.Title)
			// Start asks nothing and is what a stopped app is waiting for, so
			// it doesn't wait behind the menu with the rest.
			if st.State != stateUp && len(st.Acts) > 0 && st.Acts[0].Action == actStart {
				st.Quick, st.Acts = &st.Acts[0], st.Acts[1:]
			}
		}
		if st.Busy != "" {
			busy = true
			continue
		}
		if done && now.Sub(res.At) < resultShown {
			when := agoWords(res.At, now)
			if res.Err != "" {
				st.Done = fmt.Sprintf("Couldn't %s when %s asked %s: %s", verbs[res.Action].do, res.By, when, res.Err)
				st.Failed = true
				st.Alert = now.Sub(res.At) < alertFor
			} else {
				st.Done = fmt.Sprintf("%s by %s · %s", verbs[res.Action].done, res.By, when)
			}
		}
	}
	return busy
}

func agoWords(at, now time.Time) string {
	if d := now.Sub(at); d >= time.Minute {
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	}
	return "just now"
}

func (g *Gateway) appsSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.adminUser(w, r, "/", "Only an admin can start, stop or back up apps.")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/", "Back to apps")
		return
	}
	// Only a service in the route table. This is also what keeps the name
	// from being anything the supervisor could read as a flag or a path.
	name := r.PostForm.Get("service")
	rt, ok := g.routes.get(name)
	if !ok {
		g.message(w, r, http.StatusNotFound, "No such app", "There's no app by that name.", "/", "Back to apps")
		return
	}
	title := g.titleOf(name)
	action := r.PostForm.Get("action")
	v, known := verbs[action]
	if !known {
		g.message(w, r, http.StatusBadRequest, "That didn't go through",
			"That isn't something Outpost can do to an app.", "/", "Back to apps")
		return
	}
	if !g.offers(rt, action) {
		_, label, _ := g.describe(rt)
		g.message(w, r, http.StatusBadRequest, "Not now",
			fmt.Sprintf("%s can't %s while it's %s.", title, v.do, strings.ToLower(label)), "/#app-"+name, "Back to apps")
		return
	}
	// The update check and the claim happen under one lock, which the update
	// page takes too, so an update and an action asked for at the same moment
	// can't both get past each other's check.
	g.claim.Lock()
	if g.updateBusy(name) {
		g.claim.Unlock()
		g.message(w, r, http.StatusConflict, "Being updated",
			title+" is being updated. Try again once the update is done.", "/#app-"+name, "Back to apps")
		return
	}
	if g.selfBusy() {
		g.claim.Unlock()
		g.message(w, r, http.StatusConflict, "Outpost is updating",
			"Outpost is being updated, and its restart would cut this short. Try again once it's back.", "/#app-"+name, "Back to apps")
		return
	}
	began := g.ops.begin(name, action)
	g.claim.Unlock()
	if began {
		ip := g.clientIP(r)
		g.logf("gateway: %s asked to %s %s", u.Username, v.do, name)
		if action == actBackup {
			// Backups take a while, so the start is recorded too, as an
			// update's is; the rest are recorded once the supervisor answers.
			g.activity.record(EventControl, "%s started a backup of %s from %s", u.Username, name, ip)
		}
		go g.runAction(name, action, u.Username, ip)
	}
	// Land on the row's note, which says what's under way and then how it
	// went, so the outcome is where the eye and a screen reader arrive.
	http.Redirect(w, r, "/#note-"+name, http.StatusSeeOther)
}

// runAction carries out one action to its end, on its own goroutine, where a
// panic would otherwise take the daemon and every service with it.
func (g *Gateway) runAction(service, action, by, ip string) {
	var err error
	defer func() {
		if p := recover(); p != nil {
			g.logf("gateway: %s %s: panic: %v", verbs[action].do, service, p)
			err = errors.New("it stopped unexpectedly; see the daemon's log")
		}
		g.actionDone(service, action, by, ip, err)
	}()
	err = g.act(service, action)
}

// act asks the supervisor for one action.
func (g *Gateway) act(service, action string) error {
	timeout := controlTimeout
	if action == actBackup {
		timeout = backupTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch action {
	case actStart:
		return g.control.StartService(ctx, service)
	case actStop:
		return g.control.StopService(ctx, service)
	case actRestart:
		return g.control.RestartService(ctx, service)
	case actBackup:
		a, err := g.control.Backup(ctx, service)
		if err == nil {
			g.logf("gateway: backed up %s to %s", service, a.Path)
		}
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func (g *Gateway) actionDone(service, action, by, ip string, err error) {
	v := verbs[action]
	res := opResult{Action: action, By: by}
	switch {
	case err != nil:
		res.Err = err.Error()
		g.logf("gateway: %s couldn't %s %s: %v", by, v.do, service, err)
		g.activity.record(EventControl, "%s couldn't %s %s from %s: %v", by, v.do, service, ip, err)
	case action == actBackup:
		g.activity.record(EventControl, "backup of %s finished", service)
	default:
		g.activity.record(EventControl, "%s %s %s from %s", by, v.did, service, ip)
	}
	// Show the new state as soon as the row says it's done, not a tick later.
	// The service stays marked busy until this is over.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.refreshRoutes(ctx)
	res.At = g.now()
	g.ops.finish(service, res)
}
