package gateway

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// searchFrom is how many apps the catalog lists before it offers a filter.
const searchFrom = 13

// allowed reports whether u may open the app. Admins reach everything; an
// empty allow list means every user.
func allowed(u auth.User, rt route) bool {
	if u.Admin || len(rt.Allow) == 0 {
		return true
	}
	for _, a := range rt.Allow {
		if group, ok := strings.CutPrefix(a, "group:"); ok {
			for _, g := range u.Groups {
				if g == group {
					return true
				}
			}
		} else if a == u.Username {
			return true
		}
	}
	return false
}

// warnUnknownAllow logs, once each, allow entries that name nobody. A
// manifest may be written before its users exist, so this is a warning, not
// a refusal. Callers hold g.routes.mu.
func (g *Gateway) warnUnknownAllow(service string, allow []string) {
	if len(allow) == 0 {
		return
	}
	var users []auth.User
	for _, a := range allow {
		key := "allow:" + service + ":" + a
		if g.routes.skipped[key] {
			continue
		}
		if users == nil {
			var err error
			if users, err = g.auth.ListUsers(); err != nil {
				return
			}
		}
		if !namesSomeone(a, users) {
			g.routes.skipped[key] = true
			g.logf("gateway: %s: allow names %q, which matches no user or group yet", service, a)
		}
	}
}

func namesSomeone(entry string, users []auth.User) bool {
	group, isGroup := strings.CutPrefix(entry, "group:")
	for _, u := range users {
		if !isGroup && u.Username == entry {
			return true
		}
		for _, g := range u.Groups {
			if isGroup && g == group {
				return true
			}
		}
	}
	return false
}

// Station states, in the order the catalog lists them.
const (
	stateUp       = "up"
	stateStarting = "starting"
	stateDown     = "down"
)

// station is one app as the catalog draws it.
type station struct {
	Name        string // the service name, for the access check and commands
	Title       string // what to call it
	Description string
	URL         string
	Addr        string // the address without a scheme, for reading
	State       string
	Label       string // the state in words
	Reason      string // why it isn't up, in words
	Command     string // what an admin runs to look closer
	Hidden      bool

	// For an admin, when the portal can control apps.
	Acts   []appAction // the Manage menu
	Busy   string      // what's under way instead, in words
	Done   string      // how the last action went, in words
	Failed bool
	Alert  bool // a failure new enough to announce
}

// stations lists the apps u may open, running first.
func (g *Gateway) stations(u auth.User) []station {
	var out []station
	for _, rt := range g.routes.all() {
		if !allowed(u, rt) || (rt.Hidden && !u.Admin) {
			continue
		}
		origin := g.appOrigin(rt.Name)
		st := station{
			Name:        rt.Name,
			Title:       rt.Title,
			Description: rt.Description,
			URL:         origin + "/",
			Addr:        strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://"),
			Hidden:      rt.Hidden,
		}
		if st.Title == "" {
			st.Title = rt.Name
		}
		st.State, st.Label, st.Reason = g.describe(rt)
		if st.State != stateUp && u.Admin {
			st.Command = "outpost status " + rt.Name
		}
		out = append(out, st)
	}
	rank := map[string]int{stateUp: 0, stateStarting: 1, stateDown: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out
}

// describe turns the supervisor's account of a service into a state and the
// words for it. Restart counts and backoff stay in `outpost status`; the
// catalog says only what someone deciding whether to tap needs.
func (g *Gateway) describe(rt route) (state, label, reason string) {
	switch {
	case rt.Status == "running":
		return stateUp, "Running", ""
	case rt.Status == "starting":
		return stateStarting, "Starting", "Starting up"
	case rt.Desired == "stopped":
		return stateDown, "Down", "Stopped on purpose"
	case rt.Halted && rt.RestartCount == 0:
		// Halted on its first exit, by a restart policy that doesn't restart
		// it: there were no repeated crashes to count. After a daemon restart
		// the status reads stopped either way, so only a failure still on
		// record is called a crash.
		if rt.Status == "failed" {
			return stateDown, "Down", "Crashed, and isn't set to restart"
		}
		return stateDown, "Down", "Stopped, and isn't set to restart"
	case rt.Halted:
		return stateDown, "Down", fmt.Sprintf("Stopped after %s", crashes(rt.RestartCount))
	case !rt.NextAttempt.IsZero() && rt.NextAttempt.After(g.now()):
		wait := rt.NextAttempt.Sub(g.now()).Round(time.Second)
		if wait < time.Second {
			return stateStarting, "Restarting", "Crashed; trying again now"
		}
		return stateStarting, "Restarting", fmt.Sprintf("Crashed; trying again in %s", wait)
	case rt.Status == "unhealthy":
		return stateDown, "Down", "Not answering its health check"
	case rt.Status == "failed":
		return stateDown, "Down", "Crashed"
	default:
		return stateDown, "Down", "Not running"
	}
}

func crashes(n int) string {
	if n == 1 {
		return "1 crash"
	}
	return fmt.Sprintf("%d crashes", n)
}

// tally counts the stations by state, for the line under the portal's name.
func tally(sts []station) (up, down int) {
	for _, s := range sts {
		if s.State == stateUp {
			up++
		} else {
			down++
		}
	}
	return up, down
}

// denied is the page for someone signed in who may not open an app: a plain
// answer, not a redirect back into a login that would only end here again.
func (g *Gateway) denied(w http.ResponseWriter, r *http.Request, rt route, u auth.User) {
	g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", u.Username, rt.Name), false)
	title := rt.Title
	if title == "" {
		title = rt.Name
	}
	g.render(w, r, http.StatusForbidden, "message", pageData{
		Title:    "No access to " + title,
		Message:  "Your account can't open " + title + ". Whoever runs this Outpost can add you to its allow list.",
		Link:     g.portalOrigin + "/",
		LinkText: "Back to apps",
	})
}

// portalLink is the catalog's address, for links home from app hosts.
func (g *Gateway) portalLink() string { return g.portalOrigin + "/" }

// signedOutURL is where a logout lands: the sign-in page, saying so.
func (g *Gateway) signedOutURL() string {
	return g.portalOrigin + "/login?" + url.Values{"out": {"1"}}.Encode()
}
