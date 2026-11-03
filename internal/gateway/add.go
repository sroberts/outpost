package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/auth"
)

// previewTimeout bounds looking a repository up: its latest release and its
// descriptor, nothing downloaded. The page waits on it.
const previewTimeout = 30 * time.Second

// Adds is what the gateway needs in order to add an app from a GitHub release.
// The daemon implements it with the same code as `outpost add <repo>`, so a
// service added here is checked, installed and declared exactly as one added
// from the CLI.
type Adds interface {
	// Preview says what adding repo would do, without downloading or writing
	// anything: the manifest it would write and the asset it would install.
	Preview(ctx context.Context, repo string) (output string, err error)
	// Add installs repo's latest release and writes its manifest. It never
	// overwrites a service that's already declared. The output is what it
	// did, for showing to the admin, whether or not it failed.
	Add(repo string) (output string, err error)
}

// addState is the one add there may be at a time, and how the last ended. It
// lives in memory: once an add is over, the manifest is the record.
type addState struct {
	mu      sync.Mutex
	running string // the repository being added, or ""
	last    string // the repository the result is about
	result  updateResult
	done    bool
}

func (s *addState) begin(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running != "" {
		return false
	}
	s.running, s.done = repo, false
	return true
}

func (s *addState) finish(repo string, r updateResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.last, s.result, s.done = "", repo, r, true
}

// addView is the add page's state, for the template.
type addView struct {
	Repo    string // what's in the box
	Error   string // why the repository couldn't be looked up or added
	Preview string // what adding it would do, in the CLI's words
	Target  string // the repository the preview is about, canonical
	Running string // the repository being added now
	Last    string // the repository the result is about
	Result  string
	Failed  bool
}

// parseRepoRef reads a repository the way `outpost add` does, and returns it
// as owner/name. The canonical form is all that's passed on: it can't carry a
// flag, a path or another host.
func parseRepoRef(s string) (string, error) {
	repo, err := acquire.ParseRepo(s)
	if err != nil {
		return "", err
	}
	// GitHub names never start with a dash, and the add command reads one
	// that does as a flag.
	if strings.HasPrefix(repo.Owner, "-") || strings.HasPrefix(repo.Name, "-") {
		return "", errBadRepo
	}
	return repo.String(), nil
}

type repoError string

func (e repoError) Error() string { return string(e) }

const errBadRepo = repoError("that isn't a GitHub repository name")

// addUser is the admin behind a request to the add page.
func (g *Gateway) addUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	return g.adminUser(w, r, "/add", "Only an admin can add apps.")
}

func (g *Gateway) addPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.addUser(w, r)
	if !ok {
		return
	}
	g.renderAdd(w, r, u, http.StatusOK, addView{})
}

// renderAdd draws the add page with v, filled in with whatever add is running
// or last finished.
func (g *Gateway) renderAdd(w http.ResponseWriter, r *http.Request, u auth.User, status int, v addView) {
	s := &g.addState
	s.mu.Lock()
	v.Running = s.running
	if s.done {
		v.Last, v.Result, v.Failed = s.last, s.result.Output, s.result.Failed
	}
	s.mu.Unlock()
	g.render(w, r, status, "add", pageData{
		Title:   "Add an app",
		Who:     displayName(u),
		Admin:   u.Admin,
		Add:     &v,
		Refresh: v.Running != "",
	})
}

func (g *Gateway) addSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.addUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/add", "Back to adding an app")
		return
	}
	typed := strings.TrimSpace(r.PostForm.Get("repo"))
	repo, err := parseRepoRef(typed)
	if err != nil {
		g.renderAdd(w, r, u, http.StatusBadRequest, addView{Repo: typed,
			Error: "That isn't a GitHub repository. Paste its address, such as https://github.com/owner/repo, or owner/repo."})
		return
	}
	switch r.PostForm.Get("action") {
	case "preview":
		g.previewAdd(w, r, u, typed, repo)
	case "add":
		if g.addState.begin(repo) {
			// The only record of who asked, since the add itself runs as the
			// daemon.
			g.activity.record(EventUpdate, "%s started adding %s from %s", u.Username, repo, g.clientIP(r))
			g.logf("gateway: %s is adding %s", u.Username, repo)
			go g.runAdd(repo)
		}
		http.Redirect(w, r, "/add", http.StatusSeeOther)
	default:
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/add", "Back to adding an app")
	}
}

// previewAdd looks the repository up and shows what adding it would do, with
// the button that does it. Nothing is installed until that's pressed.
func (g *Gateway) previewAdd(w http.ResponseWriter, r *http.Request, u auth.User, typed, repo string) {
	ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
	defer cancel()
	out, err := g.adder.Preview(ctx, repo)
	v := addView{Repo: typed}
	if err != nil {
		g.logf("gateway: looking up %s to add: %v", repo, err)
		v.Error = "Couldn't add " + repo + ": " + err.Error()
		g.renderAdd(w, r, u, http.StatusOK, v)
		return
	}
	v.Preview, v.Target = strings.TrimSpace(out), repo
	g.renderAdd(w, r, u, http.StatusOK, v)
}

// runAdd runs one add to its end, on its own goroutine.
func (g *Gateway) runAdd(repo string) {
	var res updateResult
	defer func() {
		if p := recover(); p != nil {
			g.logf("gateway: adding %s: panic: %v", repo, p)
			res = updateResult{Failed: true, Output: "Adding it stopped unexpectedly; see the daemon's log."}
			g.activity.record(EventUpdate, "adding %s stopped unexpectedly", repo)
		}
		g.addState.finish(repo, res)
	}()
	output, err := g.adder.Add(repo)
	res.Output = strings.TrimSpace(output)
	if err != nil {
		res.Failed = true
		res.Output = strings.TrimSpace(res.Output + "\n" + err.Error())
		g.logf("gateway: adding %s failed: %v", repo, err)
		g.activity.record(EventUpdate, "adding %s failed", repo)
		return
	}
	g.logf("gateway: added %s", repo)
	g.activity.record(EventUpdate, "added %s", repo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g.refreshRoutes(ctx)
}
