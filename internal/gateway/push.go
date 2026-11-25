package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/push"
)

// Push is the notification service: the devices people have turned
// notifications on for, the apps each has muted, and sending. *push.Service
// has it.
type Push interface {
	PublicKey() string
	Subscribe(username, signIn, endpoint, p256dh, auth, userAgent string) error
	Unsubscribe(username, endpoint string) error
	Devices(username string) ([]push.Device, error)
	Forget(username, id string) (bool, error)
	Muted(username string) (map[string]bool, error)
	SetMuted(username, service string, muted bool) error
	Send(from string, to []string, n push.Notification) (int, error)
	Status() push.Stats
}

const (
	// pushSubPath is where a page or the service worker hands over, or takes
	// back, this device's subscription.
	pushSubPath = "/.outpost/push/subscription"
	// pushOpenPath is where tapping an app's notification lands: on the
	// portal, the subscription's own origin, which sends it on to the app.
	pushOpenPath = "/.outpost/push/open"
	// manifestPath and swPath sit at the root: a service worker's scope is
	// its own directory, and the portal is "/".
	manifestPath = "/manifest.webmanifest"
	swPath       = "/sw.js"
	pushJSPath   = "/.outpost/static/push.js"
	icon180Path  = "/.outpost/static/icon-180.png"
	icon512Path  = "/.outpost/static/icon-512.png"
)

var (
	pushJSURL  = versioned(pushJSPath, "static/push.js")
	icon180URL = versioned(icon180Path, "static/icon-180.png")
	icon512URL = versioned(icon512Path, "static/icon-512.png")
)

// outpostSender is how Outpost's own notifications are named in the mute
// form, where a field can't be empty.
const outpostSender = ".outpost"

// PushGate is what the push service asks the gateway before sending.
func (g *Gateway) PushGate() push.Gate { return pushGate{g} }

type pushGate struct{ g *Gateway }

// MayReceive is true for an enabled user who can open the app, and, for
// Outpost's own, for any enabled user: who those are for is the sender's
// choice, such as the admins.
func (p pushGate) MayReceive(service, username string) bool {
	u, err := p.g.auth.GetUser(username)
	if err != nil || u.Disabled {
		return false
	}
	if service == push.Outpost {
		return true
	}
	rt, ok := p.g.routes.get(service)
	return ok && allowed(u, rt)
}

func (p pushGate) SignIns(username string) (map[string]bool, error) {
	list, err := p.g.auth.SignIns(username)
	if errors.Is(err, auth.ErrNoSuchUser) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, si := range list {
		out[si.ID] = true
	}
	return out, nil
}

// Navigate is on the portal for every notification. Declarative Web Push
// opens only a URL on the subscription's own origin, which is the portal's,
// so an app's goes by way of pushOpenPath.
func (p pushGate) Navigate(service, path string) string {
	if service == push.Outpost {
		return p.g.portalOrigin + path
	}
	return p.g.portalOrigin + pushOpenPath + "?" + url.Values{"app": {service}, "path": {path}}.Encode()
}

// pushOpen sends a tapped notification on to its app. Only to an app's own
// origin, with a path on it, so the link can't be made to go anywhere else.
func (g *Gateway) pushOpen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name, path := q.Get("app"), q.Get("path")
	if path == "" {
		path = "/"
	}
	rt, ok := g.routes.get(name)
	if !ok || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n\t") {
		g.message(w, r, http.StatusNotFound, "Nothing here",
			"That notification's app isn't here any more.", g.portalLink(), "Back to apps")
		return
	}
	http.Redirect(w, r, g.appOrigin(rt.Name)+path, http.StatusSeeOther)
}

