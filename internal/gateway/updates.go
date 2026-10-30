package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

const (
	// updateCheckEvery is how long a check of the releases stays good before
	// an admin opening the portal starts another. GitHub allows 60 requests an
	// hour without a token, and this spends one per app.
	updateCheckEvery = time.Hour
	// updateCheckTimeout bounds asking about one app.
	updateCheckTimeout = 20 * time.Second
)

// Updates is what the gateway needs in order to update an app. The daemon
// implements it with the same code as `outpost update`, so there is one
// definition of what an update does and how it rolls back.
type Updates interface {
	// Latest reports where a service stands against its source's latest
	// release. It goes over the network.
	Latest(ctx context.Context, service string) (ReleaseCheck, error)
	// Update installs the latest release and restarts the service, putting
	// the old one back if the new one isn't healthy. It can take minutes. The
	// output is what it did, for showing to the admin, whether or not it failed.
	Update(service string) (output string, err error)
}

// ReleaseCheck is what a check found. The daemon makes them whether or not the
// gateway is on, so the type is the supervisor's.
type ReleaseCheck = supervisor.ReleaseCheck

// checked is one app's latest check. Installed is the release it was made
// against: once that changes, the answer is about a release no longer there.
type checked struct {
	ReleaseCheck
	Installed string
	Err       string
}

type updateResult struct {
	Output string
	Failed bool
}

// updateState is what the update page knows between requests. It lives in
// memory: the manifest says what's installed, and the rest is worth no more
// than asking again after a restart.
type updateState struct {
	mu        sync.Mutex
	checking  bool
	checkedAt time.Time
	checks    map[string]checked
	daemonAt  map[string]time.Time // the daemon's latest answer taken into checks
	running   map[string]bool
	results   map[string]updateResult
}

// begin claims a service for an update, and reports false if one is running.
// `outpost update` holds a lock of its own as well, which also stops a CLI
// update running beside this one; this keeps a double tap from being reported
// as a failure.
func (s *updateState) begin(service string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[service] {
		return false
	}
	if s.running == nil {
		s.running = map[string]bool{}
	}
	s.running[service] = true
	delete(s.results, service)
	return true
}

// beginCheck claims the one check there may be at a time.
func (s *updateState) beginCheck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checking {
		return false
	}
	s.checking = true
	return true
}

// stale reports whether a check is due: the last is old, or it says nothing
// about what's installed now, because an app is new or was updated from the
// CLI. Each check answers for what's installed, so the second can't repeat.
func (s *updateState) stale(now time.Time, routes []route) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checking {
		return false
	}
	if now.Sub(s.checkedAt) >= updateCheckEvery {
		return true
	}
	for _, rt := range routes {
		if c, ok := s.checks[rt.Name]; rt.Source != "" && !s.running[rt.Name] && (!ok || c.Installed != rt.Release) {
			return true
		}
	}
	return false
}

func (s *updateState) record(service string, c checked) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checks == nil {
		s.checks = map[string]checked{}
	}
	s.checks[service] = c
}

func (s *updateState) finish(service string, r updateResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, service)
	if s.results == nil {
		s.results = map[string]updateResult{}
	}
	s.results[service] = r
}

// takeDaemonChecks takes in what the daemon's own checks found, so a release
// it noticed shows up here without waiting for a page to ask. Only an answer
// not seen before counts: the daemon keeps its last good answer through a
// failed check, and that mustn't paper over the failure the page is showing.
func (s *updateState) takeDaemonChecks(list []supervisor.ServiceStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range list {
		if st.ReleaseCheckedAt.IsZero() || st.ReleaseCheckedAt.Equal(s.daemonAt[st.Name]) {
			continue
		}
		if s.daemonAt == nil {
			s.daemonAt = map[string]time.Time{}
		}
		if s.checks == nil {
			s.checks = map[string]checked{}
		}
		s.daemonAt[st.Name] = st.ReleaseCheckedAt
		s.checks[st.Name] = checked{
			ReleaseCheck: ReleaseCheck{Latest: st.LatestRelease, Newer: st.UpdateAvailable, Note: st.ReleaseNote},
			Installed:    st.Release,
		}
	}
}

// startCheck asks about every app from a release, in the background. Pages
// never wait on GitHub.
func (g *Gateway) startCheck() {
	if !g.updateState.beginCheck() {
		return
	}
	go func() {
		defer func() {
			g.updateState.mu.Lock()
			g.updateState.checking = false
			g.updateState.checkedAt = g.now()
			g.updateState.mu.Unlock()
		}()
		for _, rt := range g.routes.all() {
			if rt.Source != "" {
				g.checkOne(rt.Name, rt.Release)
			}
		}
	}()
}

