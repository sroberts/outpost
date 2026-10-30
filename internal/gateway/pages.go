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

//go:embed templates/*.html static/portal.css static/catalog.js static/lighthouse.webp
var assets embed.FS

const (
	cssPath = "/.outpost/static/portal.css"
	// jsPath is the catalog's filter, its only script. It's a file rather
	// than inline because the pages' CSP allows scripts from 'self' only.
	jsPath = "/.outpost/static/catalog.js"
	// artPath is the lighthouse from the Outpost header, cropped, for the
	// sign-in page.
	artPath = "/.outpost/static/lighthouse.webp"
)

// pages are the portal's templates, each parsed with the shared layout.
type pages map[string]*template.Template

// assetURLs carry a hash of each file, so a browser holding an hour-old copy
// from before an upgrade asks for the new one instead.
var (
	cssURL = versioned(cssPath, "static/portal.css")
	jsURL  = versioned(jsPath, "static/catalog.js")
	artURL = versioned(artPath, "static/lighthouse.webp")
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
	SignIns     []signInRow

	Who      string
	Admin    bool
	Stations []station
	Up, Down int
	Search   bool

	Updates    bool // this admin can reach the update page
	Waiting    int  // apps known to have a newer release
	UpdateRows []updateRow
	ByHand     string // apps not from a release, in words
	Checked    string // when releases were last checked, in words
	Refresh    bool   // reload while something on the page is in progress

	Message  string
	Link     string
	LinkText string
	// Station, on a message page about one app, draws it as it stands.
	Station *station

	// Filled in by render for every page.
	CSS, JS, Art string // the assets' versioned URLs
	Portal       string // what the portal calls itself
	Host         string // the portal's address, for reading
	Home         string // the catalog's URL
}

// render writes a page with the headers every gateway page carries.
func (g *Gateway) render(w http.ResponseWriter, r *http.Request, status int, name string, d pageData) {
	d.CSS, d.JS, d.Art = cssURL, jsURL, artURL
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
