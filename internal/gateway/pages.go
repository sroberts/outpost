package gateway

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/sroberts/outpost/internal/version"
)

//go:embed templates/*.html static/portal.css static/catalog.js static/poll.js static/return.js static/selfupdate.js static/menus.js static/logs.js static/lighthouse.webp static/push.js static/sw.js static/icon-180.png static/icon-512.png
var assets embed.FS

const (
	cssPath = "/.outpost/static/portal.css"
	// jsPath is the catalog's filter, its only script. It's a file rather
	// than inline because the pages' CSP allows scripts from 'self' only.
	jsPath = "/.outpost/static/catalog.js"
	// pollPath reloads the catalog while an action runs. A meta refresh
	// would close a Manage menu or a confirmation under the admin's thumb.
	pollPath = "/.outpost/static/poll.js"
	// returnPath reloads the catalog when its tab comes back into view.
	returnPath = "/.outpost/static/return.js"
	// selfJSPath waits out Outpost's restart on the update page, asking
	// until it's back, where a refresh would stop at the browser's error page.
	selfJSPath = "/.outpost/static/selfupdate.js"
	// menusPath lets Escape close a menu on any signed-in page.
	menusPath = "/.outpost/static/menus.js"
	// logsJSPath follows new lines on the Logs page.
	logsJSPath = "/.outpost/static/logs.js"
	// artPath is the lighthouse from the Outpost header, cropped, for the
	// sign-in page.
	artPath = "/.outpost/static/lighthouse.webp"
)

// pages are the portal's templates, each parsed with the shared layout.
type pages map[string]*template.Template

// assetURLs carry a hash of each file, so a browser holding an hour-old copy
// from before an upgrade asks for the new one instead.
var (
	cssURL    = versioned(cssPath, "static/portal.css")
	jsURL     = versioned(jsPath, "static/catalog.js")
	pollURL   = versioned(pollPath, "static/poll.js")
	returnURL = versioned(returnPath, "static/return.js")
	selfURL   = versioned(selfJSPath, "static/selfupdate.js")
	menusURL  = versioned(menusPath, "static/menus.js")
	logsJSURL = versioned(logsJSPath, "static/logs.js")
	artURL    = versioned(artPath, "static/lighthouse.webp")
)

func versioned(urlPath, file string) string {
	body, err := assets.ReadFile(file)
	if err != nil {
		return urlPath
	}
	return urlPath + "?v=" + assetHash(body)
}

func assetHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

// staticAsset is an embedded file as it's served: its hash, and a gzipped
// copy made once, since the files never change while the daemon runs.
type staticAsset struct {
	body, gz []byte
	hash     string
}

var (
	staticMu    sync.Mutex
	staticCache = map[string]*staticAsset{}
)

func loadAsset(file string) (*staticAsset, error) {
	staticMu.Lock()
	defer staticMu.Unlock()
	if a, ok := staticCache[file]; ok {
		return a, nil
	}
	body, err := assets.ReadFile(file)
	if err != nil {
		return nil, err
	}
	a := &staticAsset{body: body, hash: assetHash(body)}
	// The image is compressed already; the text is mostly repetition, and a
	// phone on a slow link gets a quarter of it.
	if !strings.HasSuffix(file, ".webp") && !strings.HasSuffix(file, ".png") {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // a valid level can't fail
		if _, err := zw.Write(body); err == nil && zw.Close() == nil && buf.Len() < len(body) {
			a.gz = buf.Bytes()
		}
	}
	staticCache[file] = a
	return a, nil
}

