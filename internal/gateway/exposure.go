package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// maxExposureForm bounds the Internet access page's form: a list of paths.
const maxExposureForm = 64 << 10

// Exposure is what the gateway needs to set how an app is reached without a
// login. A setting made here wins over the manifest's funnel and
// public_paths; Reset goes back to the manifest's. Each is checked by the
// manifest's own rules and applied before it returns.
type Exposure interface {
	Set(ctx context.Context, service string, funnel bool, publicPaths []string, by string) error
	Reset(ctx context.Context, service, by string) error
}

// publicView is the Internet access page's state, for the template.
type publicView struct {
	Rows []publicRow
}

type publicRow struct {
	Name, Title string
	Paths       string // one per line, for the form
	PathList    []string
	Funnel      bool
	// CanFunnel is whether the app has a tailnet node for Funnel to be on.
	CanFunnel bool
	// State is how Funnel is doing, in words; Live is whether the internet
	// reaches it now, and URLs where.
	State string
	Live  bool
	URLs  []string
	// FromPortal says this page set it, over what the manifest says, in
	// Manifest.
	FromPortal bool
	Manifest   string
	Notice     string
	Error      string
	// Health is its station's state: up when the internet reaches it, down
	// when Funnel is on and failing, starting on the way, off when only the
	// tailnet can; HealthWords says it for a reader.
	Health, HealthWords string
}

func (g *Gateway) publicUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	return g.adminUser(w, r, "/public", "Only an admin can change what the internet can reach.")
}

func (g *Gateway) publicPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.publicUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	notice := map[string]string{}
	if name := q.Get("app"); name != "" {
		if _, ok := g.routes.get(name); ok {
			switch q.Get("done") {
			case "saved":
				notice[name] = "Saved. It takes effect in a few seconds; the app itself doesn't restart."
			case "reset":
				notice[name] = "Back to what the manifest says."
			}
		}
	}
	g.renderPublic(w, r, u, http.StatusOK, notice, nil, nil)
}

// renderPublic draws the page. typed is what a refused form had, by app,
// shown again in place of what's in force.
func (g *Gateway) renderPublic(w http.ResponseWriter, r *http.Request, u auth.User, status int, notice, errs map[string]string, typed map[string]publicRow) {
	var v publicView
	for _, rt := range g.routes.all() {
		row := publicRow{
			Name: rt.Name, Title: rt.Title, PathList: rt.PublicPaths, Funnel: rt.Funnel,
			CanFunnel:  rt.TailscaleName != "",
			FromPortal: rt.FromPortal,
			Notice:     notice[rt.Name], Error: errs[rt.Name],
		}
		if row.Title == "" {
			row.Title = titleCase(rt.Name)
		}
		row.Paths = strings.Join(rt.PublicPaths, "\n")
		row.State, row.Live = funnelWords(rt)
		switch {
		case !rt.Funnel:
			row.Health, row.HealthWords = "off", "tailnet only"
		case row.Live:
			row.Health, row.HealthWords = "up", "on the internet"
		case rt.FunnelState == "unavailable":
			row.Health, row.HealthWords = "down", "Funnel isn't working"
		default:
			row.Health, row.HealthWords = "starting", "Funnel starting"
		}
		if row.Live {
			for _, p := range rt.PublicPaths {
				row.URLs = append(row.URLs, "https://"+rt.Host+p)
			}
		}
		if rt.FromPortal {
			row.Manifest = manifestWords(rt)
		}
		if t, ok := typed[rt.Name]; ok {
			row.Paths, row.Funnel = t.Paths, t.Funnel
		}
		v.Rows = append(v.Rows, row)
	}
	g.render(w, r, status, "public", pageData{
		Title:  "Internet access",
		Who:    displayName(u),
		Admin:  u.Admin,
		Public: &v,
	})
}

// funnelWords says how an app's Funnel is doing, and whether the internet
// reaches it now.
func funnelWords(rt route) (string, bool) {
	switch {
	case !rt.Funnel:
		if len(rt.PublicPaths) == 0 {
			return "Nothing skips the login.", false
		}
		return "These skip the login on the tailnet. The internet can't reach them.", false
	case rt.FunnelState == "on" && rt.Host != "":
		return "On: the internet reaches these paths, and nothing else of this app.", true
	case rt.FunnelState == "unavailable":
		return "Unavailable: " + strings.TrimSuffix(rt.FunnelError, ".") + ". Only the tailnet reaches it; Outpost keeps trying.", false
	case rt.Status != "running":
		return "Waiting for the app to run.", false
	default:
		return "Starting.", false
	}
}

func manifestWords(rt route) string {
	funnel := "off"
	if rt.ManifestFunnel {
		funnel = "on"
	}
	paths := "no public paths"
	if len(rt.ManifestPublicPaths) > 0 {
		paths = "public paths " + strings.Join(rt.ManifestPublicPaths, ", ")
	}
	return "Set here, over the manifest, which has Funnel " + funnel + " and " + paths + "."
}

// splitPaths reads the form's paths: one per line, blank lines and repeats
// dropped. The manifest's rules check each.
func splitPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func (g *Gateway) publicSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.publicUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxExposureForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/public", "Back to internet access")
		return
	}
	f := r.PostForm
	name := f.Get("app")
	rt, ok := g.routes.get(name)
	if !ok {
		g.message(w, r, http.StatusBadRequest, "That app isn't there", "It may have been removed.", "/public", "Back to internet access")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var err error
	var done, what string
	switch f.Get("action") {
	case "save":
		funnel, paths := f.Get("funnel") != "", splitPaths(f.Get("paths"))
		err = g.exposure.Set(ctx, rt.Name, funnel, paths, u.Username)
		done, what = "saved", describeExposure(funnel, paths)
	case "reset":
		err = g.exposure.Reset(ctx, rt.Name, u.Username)
		done, what = "reset", "went back to the manifest's public paths and Funnel"
	default:
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/public", "Back to internet access")
		return
	}
	if err != nil {
		typed := map[string]publicRow{}
		if done == "saved" {
			typed[rt.Name] = publicRow{Paths: f.Get("paths"), Funnel: f.Get("funnel") != ""}
		}
		msg := err.Error()
		g.renderPublic(w, r, u, http.StatusBadRequest, nil, map[string]string{rt.Name: strings.ToUpper(msg[:1]) + msg[1:]}, typed)
		return
	}
	g.activity.record(EventExposure, "%s %s for %s from %s", u.Username, what, rt.Name, g.clientIP(r))
	g.logf("gateway: %s %s for %s", u.Username, what, rt.Name)
	// So the page it lands on shows what was just set.
	g.refreshRoutes(r.Context())
	http.Redirect(w, r, "/public?done="+done+"&app="+url.QueryEscape(rt.Name)+"#p-"+url.QueryEscape(rt.Name), http.StatusSeeOther)
}

func describeExposure(funnel bool, paths []string) string {
	var b strings.Builder
	b.WriteString("set public paths ")
	if len(paths) == 0 {
		b.WriteString("to none")
	} else {
		b.WriteString(strings.Join(paths, ", "))
	}
	if funnel {
		b.WriteString(", with Funnel on")
	} else {
		b.WriteString(", with Funnel off")
	}
	return b.String()
}
