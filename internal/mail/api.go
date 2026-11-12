package mail

import (
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxRequest bounds what an app sends: a message, as JSON.
const maxRequest = maxSendBody + 64<<10

// feedEntries is how many messages a feed lists.
const feedEntries = 50

// ServeHTTP is the endpoint apps call, and the feeds feed readers fetch.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if rest, ok := strings.CutPrefix(r.URL.Path, "/feeds/"); ok {
		s.serveFeed(w, r, rest)
		return
	}
	service, ok := s.serviceFor(appToken(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "this endpoint takes the token Outpost gives each app, in OUTPOST_MAIL_TOKEN")
		return
	}
	switch path := r.URL.Path; {
	case path == "/v1/me":
		if !method(w, r, http.MethodGet) {
			return
		}
		s.me(w, service)
	case path == "/v1/messages":
		if !method(w, r, http.MethodGet) {
			return
		}
		s.list(w, r, service)
	case strings.HasPrefix(path, "/v1/messages/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(path, "/v1/messages/"), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusNotFound, "no such message")
			return
		}
		switch r.Method {
		case http.MethodGet:
			m, err := s.store.get(service, id)
			if err != nil {
				writeError(w, http.StatusNotFound, "no such message")
				return
			}
			writeJSON(w, http.StatusOK, m)
		case http.MethodDelete:
			if err := s.store.remove(service, id); err != nil {
				writeError(w, http.StatusNotFound, "no such message")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, http.StatusMethodNotAllowed, "use GET or DELETE")
		}
	case path == "/v1/send":
		if !method(w, r, http.MethodPost) {
			return
		}
		s.serveSend(w, r, service)
	default:
		writeError(w, http.StatusNotFound, "Outpost serves /v1/me, /v1/messages, /v1/messages/{id} and /v1/send")
	}
}

func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	writeError(w, http.StatusMethodNotAllowed, "use "+want)
	return false
}

// appToken is the token an app sent, as a bearer token.
func appToken(r *http.Request) string {
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(tok)
	}
	return ""
}

func (s *Service) me(w http.ResponseWriter, service string) {
	st := s.Status()
	writeJSON(w, http.StatusOK, struct {
		Ready     bool    `json:"ready"`
		Send      bool    `json:"send"`
		Addresses []Route `json:"addresses"`
	}{st.Ready(), s.store.canSend(service), nonNil(s.Routes(service))})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Service) list(w http.ResponseWriter, r *http.Request, service string) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	msgs, err := s.store.list(service, after, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't read the mail")
		return
	}
	next := after
	if len(msgs) > 0 {
		next = msgs[len(msgs)-1].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Messages []Message `json:"messages"`
		Next     int64     `json:"next"`
	}{nonNil(msgs), next})
}

func (s *Service) serveSend(w http.ResponseWriter, r *http.Request, service string) {
	var o Outgoing
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		writeError(w, http.StatusBadRequest, "the body has to be a JSON message: "+err.Error())
		return
	}
	id, err := s.Send(r.Context(), service, o)
	var br badRequest
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, struct {
			MessageID string `json:"message_id"`
		}{id})
	case errors.As(err, &br):
		writeError(w, http.StatusBadRequest, br.msg)
	case errors.Is(err, ErrNotAllowed):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrLimit):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the app hung up; nothing to tell it
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}

// feedURL is route's feed. The link carries its own key, since a feed
// reader can't send a token.
func (s *Service) feedURL(route string) string {
	return s.URL() + "/feeds/" + route + ".atom?key=" + s.store.feedSig(route)
}

// serveFeed answers /feeds/<route>.atom, the feed, and /feeds/<route>/<id>,
// one message as its sender wrote it, which each entry links to.
func (s *Service) serveFeed(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	route, id, one := strings.Cut(rest, "/")
	if !one {
		var ok bool
		if route, ok = strings.CutSuffix(rest, ".atom"); !ok {
			http.NotFound(w, r)
			return
		}
	}
	key := r.URL.Query().Get("key")
	if !validRoute(route) || subtle.ConstantTimeCompare([]byte(key), []byte(s.store.feedSig(route))) != 1 {
		http.NotFound(w, r)
		return
	}
	owner, ok := s.owner(route)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if one {
		s.serveOne(w, r, route, id)
		return
	}
	msgs, err := s.store.latest(owner, route, feedEntries)
	if err != nil {
		http.Error(w, "couldn't read the mail", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	_, _ = w.Write(s.atom(route, key, msgs)) // the reader hung up; nothing to tell it
}

// serveOne shows one message's HTML, or its text, sandboxed so nothing in
// it runs.
func (s *Service) serveOne(w http.ResponseWriter, r *http.Request, route, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	owner, _ := s.owner(route)
	m, err := s.store.get(owner, id)
	if err != nil || m.Route != route {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src * data:; style-src 'unsafe-inline' *; font-src *")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if m.HTML != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(m.HTML)) // the reader hung up
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(m.Text)) // the reader hung up
}

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Link    atomLink    `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
}

type atomEntry struct {
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  atomAuthor  `xml:"author"`
	Link    atomLink    `xml:"link"`
	Content atomContent `xml:"content"`
}

type atomAuthor struct {
	Name  string `xml:"name"`
	Email string `xml:"email,omitempty"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

// atom is route's feed: its newest messages, newest first. Its title is
// the alias's note, or else who the newest message is from, which is what a
// reader shows as the feed's name when it's added.
func (s *Service) atom(route, key string, msgs []Message) []byte {
	title := ""
	if a, ok := s.store.aliasFor(route); ok {
		title = a.Note
	}
	if title == "" && len(msgs) > 0 {
		title = firstNonEmpty(msgs[0].From.Name, msgs[0].From.Address)
	}
	if title == "" {
		title = "Mail to +" + route
	}
	self := s.feedURL(route)
	f := atomFeed{
		ID:      "urn:outpost:mail:" + route,
		Title:   title,
		Updated: time.Unix(0, 0).UTC().Format(time.RFC3339),
		Link:    atomLink{Rel: "self", Href: self},
	}
	if len(msgs) > 0 {
		f.Updated = msgs[0].Received.Format(time.RFC3339)
	}
	for _, m := range msgs {
		content := atomContent{Type: "html", Body: m.HTML}
		if m.HTML == "" {
			content = atomContent{Type: "text", Body: m.Text}
		}
		f.Entries = append(f.Entries, atomEntry{
			ID:      "urn:outpost:mail:" + route + ":" + strconv.FormatInt(m.ID, 10),
			Title:   firstNonEmpty(m.Subject, "(no subject)"),
			Updated: m.Date.Format(time.RFC3339),
			Author:  atomAuthor{Name: firstNonEmpty(m.From.Name, m.From.Address), Email: m.From.Address},
			Link:    atomLink{Rel: "alternate", Href: s.URL() + "/feeds/" + route + "/" + strconv.FormatInt(m.ID, 10) + "?key=" + url.QueryEscape(key)},
			Content: content,
		})
	}
	out, err := xml.MarshalIndent(f, "", "  ")
	if err != nil {
		return []byte(xml.Header)
	}
	return append([]byte(xml.Header), out...)
}
