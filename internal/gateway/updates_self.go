package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/version"
)

// SelfUpdates is what the gateway needs in order to update Outpost itself. It
// is apart from Updates so that anything implementing that alone still gives
// the page it always did. The daemon implements it by starting
// `outpost update --self`'s own code in a process that outlives its restart.
type SelfUpdates interface {
	// Latest reports where this build stands against Outpost's latest
	// release. It goes over the network.
	Latest(ctx context.Context) (ReleaseCheck, error)
	// Refusal says why Outpost can't be updated from the page, in words, or
	// "" if it can. It asks the init system, so it isn't quick.
	Refusal() string
	// Start begins an update and returns once it's under way, or says why it
	// wasn't started. by is the admin who asked.
	Start(by string) error
	// Status is the latest update's progress, or its outcome for a while
	// after; false when there's nothing to show.
	Status() (SelfStatus, bool)
}

// SelfStatus is how an update of Outpost stands.
type SelfStatus struct {
	Phase  string // starting, downloading, restarting, waiting, rolling back, done or failed
	Final  bool   // over, one way or the other
	Failed bool
	From   string // the release it started from
	To     string // the release being installed, once known
	By     string // the admin who asked
	Output string // what it did, as `outpost update --self` says it
	Log    string // where to look if it never comes back
	// RolledBack is whether the previous release was put back, and Error
	// why it failed, in one line.
	RolledBack bool
	Error      string
	// Announce is set on the first read of a finished update, so its outcome
	// is recorded once, by whichever daemon is running by then.
	Announce bool
}

// selfPhaseText says what an update of Outpost is doing now, naming the
// releases once they're known. The page's script shows it as it changes.
func selfPhaseText(st SelfStatus) string {
	to := "the new release"
	if st.To != "" {
		to = st.To
	}
	switch st.Phase {
	case "downloading":
		return "Downloading " + to + "."
	case "restarting":
		return "Restarting Outpost on " + to + "."
	case "waiting":
		return "Waiting for Outpost to come back on " + to + ". This can take a few minutes."
	case "rolling back":
		return to + " didn't come up, so Outpost is going back to " + releaseName(st.From) + "."
	}
	return "Getting ready to install " + to + "."
}

// releaseName is a build as the page names it: its release, without the
// commit that `outpost --version` adds.
func releaseName(build string) string {
	if tag, _, ok := strings.Cut(build, " ("); ok {
		return tag
	}
	if build == "" {
		return "the previous release"
	}
	return build
}

// sameRelease reports whether two builds are the same release, by their
// releaseName and ignoring a leading v, as `outpost update` compares them:
// "v1.2.0 (abc123)" and "1.2.0" are one release.
func sameRelease(a, b string) bool {
	norm := func(s string) string { return strings.TrimPrefix(releaseName(s), "v") }
	return norm(a) == norm(b)
}

// selfOutcome is how a finished update of Outpost ended, in one sentence,
// and whether it needs attention. running is the build serving the page,
// which is the only first-hand word on what came back.
func selfOutcome(st SelfStatus, running string) (summary string, alert bool) {
	to := st.To
	if to == "" {
		to = "the new release"
	}
	switch {
	case !st.Failed && st.To != "" && sameRelease(running, st.To):
		return "Outpost was updated to " + st.To + ".", false
	case !st.Failed:
		return "Outpost wasn't changed: it is running " + releaseName(running) + ".", false
	case st.RolledBack && sameRelease(running, st.From):
		return to + " didn't start, so Outpost went back to " + releaseName(st.From) + ", which is running.", false
	case st.RolledBack:
		return to + " didn't start, and going back didn't finish cleanly. Outpost is running " +
			releaseName(running) + "; the log below says what happened.", true
	case !sameRelease(running, st.From):
		return "The update stopped partway. Outpost is running " + releaseName(running) + "; the log below says what happened.", true
	}
	why := strings.TrimSuffix(strings.TrimPrefix(st.Error, "outpost: "), ".")
	if why == "" {
		why = "it stopped before changing anything"
	}
	return "Outpost wasn't updated: " + why + ". Nothing was changed.", false
}

// selfRow is Outpost on the update page.
type selfRow struct {
	Installed string
	State     string // as an app's, or "refused"
	Latest    string
	Detail    string
	Result    string // the update's whole output, behind a disclosure
	Summary   string // how it ended, in one sentence
	Failed    bool
	Alert     bool // ended somewhere that needs looking at
	Log       string
}

const updateRefused = "refused"

