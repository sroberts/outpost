package gateway

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"strings"
)

//go:embed templates/*.html static/portal.css static/catalog.js static/poll.js static/selfupdate.js static/menus.js static/lighthouse.webp
var assets embed.FS

const (
	cssPath = "/.outpost/static/portal.css"
	// jsPath is the catalog's filter, its only script. It's a file rather
	// than inline because the pages' CSP allows scripts from 'self' only.
	jsPath = "/.outpost/static/catalog.js"
	// pollPath reloads the catalog while an action runs. A meta refresh
	// would close a Manage menu or a confirmation under the admin's thumb.
	pollPath = "/.outpost/static/poll.js"
	// selfJSPath waits out Outpost's restart on the update page, asking
	// until it's back, where a refresh would stop at the browser's error page.
	selfJSPath = "/.outpost/static/selfupdate.js"
	// menusPath lets Escape close a menu on any signed-in page.
	menusPath = "/.outpost/static/menus.js"
	// artPath is the lighthouse from the Outpost header, cropped, for the
	// sign-in page.
	artPath = "/.outpost/static/lighthouse.webp"
)

// pages are the portal's templates, each parsed with the shared layout.
type pages map[string]*template.Template

// assetURLs carry a hash of each file, so a browser holding an hour-old copy
// from before an upgrade asks for the new one instead.
var (
	cssURL   = versioned(cssPath, "static/portal.css")
	jsURL    = versioned(jsPath, "static/catalog.js")
	pollURL  = versioned(pollPath, "static/poll.js")
	selfURL  = versioned(selfJSPath, "static/selfupdate.js")
	menusURL = versioned(menusPath, "static/menus.js")
	artURL   = versioned(artPath, "static/lighthouse.webp")
)

func versioned(urlPath, file string) string {
	body, err := assets.ReadFile(file)
	if err != nil {
		return urlPath
	}
	sum := sha256.Sum256(body)
	return urlPath + "?v=" + hex.EncodeToString(sum[:6])
}

func loadPages() (pages, error) {
	p := pages{}
	for _, name := range []string{"login", "setup", "portal", "message", "account", "updates"} {
		t, err := template.New(name).Funcs(template.FuncMap{"lower": strings.ToLower}).
			ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		p[name] = t
	}
	return p, nil
}

// pageData is everything any page shows.
type pageData struct {
	Title    string
	Error    string
	Notice   string // a quiet confirmation, such as being signed out
	App      string
	AppTitle string
	Next     string
	Username string

	Token       string
	MinPassword int

	Who     string
	SignIns []signInView
	// SignInNotice confirms what happened to a sign-in, beside the list.
	SignInNotice string
	Admin        bool
	Stations     []station
	Counts       counts
	Search       bool

	Waiting     int  // apps known to have a newer release; only counted for an admin
	UpdatesLink bool // the updates page is there for this user, waiting or not
	UpdateRows  []updateRow
	ByHand      string // apps not from a release, in words
	Checked     string // when releases were last checked, in words
	Refresh     bool   // reload while something on the page is in progress
	// Poll reloads the catalog while an action runs, by script so an open
	// menu stays open, with a meta refresh only where there's no script.
	Poll bool
	// Self is Outpost's own row on the update page, when it has one, and
	// SelfPoll waits out its restart by script. SelfBusy holds back the
	// apps' updates meanwhile.
	Self     *selfRow
	SelfPoll bool
	SelfBusy bool

	Message  string
	Link     string
	LinkText string
	// Station, on a message page about one app, draws it as it stands.
	Station *station

	// Filled in by render for every page.
	CSS, JS, PollJS, Art string // the assets' versioned URLs
	SelfJS               string
	MenusJS              string
	Portal               string // what the portal calls itself
	Host                 string // the portal's address, for reading
	Home                 string // the catalog's URL
}

// render writes a page with the headers every gateway page carries.
func (g *Gateway) render(w http.ResponseWriter, r *http.Request, status int, name string, d pageData) {
	d.CSS, d.JS, d.PollJS, d.Art, d.SelfJS, d.MenusJS = cssURL, jsURL, pollURL, artURL, selfURL, menusURL
	d.Portal = g.cfg.PortalName()
	d.Host = strings.TrimPrefix(strings.TrimPrefix(g.portalOrigin, "https://"), "http://")
	d.Home = g.portalLink()
	var buf bytes.Buffer
	if err := g.pages[name].ExecuteTemplate(&buf, "layout", d); err != nil {
		g.logf("gateway: render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	g.pageHeaders(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w) // a client that hung up has nothing to be told
}

// pageHeaders lock the gateway's own pages down. They go on nothing proxied:
// an app's headers are the app's business.
func (g *Gateway) pageHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	// Not no-referrer: under it a browser sends Origin: null and no Referer
	// even on a same-origin form POST, and sameOrigin refuses every login.
	// same-origin still keeps the setup token and next URLs from other sites.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	if g.secure && g.hostOf(r) == g.portalHost {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

func (g *Gateway) serveCSS(w http.ResponseWriter, _ *http.Request) {
	body, err := assets.ReadFile("static/portal.css")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body) // a client that hung up has nothing to be told
}

func (g *Gateway) serveJS(w http.ResponseWriter, r *http.Request) {
	g.serveAsset(w, r, "static/catalog.js", "text/javascript; charset=utf-8")
}

func (g *Gateway) servePoll(w http.ResponseWriter, r *http.Request) {
	g.serveAsset(w, r, "static/poll.js", "text/javascript; charset=utf-8")
}

func (g *Gateway) serveArt(w http.ResponseWriter, r *http.Request) {
	g.serveAsset(w, r, "static/lighthouse.webp", "image/webp")
}

func (g *Gateway) serveAsset(w http.ResponseWriter, _ *http.Request, file, contentType string) {
	body, err := assets.ReadFile(file)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body) // a client that hung up has nothing to be told
}

// message shows a one-line explanation, with an optional way on.
func (g *Gateway) message(w http.ResponseWriter, r *http.Request, status int, title, msg, link, linkText string) {
	g.render(w, r, status, "message", pageData{Title: title, Message: msg, Link: link, LinkText: linkText})
}