// serveManifest makes the portal installable: Add to Home Screen on iOS,
// which is the only way an iPhone gets web notifications, and Add to Dock on
// a Mac. A browser asks for it without cookies, so it is public.
func (g *Gateway) serveManifest(w http.ResponseWriter, r *http.Request) {
	name := g.cfg.PortalName()
	body, err := json.Marshal(map[string]any{
		"name":             name,
		"short_name":       name,
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#141a21",
		"theme_color":      "#141a21",
		"icons": []map[string]string{
			{"src": icon180URL, "sizes": "180x180", "type": "image/png"},
			{"src": icon512URL, "sizes": "512x512", "type": "image/png"},
		},
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body) // a client that hung up has nothing to be told
}

// pushSubRequest is a browser's PushSubscription, as its toJSON gives it.
type pushSubRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	// OldEndpoint is one this browser held before, which it replaces.
	OldEndpoint string `json:"old_endpoint"`
	// ExpirationTime comes with toJSON; push services set none worth acting on.
	ExpirationTime any `json:"expirationTime"`
}

// pushSubscription takes this device's subscription (POST) or forgets it
// (DELETE). It's a portal session's alone: an app's session, or a token,
// never reaches here.
func (g *Gateway) pushSubscription(w http.ResponseWriter, r *http.Request) {
	jsonError := func(status int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg}) // the page has gone
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		jsonError(http.StatusMethodNotAllowed, "use POST or DELETE")
		return
	}
	if !g.sameOrigin(r) {
		jsonError(http.StatusForbidden, "that came from another site")
		return
	}
	// JSON, so a plain form on another site couldn't send it even if it got
	// past the origin check.
	if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
		jsonError(http.StatusUnsupportedMediaType, "send JSON")
		return
	}
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		jsonError(http.StatusUnauthorized, "sign in first")
		return
	}
	var req pushSubRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		jsonError(http.StatusBadRequest, "the body has to be a push subscription as JSON")
		return
	}
	if r.Method == http.MethodDelete {
		if err := g.push.Unsubscribe(u.Username, req.Endpoint); err != nil {
			g.logf("gateway: turn notifications off: %v", err)
			jsonError(http.StatusInternalServerError, "couldn't turn notifications off")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := g.push.Subscribe(u.Username, auth.HashToken(tok), req.Endpoint, req.Keys.P256dh, req.Keys.Auth, r.UserAgent())
	switch {
	case push.IsBadRequest(err):
		jsonError(http.StatusBadRequest, err.Error())
		return
	case err != nil:
		g.logf("gateway: turn notifications on: %v", err)
		jsonError(http.StatusInternalServerError, "couldn't turn notifications on")
		return
	}
	if req.OldEndpoint != "" && req.OldEndpoint != req.Endpoint {
		if err := g.push.Unsubscribe(u.Username, req.OldEndpoint); err != nil {
			g.logf("gateway: forget a replaced subscription: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// notifyView is the Notifications section of the account page.
type notifyView struct {
	Key     string // the VAPID public key the page subscribes with
	Devices []notifyDevice
	// Here says this sign-in has a device, so the page knows the server
	// has its subscription.
	Here   bool
	Apps   []notifyApp
	Notice string
	Error  string
	Counts string // deliveries since Outpost started, for an admin
}

type notifyDevice struct {
	ID       string
	Device   string
	Platform string
	Since    string
	Last     string
	Failing  bool
	This     bool
}

type notifyApp struct {
	Value, Title string
	On           bool
}

// platformNames are the push services, as people know them.
var platformNames = map[string]string{"apple": "Apple", "fcm": "Google", "mozilla": "Mozilla", "windows": "Microsoft"}

// notifyData builds the section for u. A failure leaves the section out
// rather than the page.
func (g *Gateway) notifyData(r *http.Request, u auth.User) *notifyView {
	if g.push == nil {
		return nil
	}
	v := &notifyView{Key: g.push.PublicKey()}
	devs, err := g.push.Devices(u.Username)
	if err != nil {
		g.logf("gateway: list devices: %v", err)
		return nil
	}
	this := ""
	if c, err := r.Cookie(g.cookieName); err == nil {
		this = auth.HashToken(c.Value)
	}
	now := g.now()
	for _, d := range devs {
		nd := notifyDevice{ID: d.ID, Device: DescribeAgent(d.UserAgent), Platform: platformNames[d.Platform],
			Since: signedInAt(now, d.CreatedAt), Failing: d.Failing, This: d.SignIn == this}
		if !d.LastSuccess.IsZero() {
			nd.Last = agoAt(now, d.LastSuccess)
		}
		if nd.This {
			v.Here = true
		}
		v.Devices = append(v.Devices, nd)
	}
	muted, err := g.push.Muted(u.Username)
	if err != nil {
		g.logf("gateway: list muted apps: %v", err)
		return nil
	}
	v.Apps = append(v.Apps, notifyApp{Value: outpostSender, Title: g.cfg.PortalName() + " itself", On: !muted[push.Outpost]})
	var apps []notifyApp
	for _, rt := range g.routes.all() {
		if allowed(u, rt) {
			apps = append(apps, notifyApp{Value: rt.Name, Title: firstNonEmpty(rt.Title, titleCase(rt.Name)), On: !muted[rt.Name]})
		}
	}
	slices.SortStableFunc(apps, func(a, b notifyApp) int {
		return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	})
	v.Apps = append(v.Apps, apps...)
	if u.Admin {
		st := g.push.Status()
		v.Counts = fmt.Sprintf("Since Outpost started: %s delivered, %s failed", count(st.Delivered, "notification"), count(st.Failed, "delivery"))
		if st.Gone > 0 {
			v.Counts += fmt.Sprintf(", %s ended by the device", count(st.Gone, "subscription"))
		}
		v.Counts += "."
	}
	switch r.URL.Query().Get("push") {
	case "sent":
		v.Notice = "Sent. It should arrive on each device below in a few seconds."
	case "none":
		v.Notice = "There's no device to send to: turn notifications on for one first."
	case "forgot":
		v.Notice = "That device won't get notifications any more."
	case "saved":
		v.Notice = "Saved which apps can notify you."
	}
	return v
}

// pushSubmit handles the Notifications section's forms.
func (g *Gateway) pushSubmit(w http.ResponseWriter, r *http.Request, u auth.User) {
	if g.push == nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	back := func(done string) {
		http.Redirect(w, r, "/account?push="+done+"#notify", http.StatusSeeOther)
	}
	fail := func(err error) {
		g.logf("gateway: notifications: %v", err)
		g.message(w, r, http.StatusInternalServerError, "That didn't go through", "Something went wrong. Try again.", "/account#notify", "Back to account")
	}
	switch r.PostForm.Get("action") {
	case "push-test":
		n, err := g.push.Send(push.Outpost, []string{u.Username}, push.Notification{
			Title: "Test from " + g.cfg.PortalName(),
			Body:  "Notifications reach this device.",
			Path:  "/account",
			Tag:   "outpost-test",
		})
		switch {
		case err != nil:
			fail(err)
		case n == 0:
			back("none")
		default:
			back("sent")
		}
	case "push-forget":
		if _, err := g.push.Forget(u.Username, r.PostForm.Get("id")); err != nil {
			fail(err)
			return
		}
		back("forgot")
	case "push-apps":
		on := map[string]bool{}
		for _, v := range r.PostForm["on"] {
			on[v] = true
		}
		// Every sender the form listed, so one it didn't (an app added
		// since the page loaded) is left as it was.
		senders := []string{outpostSender}
		for _, rt := range g.routes.all() {
			if allowed(u, rt) {
				senders = append(senders, rt.Name)
			}
		}
		listed := map[string]bool{}
		for _, v := range r.PostForm["listed"] {
			listed[v] = true
		}
		for _, name := range senders {
			if !listed[name] {
				continue
			}
			svc := name
			if name == outpostSender {
				svc = push.Outpost
			}
			if err := g.push.SetMuted(u.Username, svc, !on[name]); err != nil {
				fail(err)
				return
			}
		}
		back("saved")
	default:
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	}
}
