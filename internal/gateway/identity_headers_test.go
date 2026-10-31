package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// The proxy drops every header a client names in Connection before it
// forwards, so identity set on the inbound request could be taken back by
// naming it there. It has to be set on the outgoing copy, after that.
func TestClientCannotDropIdentityWithConnection(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", Groups: []string{"family"}})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	got := backendSaw(t, h.get(c, yarr+"/x",
		"Connection", "X-Outpost-User, X-Outpost-Identity, X-Outpost-Groups, X-Forwarded-For"))
	if u := got.Header.Get("X-Outpost-User"); u != "alice" {
		t.Errorf("X-Outpost-User = %q; want alice", u)
	}
	if g := got.Header.Get("X-Outpost-Groups"); g != "family" {
		t.Errorf("X-Outpost-Groups = %q; want family", g)
	}
	if got.Header.Get("X-Outpost-Identity") == "" {
		t.Error("X-Outpost-Identity was dropped")
	}
	if got.Header.Get("X-Forwarded-For") == "" {
		t.Error("X-Forwarded-For was dropped")
	}
}

// A header named X_Outpost_User or X.Outpost.User is a different header to
// Go and the same one to a CGI-style app, which folds both into
// HTTP_X_OUTPOST_USER. Every spelling has to go, signed in or not.
func TestLookAlikeIdentityHeadersAreStripped(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", PublicPaths: []string{"/webhook/*"}})
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	forged := []string{"X_Outpost_User", "X.Outpost.User", "x_outpost-groups", "X-Outpost_Identity", "X~Outpost~User"}
	for _, rawURL := range []string{yarr + "/x", yarr + "/webhook/github"} {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range forged {
			// Set as given: Header.Set would not change these names, but
			// the point is that they reach the gateway exactly as sent.
			req.Header[name] = []string{"root"}
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := backendSaw(t, resp)
		for name, vs := range got.Header {
			// The gateway's own are the canonical names; anything else
			// that an app would fold into them came from the client.
			if isIdentityName(name) && !strings.HasPrefix(name, "X-Outpost-") {
				t.Errorf("%s: forged %s=%q reached the app", rawURL, name, vs)
			}
		}
		if strings.HasSuffix(rawURL, "/x") {
			if u := got.Header.Get("X-Outpost-User"); u != "alice" {
				t.Errorf("X-Outpost-User = %q; want alice", u)
			}
		} else if u := got.Header.Get("X-Outpost-User"); u != "" {
			t.Errorf("public path: X-Outpost-User = %q; want none", u)
		}
	}
}

func TestIsIdentityName(t *testing.T) {
	for name, want := range map[string]bool{
		"X-Outpost-User":     true,
		"x-outpost-identity": true,
		"X_Outpost_User":     true,
		"X.Outpost.Groups":   true,
		"X-Outpost_Email":    true,
		"XOutpostUser":       true,
		"X-Outpost-":         true,
		"X-Out-Post-User":    true, // deliberately broad: nothing legitimate is spelt like this
		"X-Forwarded-For":    false,
		"Outpost-Token":      false,
		"X-Outpos-User":      false,
		"Cookie":             false,
	} {
		if got := isIdentityName(name); got != want {
			t.Errorf("isIdentityName(%q) = %v; want %v", name, got, want)
		}
	}
}

// The other names an app might read a client address from are the
// gateway's own too, so an app written for nginx can't be handed a chosen one.
func TestClientAddressHeadersAreTheGatewaysOwn(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	got := backendSaw(t, h.get(c, yarr+"/x",
		"X-Real-IP", "203.0.113.9",
		"True-Client-IP", "203.0.113.9",
		"X-Client-IP", "203.0.113.9"))
	if ip := got.Header.Get("X-Real-IP"); ip == "" || ip == "203.0.113.9" {
		t.Errorf("X-Real-IP = %q; want the gateway's view of the client", ip)
	}
	if ip := got.Header.Get("X-Real-IP"); ip != got.Header.Get("X-Forwarded-For") {
		t.Errorf("X-Real-IP = %q but X-Forwarded-For = %q", ip, got.Header.Get("X-Forwarded-For"))
	}
	for _, name := range []string{"True-Client-IP", "X-Client-IP"} {
		if v := got.Header.Get(name); v != "" {
			t.Errorf("%s = %q reached the app", name, v)
		}
	}
}
