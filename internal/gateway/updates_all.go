package gateway

import (
	"net/http"
	"strings"
)

// waitingApps lists the apps with a newer release to install, from what's
// known, by name and in the page's order. Outpost isn't one: its restart
// would cut the rest short, so it keeps its own button.
func (g *Gateway) waitingApps() (names, titles []string) {
	rows, _, _ := g.updateRows()
	for _, r := range rows {
		if r.State == updateAvailable {
			names, titles = append(names, r.Name), append(titles, r.Title)
		}
	}
	return names, titles
}

// startUpdateAll handles "Update all": every app with a newer release, one at
// a time, stopping at the first that fails, as `outpost update --all` does.
func (g *Gateway) startUpdateAll(w http.ResponseWriter, r *http.Request, username string) {
	g.claim.Lock()
	if g.selfBusy() {
		g.claim.Unlock()
		g.message(w, r, http.StatusConflict, "Busy",
			"Outpost is updating, and its restart would cut this short. Try again once it's back.", "/updates", "Back to updates")
		return
	}
	names, _ := g.waitingApps()
	s := &g.updateState
	s.mu.Lock()
	started := len(s.queue) == 0 && len(names) > 0
	if started {
		s.queue = names
		for _, n := range names {
			delete(s.results, n)
		}
	}
	s.mu.Unlock()
	g.claim.Unlock()
	if started {
		g.activity.record(EventUpdate, "%s started updating %s from %s", username, strings.Join(names, ", "), g.clientIP(r))
		g.lg("user", username, "services", strings.Join(names, ",")).Infof("gateway: %s is updating %s", username, strings.Join(names, ", "))
		go g.runQueue()
	}
	// A second press, or nothing left to update: the page shows where it is.
	http.Redirect(w, r, "/updates", http.StatusSeeOther)
}

// runQueue works through the queue on its own goroutine, one update at a
// time through the same runUpdate as a single app's button.
func (g *Gateway) runQueue() {
	s := &g.updateState
	for {
		g.claim.Lock()
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			g.claim.Unlock()
			return
		}
		name := s.queue[0]
		s.mu.Unlock()
		// Whatever was started in the meantime goes first, and ends the run:
		// an app being backed up or stopped, or Outpost restarting.
		stop := ""
		if g.selfBusy() {
			stop = "Outpost started updating"
		} else if doing, _, _ := g.ops.state(name); doing != "" {
			stop = g.titleOf(name) + " was being " + verbs[doing].did
		}
		began := stop == "" && s.begin(name)
		g.claim.Unlock()
		if stop != "" {
			g.stopQueue(name, "Update all stopped before "+g.titleOf(name)+": "+stop+". Update the rest once that's done.")
			return
		}
		if began {
			g.runUpdate(name)
		}
		// Not begun: someone's own button got there first, and that update
		// is the one that counts for it.
		s.mu.Lock()
		failed := began && s.results[name].Failed
		if len(s.queue) > 0 && s.queue[0] == name {
			s.queue = s.queue[1:]
		}
		if failed {
			s.queue = nil
		}
		s.mu.Unlock()
		if failed {
			g.lg("service", name).Warnf("gateway: update all stopped at %s", name)
			return
		}
	}
}

// stopQueue ends a run of updates early, saying why on the app it stopped at.
func (g *Gateway) stopQueue(name, why string) {
	s := &g.updateState
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = nil
	if s.results == nil {
		s.results = map[string]updateResult{}
	}
	s.results[name] = updateResult{Output: why, Failed: true}
	g.lg("service", name).Warnf("gateway: %s", why)
}