func loadPages() (pages, error) {
	p := pages{}
	for _, name := range []string{"login", "setup", "portal", "message", "account", "updates", "add", "secrets", "ai", "mail", "authorize", "logs"} {
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
	Page     string // the template it's drawn with, so the admin's places can mark this one
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
	AddLink     bool // the add page is there for this user
	Add         *addView
	SecretsLink bool // the secrets page is there for this user
	Secrets     *secretsView
	AILink      bool // the AI models page is there for this user
	AI          *aiView
	MailLink    bool // the Mail page is there for this user
	Mail        *mailView
	LogsLink    bool // the Logs page is there for this user
	Logs        *logsView
	UpdateRows  []updateRow
	ByHand      string // apps not from a release, in words
	AllCount    int    // apps "Update all" would update
	AllRunning  bool   // an update of one or more apps is under way
	Checked     string // when releases were last checked, in words
	Refresh     bool   // reload while something on the page is in progress
	// Icons draws the sprite the usage figures and actions use, when any show.
	Icons bool
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
	// Consent is the page asking to let an OAuth client in.
	Consent *consentView
	// Notify is the account page's Notifications section.
	Notify *notifyView

	// Filled in by render for every page.
	CSS, JS, PollJS, Art string // the assets' versioned URLs
	SelfJS               string
	MenusJS, ReturnJS    string
	LogsJS               string
	Portal               string // what the portal calls itself
	Host                 string // the portal's address, for reading
	Home                 string // the catalog's URL
	Version              string // the build running, shown to anyone signed in
	SpecLink             string // the app spec on GitHub, for an admin to hand to Claude
	TouchIcon            string // the Home Screen icon
	// PushJS keeps this device's subscription current on every signed-in
	// page, and PushKey is what it subscribes with; both "" without push.
	PushJS, PushKey string
}

// specRelease is a version that names a release tag; anything else (a dev
// build, a pseudo-version) has no tag of its own to link.
var specRelease = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// specLink is the service spec as published with the running release, so
// what an admin hands an assistant from the footer is the contract this
// Outpost implements. GitHub's rather than the portal's own copy at
// /.well-known: an assistant on the internet can reach it, and the portal
// is on the tailnet only.
func specLink(v string) string {
	ref := "main"
	if specRelease.MatchString(v) {
		ref = v
	}
	return "https://raw.githubusercontent.com/sroberts/outpost/" + ref + "/outpost-service-spec.md"
}

// render writes a page with the headers every gateway page carries.
func (g *Gateway) render(w http.ResponseWriter, r *http.Request, status int, name string, d pageData) {
	d.CSS, d.JS, d.PollJS, d.Art, d.SelfJS, d.MenusJS, d.ReturnJS = cssURL, jsURL, pollURL, artURL, selfURL, menusURL, returnURL
	d.LogsJS = logsJSURL
	d.Portal = g.cfg.PortalName()
	d.Host = strings.TrimPrefix(strings.TrimPrefix(g.portalOrigin, "https://"), "http://")
	d.Home = g.portalLink()
	d.Version = version.Get().Version
	d.Page = name
	d.TouchIcon = icon180URL
	if g.push != nil && d.Who != "" {
		d.PushJS, d.PushKey = pushJSURL, g.push.PublicKey()
	}
	// Every page an admin sees links to the same places, from the same list
	// at its foot, so getting from one to another never goes through home.
	if d.Admin && d.Who != "" {
		d.UpdatesLink = g.updater != nil
		d.AddLink = g.adder != nil
		d.SecretsLink = g.secrets != nil
		d.AILink = g.ai != nil
		d.MailLink = g.mail != nil
		d.LogsLink = g.logs != nil
		d.SpecLink = specLink(d.Version)
	}
	var buf bytes.Buffer
	if err := g.pages[name].ExecuteTemplate(&buf, "layout", d); err != nil {
		g.lg("page", name, "err", err).Errorf("gateway: render %s: %v", name, err)
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

func (g *Gateway) serveCSS(w http.ResponseWriter, r *http.Request) {
	g.serveAsset(w, r, "static/portal.css", "text/css; charset=utf-8")
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

func (g *Gateway) serveAsset(w http.ResponseWriter, r *http.Request, file, contentType string) {
	a, err := loadAsset(file)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	// Pages ask for each file by its hash, so a URL carrying the current
	// one names these bytes for good, and a phone needn't ask again until an
	// upgrade changes the hash. Anything else, an old hash or none, is kept
	// an hour, as before.
	if r.URL.Query().Get("v") == a.hash {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	body := a.body
	if a.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
			body = a.gz
		}
	}
	_, _ = w.Write(body) // a client that hung up has nothing to be told
}

// acceptsGzip is a loose reading of Accept-Encoding: any mention of gzip
// that isn't explicitly refused with q=0.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

// message shows a one-line explanation, with an optional way on.
func (g *Gateway) message(w http.ResponseWriter, r *http.Request, status int, title, msg, link, linkText string) {
	g.render(w, r, status, "message", pageData{Title: title, Message: msg, Link: link, LinkText: linkText})
}