// checkSelf records where Outpost stands, and whether it can be updated from
// here. It runs inside a page's check, which is hourly, because the refusal
// asks the init system and is too slow for every page.
func (g *Gateway) checkSelf() {
	var c checked
	refusal := ""
	defer func() {
		if p := recover(); p != nil {
			g.logf("gateway: checking Outpost for updates: panic: %v", p)
			c.Err = "The check failed unexpectedly; see the daemon's log."
		}
		s := &g.updateState
		s.mu.Lock()
		s.self, s.selfChecked, s.selfRefusal = c, true, refusal
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
	defer cancel()
	refusal = g.selfUpdater.Refusal()
	rc, err := g.selfUpdater.Latest(ctx)
	if err != nil {
		g.logf("gateway: checking Outpost for updates: %v", err)
		c.Err = "Couldn't check: " + err.Error()
		return
	}
	c.ReleaseCheck = rc
}

// selfMemoFor is how long selfState's answer is reused. The catalog asks once
// per app on every render, and each answer reads a file.
const selfMemoFor = time.Second

// selfState is how an update of Outpost stands, as read within the last
// second. It's for the places that ask often; what an admin is looking at, or
// about to act on, uses selfStateNow.
func (g *Gateway) selfState() (SelfStatus, bool) {
	if g.selfUpdater == nil {
		return SelfStatus{}, false
	}
	s := &g.updateState
	s.mu.Lock()
	// The wall clock, not g.now: the memo is about load, not about time
	// as the gateway's logic sees it.
	if !s.selfMemoAt.IsZero() && time.Since(s.selfMemoAt) < selfMemoFor {
		st, ok := s.selfMemo, s.selfMemoOK
		s.mu.Unlock()
		return st, ok
	}
	s.mu.Unlock()
	return g.selfStateNow()
}

// selfStateNow reads how an update of Outpost stands, and records a finished
// one's outcome the first time it's seen. The daemon that started an update
// is gone by the time it ends, so the record falls to the one after. An
// outcome too old to show is still recorded, then not shown.
func (g *Gateway) selfStateNow() (SelfStatus, bool) {
	if g.selfUpdater == nil {
		return SelfStatus{}, false
	}
	st, ok := g.selfUpdater.Status()
	defer func() {
		s := &g.updateState
		s.mu.Lock()
		s.selfMemo, s.selfMemoOK, s.selfMemoAt = st, ok, time.Now()
		s.mu.Unlock()
	}()
	if st.Announce {
		to := st.To
		if to == "" {
			to = "the latest release"
		}
		by := ""
		if st.By != "" {
			by = ", which " + st.By + " started,"
		}
		if st.Failed {
			g.logf("gateway: the update of Outpost to %s failed", to)
			g.activity.record(EventUpdate, "update of Outpost to %s%s failed", to, by)
		} else {
			g.logf("gateway: the update of Outpost to %s finished", to)
			g.activity.record(EventUpdate, "update of Outpost to %s%s finished", to, by)
		}
		st.Announce = false // recorded; a remembered copy mustn't record it again
	}
	return st, ok
}

// selfBusy reports whether an update of Outpost is under way, or being
// started. Apps aren't updated or managed meanwhile: the restart would cut
// what they're doing short.
func (g *Gateway) selfBusy() bool { return g.selfBusyFrom(g.selfState) }

// selfBusyNow is selfBusy without the memo, for deciding whether to start one.
func (g *Gateway) selfBusyNow() bool { return g.selfBusyFrom(g.selfStateNow) }

func (g *Gateway) selfBusyFrom(state func() (SelfStatus, bool)) bool {
	if g.selfUpdater == nil {
		return false
	}
	g.updateState.mu.Lock()
	starting := g.updateState.selfStarting
	g.updateState.mu.Unlock()
	if starting {
		return true
	}
	st, ok := state()
	return ok && !st.Final
}

// selfWaiting reports whether there's a newer Outpost to install from here.
// Called with updateState.mu held.
func (s *updateState) selfWaiting() bool {
	return s.selfChecked && s.self.Err == "" && s.self.Newer && s.selfRefusal == ""
}

// selfUpdateRow is Outpost's row on the update page, or nil without one.
func (g *Gateway) selfUpdateRow() *selfRow {
	if g.selfUpdater == nil {
		return nil
	}
	st, ok := g.selfStateNow()
	s := &g.updateState
	s.mu.Lock()
	defer s.mu.Unlock()
	row := &selfRow{Installed: version.Get().String(), State: updateUnknown}
	switch {
	case s.selfStarting || ok && !st.Final:
		row.State, row.Latest, row.Log = updateRunning, st.To, st.Log
		if st.To == "" {
			st.To = s.self.Latest // what the check offered, until the helper says
		}
		row.Detail = selfPhaseText(st)
	case s.selfChecked && s.self.Err != "":
		row.Detail = s.self.Err
	case s.selfChecked && s.self.Newer && s.selfRefusal != "":
		row.State, row.Latest, row.Detail = updateRefused, s.self.Latest, s.selfRefusal
	case s.selfChecked && s.self.Newer:
		row.State, row.Latest = updateAvailable, s.self.Latest
	case s.selfChecked:
		row.State, row.Detail = updateCurrent, s.self.Note
	case s.checking:
		row.State = updateChecking
	}
	if ok && st.Final {
		row.Result, row.Failed = st.Output, st.Failed
		row.Summary, row.Alert = selfOutcome(st, row.Installed)
	}
	return row
}

// startSelfUpdate handles the page's button for updating Outpost.
func (g *Gateway) startSelfUpdate(w http.ResponseWriter, r *http.Request, username string) {
	if g.selfUpdater == nil {
		g.message(w, r, http.StatusNotFound, "Nothing to update",
			"Outpost can't be updated from here.", "/updates", "Back to updates")
		return
	}
	// Under the claim, which an app's update also takes before it begins, so
	// neither can start in the moment the other is checking.
	g.claim.Lock()
	s := &g.updateState
	s.mu.Lock()
	apps := len(s.running) > 0
	s.mu.Unlock()
	if apps {
		g.claim.Unlock()
		g.message(w, r, http.StatusConflict, "Busy",
			"An app is being updated, and restarting Outpost would cut that short. Try again once it's done.", "/updates", "Back to updates")
		return
	}
	if name, doing := g.ops.anyBusy(); doing != "" {
		g.claim.Unlock()
		g.message(w, r, http.StatusConflict, "Busy",
			g.titleOf(name)+" is being "+verbs[doing].did+", and restarting Outpost would cut that short. Try again once it's done.",
			"/updates", "Back to updates")
		return
	}
	if g.selfBusyNow() {
		// A second press of the button, or one in another tab: the first
		// is what's running, and the page shows it.
		g.claim.Unlock()
		http.Redirect(w, r, "/updates", http.StatusSeeOther)
		return
	}
	s.mu.Lock()
	s.selfStarting = true
	s.mu.Unlock()
	g.claim.Unlock()

	err := g.selfUpdater.Start(username)
	s.mu.Lock()
	s.selfStarting = false
	s.selfMemoAt = time.Time{} // Start wrote a status the memo hasn't seen
	s.mu.Unlock()
	if err != nil {
		g.logf("gateway: %s asked to update Outpost: %v", username, err)
		g.message(w, r, http.StatusConflict, "Outpost wasn't updated", err.Error(), "/updates", "Back to updates")
		return
	}
	// The only record of who asked, since the update itself runs as the daemon.
	g.activity.record(EventUpdate, "%s started an update of Outpost from %s", username, g.clientIP(r))
	g.logf("gateway: %s is updating Outpost", username)
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}

// selfStatusJSON is what the page's script polls while Outpost restarts. It
// answers in JSON, never with a redirect to sign in, since a script can't
// follow one to anywhere useful; and with no CORS headers, so only the
// portal's own pages can read it.
func (g *Gateway) selfStatusJSON(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"error":"method not allowed"}` + "\n")) // a client that hung up has nothing to be told
		return
	}
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"sign in"}` + "\n")) // a client that hung up has nothing to be told
		return
	}
	g.touch(tok, s)
	if !u.Admin {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"admins only"}` + "\n")) // a client that hung up has nothing to be told
		return
	}
	resp := struct {
		Phase string `json:"phase"`
		Final bool   `json:"final"`
		Words string `json:"words,omitempty"` // the phase, as the page says it
	}{Final: true}
	if st, ok := g.selfStateNow(); ok {
		resp.Phase, resp.Final = st.Phase, st.Final
		if !st.Final {
			resp.Words = selfPhaseText(st)
		}
	}
	g.updateState.mu.Lock()
	if g.updateState.selfStarting {
		resp.Phase, resp.Final, resp.Words = "starting", false, selfPhaseText(SelfStatus{})
	}
	g.updateState.mu.Unlock()
	_ = json.NewEncoder(w).Encode(resp) // a client that hung up has nothing to be told
}

// noteSelfUpdate takes a look at an update of Outpost, so that its outcome
// reaches the activity feed even if no admin opens the page.
func (g *Gateway) noteSelfUpdate() {
	if g.selfUpdater != nil {
		g.selfState()
	}
}
