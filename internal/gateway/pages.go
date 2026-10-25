package gateway

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.html static/portal.css
var assets embed.FS

const cssPath = "/.outpost/static/portal.css"

// pages are the portal's templates, each parsed with the shared layout.
type pages map[string]*template.Template

func loadPages() (pages, error) {
	p := pages{}
	for _, name := range []string{"login", "setup", "portal", "message"} {
		t, err := template.ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
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
	App      string
	Next     string
	Username string

	Token       string
	MinPassword int

	Who   string
	Admin bool
	Apps  []appLink

	Message  string
	Link     string
	LinkText string
}

type appLink struct {
	Name   string
	URL    string
	Status string
	Up     bool
}

// render writes a page with the headers every gateway page carries.
func (g *Gateway) render(w http.ResponseWriter, r *http.Request, status int, name string, d pageData) {
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
	h.Set("Referrer-Policy", "no-referrer")
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

// message shows a one-line explanation, with an optional way on.
func (g *Gateway) message(w http.ResponseWriter, r *http.Request, status int, title, msg, link, linkText string) {
	g.render(w, r, status, "message", pageData{Title: title, Message: msg, Link: link, LinkText: linkText})
}