// checkOne records where a service stands. It runs on its own goroutine, and
// a panic there would otherwise take the daemon and every service with it.
func (g *Gateway) checkOne(service, installed string) {
	c := checked{Installed: installed}
	defer func() {
		if p := recover(); p != nil {
			g.logf("gateway: checking %s for updates: panic: %v", service, p)
			c.Err = "The check failed unexpectedly; see the daemon's log."
		}
		g.updateState.record(service, c)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	rc, err := g.updater.Latest(ctx, service)
	if err != nil {
		g.logf("gateway: checking %s for updates: %v", service, err)
		c.Err = "Couldn't check: " + err.Error()
		return
	}
	c.ReleaseCheck = rc
}

// checkIfStale starts a check if one is due, so an admin who just opens the
// portal sees what's waiting without asking.
func (g *Gateway) checkIfStale() {
	if g.updater != nil && g.updateState.stale(g.now(), g.routes.all()) {
		g.startCheck()
	}
}

// runUpdate runs one update to its end, on its own goroutine.
func (g *Gateway) runUpdate(service string) {
	var res updateResult
	defer func() {
		if p := recover(); p != nil {
			g.logf("gateway: updating %s: panic: %v", service, p)
			res = updateResult{Failed: true, Output: "The update stopped unexpectedly; see the daemon's log, and `outpost status " + service + "`."}
			g.activity.record(EventUpdate, "update of %s stopped unexpectedly", service)
		}
		g.updateState.finish(service, res)
	}()
	output, err := g.updater.Update(service)
	res.Output = strings.TrimSpace(output)
	if err != nil {
		res.Failed = true
		res.Output = strings.TrimSpace(res.Output + "\n" + err.Error())
		g.logf("gateway: updating %s failed: %v", service, err)
		g.activity.record(EventUpdate, "update of %s failed", service)
	} else {
		g.logf("gateway: updated %s", service)
		g.activity.record(EventUpdate, "update of %s finished", service)
	}
	// Show the new release as soon as the page says it's done, not a tick
	// later, and ask afresh, since the last answer was about the old one. The
	// service stays marked as running until this is over.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.refreshRoutes(ctx)
	if rt, ok := g.routes.get(service); ok {
		g.checkOne(service, rt.Release)
	}
}

// waiting counts the apps with a newer release to install, from what's known.
func (g *Gateway) waiting() int {
	s := &g.updateState
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rt := range g.routes.all() {
		if c, ok := s.checks[rt.Name]; ok && c.Newer && c.Installed == rt.Release {
			n++
		}
	}
	return n
}

// Update row states.
const (
	updateRunning   = "running"
	updateAvailable = "available"
	updateCurrent   = "current"
	updateChecking  = "checking"
	updateUnknown   = "unknown"
)

// updateRow is one app on the update page.
type updateRow struct {
	Name      string
	Title     string
	Source    string
	Installed string
	State     string
	Latest    string
	Detail    string // a note or a failed check, in words
	Result    string // what the last update did
	Failed    bool
}

// updateRows lists the apps from a release, and names those that aren't.
func (g *Gateway) updateRows() (rows []updateRow, byHand []string, busy bool) {
	s := &g.updateState
	s.mu.Lock()
	defer s.mu.Unlock()
	busy = s.checking
	for _, rt := range g.routes.all() {
		title := rt.Title
		if title == "" {
			title = rt.Name
		}
		if rt.Source == "" {
			byHand = append(byHand, title)
			continue
		}
		row := updateRow{Name: rt.Name, Title: title, Source: rt.Source, Installed: rt.Release, State: updateUnknown}
		if row.Installed == "" {
			row.Installed = "an unrecorded release"
		}
		c, known := s.checks[rt.Name]
		known = known && c.Installed == rt.Release
		switch {
		case s.running[rt.Name]:
			row.State = updateRunning
			busy = true
		case known && c.Err != "":
			row.Detail = c.Err
		case known && c.Newer:
			row.State, row.Latest = updateAvailable, c.Latest
		case known:
			row.State, row.Detail = updateCurrent, c.Note
		case s.checking:
			row.State = updateChecking
		}
		if r, ok := s.results[rt.Name]; ok && !s.running[rt.Name] {
			row.Result, row.Failed = r.Output, r.Failed
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title) })
	sort.Strings(byHand)
	return rows, byHand, busy
}

// updatesUser is the admin behind a request to the update page. It writes the
// answer itself and returns false for anyone else. Admin comes from auth.db on
// every request, so revoking it takes effect at once.
func (g *Gateway) updatesUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		http.Redirect(w, r, "/login?next=%2Fupdates", http.StatusSeeOther)
		return auth.User{}, false
	}
	g.touch(tok, s)
	if !u.Admin {
		g.message(w, r, http.StatusForbidden, "Admins only",
			"Only an admin can update apps.", g.portalLink(), "Back to apps")
		return auth.User{}, false
	}
	return u, true
}

func (g *Gateway) updatesPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.updatesUser(w, r)
	if !ok {
		return
	}
	g.checkIfStale()
	rows, byHand, busy := g.updateRows()
	g.updateState.mu.Lock()
	at := g.updateState.checkedAt
	g.updateState.mu.Unlock()
	d := pageData{
		Title:      "Updates",
		Who:        displayName(u),
		Admin:      u.Admin,
		UpdateRows: rows,
		ByHand:     strings.Join(byHand, ", "),
		Refresh:    busy,
	}
	if !at.IsZero() {
		d.Checked = checkedWords(at, g.now())
	}
	g.render(w, r, http.StatusOK, "updates", d)
}

func checkedWords(at, now time.Time) string {
	switch d := now.Sub(at); {
	case d < time.Minute:
		return "Checked just now"
	case d < time.Hour:
		return fmt.Sprintf("Checked %d min ago", int(d.Minutes()))
	}
	return "Checked at " + at.Format("15:04")
}

func (g *Gateway) updatesSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.updatesUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/updates", "Back to updates")
		return
	}
	switch r.PostForm.Get("action") {
	case "check":
		g.startCheck()
	case "update":
		// Only a service in the route table with a recorded source. This is
		// also what keeps the name from being anything the update command
		// could read as a flag or a path.
		name := r.PostForm.Get("service")
		rt, ok := g.routes.get(name)
		if !ok || rt.Source == "" {
			g.message(w, r, http.StatusNotFound, "Nothing to update",
				"That isn't an app installed from a release.", "/updates", "Back to updates")
			return
		}
		if g.updateState.begin(name) {
			// The only record of who asked, since the update itself runs as
			// the daemon.
			g.activity.record(EventUpdate, "%s started an update of %s from %s", u.Username, name, g.clientIP(r))
			g.logf("gateway: %s is updating %s", u.Username, name)
			go g.runUpdate(name)
		}
	}
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}
