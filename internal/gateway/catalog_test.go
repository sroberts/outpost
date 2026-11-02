package gateway

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/supervisor"
)

// withServices replaces the harness's services and rebuilds the routes. Every
// service points at the echo backend.
func (h *harness) withServices(list ...supervisor.ServiceStatus) {
	h.t.Helper()
	port := h.backend.Listener.Addr().(*net.TCPAddr).Port
	for i := range list {
		if list[i].Port == 0 {
			list[i].Port = port
		}
	}
	fs := h.g.services.(*fakeServices)
	fs.mu.Lock()
	fs.list = list
	fs.mu.Unlock()
	h.g.refreshRoutes(context.Background())
}

// order reports where each needle first appears in haystack, so a test can
// check what's listed before what.
func before(b, first, second string) bool {
	i, j := strings.Index(b, first), strings.Index(b, second)
	return i >= 0 && j >= 0 && i < j
}

func TestCatalogListsRunningFirstAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "atlas", Status: "stopped", Desired: "running", Halted: true, RestartCount: 3},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Title: "Yarr", Description: "RSS reader"},
		supervisor.ServiceStatus{Name: "notes", Status: "stopped", Desired: "stopped"},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if !before(b, ">Yarr<", ">atlas<") || !before(b, ">Yarr<", ">notes<") {
		t.Errorf("running apps aren't listed first:\n%s", b)
	}
	for _, want := range []string{"RSS reader", "Stopped after 3 crashes", "Stopped on purpose", "1 running · 1 down · 1 stopped"} {
		if !strings.Contains(b, want) {
			t.Errorf("catalog lacks %q", want)
		}
	}
	if strings.Contains(b, "outpost status") {
		t.Error("a user who isn't an admin was shown a CLI command")
	}

	h.addUser(auth.User{Username: "root", Admin: true})
	if b := body(t, h.login(h.client(true), "root", "", "/")); !strings.Contains(b, "outpost status atlas") {
		t.Error("an admin isn't shown how to look closer at a down app")
	}
}

func TestHiddenAppsAreReachableButNotListed(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Hidden: true})
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "root", Admin: true})

	c := h.client(true)
	if b := body(t, h.login(c, "alice", "", "/")); strings.Contains(b, "yarr.outpost.test") {
		t.Error("a hidden app is listed for a user")
	}
	backendSaw(t, h.get(c, yarr+"/"))

	if b := body(t, h.login(h.client(true), "root", "", "/")); !strings.Contains(b, "yarr.outpost.test") || !strings.Contains(b, ">Hidden<") {
		t.Error("an admin doesn't see the hidden app, marked as hidden")
	}
}

func TestAllowListDecidesWhoGetsIn(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"alice"}},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Allow: []string{"group:family"}},
	)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob", Groups: []string{"family"}})
	h.addUser(auth.User{Username: "root", Admin: true})

	alice := h.client(true)
	b := body(t, h.login(alice, "alice", "", "/"))
	if !strings.Contains(b, "yarr.outpost.test") || strings.Contains(b, "wiki.outpost.test") {
		t.Errorf("alice's catalog should list yarr only:\n%s", b)
	}
	backendSaw(t, h.get(alice, yarr+"/"))
	resp := h.get(alice, wiki+"/")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body(t, resp), "No access to wiki") {
		t.Errorf("alice reaching wiki = %d; want the access-denied page", resp.StatusCode)
	}

	bob := h.client(true)
	h.login(bob, "bob", "", "/")
	backendSaw(t, h.get(bob, wiki+"/"))
	if resp := h.get(bob, yarr+"/"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bob reaching yarr = %d; want 403", resp.StatusCode)
	}

	root := h.client(true)
	h.login(root, "root", "", "/")
	backendSaw(t, h.get(root, yarr+"/"))
	backendSaw(t, h.get(root, wiki+"/"))
}

// An app session made before the allow list changed doesn't outlive it.
func TestNarrowedAllowListEndsAccess(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")
	backendSaw(t, h.get(c, yarr+"/"))

	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}})
	if resp := h.get(c, yarr+"/"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("after the list changed, alice got %d; want 403", resp.StatusCode)
	}
}

func TestPublicPathsSkipLoginButNotIdentityStripping(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", PublicPaths: []string{"/webhook/*", "/feed.xml"}})
	anon := h.client(false)

	for _, p := range []string{"/webhook/github", "/feed.xml"} {
		got := backendSaw(t, h.get(anon, yarr+p, "X-Outpost-User", "root", "Cookie", "outpost=stolen; theme=dark"))
		if v := got.Header.Get("X-Outpost-User"); v != "" {
			t.Errorf("%s: forged identity reached the app: %q", p, v)
		}
		if c := got.Header.Get("Cookie"); strings.Contains(c, "outpost=") || !strings.Contains(c, "theme=dark") {
			t.Errorf("%s: app saw Cookie %q", p, c)
		}
	}
	for _, p := range []string{"/webhook/../admin", "/webhook%2F..%2Fadmin", "/feed.xml/x", "/admin", "/webhook"} {
		req, _ := http.NewRequest(http.MethodGet, yarr+"/", nil)
		req.URL.RawPath = ""
		req.URL.Opaque = "//yarr.outpost.test" + p
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s: %d without a session; want a redirect to sign in", p, resp.StatusCode)
		}
	}
}

func TestSubdomainAndHostnameMoveTheApp(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Subdomain: "rss"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", Hostname: "wiki.tail1234.ts.net"},
	)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	b := body(t, h.login(c, "alice", "", "/"))
	for _, want := range []string{`href="http://rss.outpost.test/"`, `href="http://wiki.tail1234.ts.net/"`} {
		if !strings.Contains(b, want) {
			t.Errorf("catalog lacks %s", want)
		}
	}
	if got := backendSaw(t, h.get(c, "http://rss.outpost.test/")); got.Host != "rss.outpost.test" {
		t.Errorf("app saw host %q", got.Host)
	}
	backendSaw(t, h.get(c, "http://wiki.tail1234.ts.net/"))
	if resp := h.get(c, yarr+"/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the old address still answers: %d", resp.StatusCode)
	}
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c, other := h.client(true), h.client(true)
	h.login(c, "alice", "", "/")
	h.login(other, "alice", "", "/")
	const next = "a much longer passphrase"

	good := url.Values{"action": {"password"}, "current": {pw}, "password": {next}, "confirm": {next}}
	resp := h.post(c, portal+"/account", portal, good)
	if resp.Request.URL.Path != "/account" || !strings.Contains(body(t, resp), "Password changed") {
		t.Fatalf("change ended at %s", resp.Request.URL)
	}
	if _, err := h.store.Authenticate("alice", next); err != nil {
		t.Errorf("new password doesn't work: %v", err)
	}
	if resp := h.get(other, portal+"/"); resp.Request.URL.Path != "/login" {
		t.Error("another device stayed signed in after the password changed")
	}
	if resp := h.get(c, portal+"/"); resp.Request.URL.Path != "/" {
		t.Error("the browser that changed the password was signed out")
	}
	if resp := h.post(h.client(true), portal+"/account", "http://evil.test", good); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin change = %d", resp.StatusCode)
	}
	// Last, since a wrong guess starts the same backoff a login does.
	wrong := url.Values{"action": {"password"}, "current": {"not it"}, "password": {next}, "confirm": {next}}
	if resp := h.post(c, portal+"/account", portal, wrong); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong current password = %d; want 401", resp.StatusCode)
	}
}

func TestSignOutEverywhere(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c, other := h.client(true), h.client(true)
	h.login(c, "alice", "", "/")
	h.login(other, "alice", "", "/")
	backendSaw(t, h.get(other, yarr+"/"))

	resp := h.post(c, portal+"/account", portal, url.Values{"action": {"everywhere"}})
	if !strings.Contains(body(t, resp), "signed out of every app") {
		t.Error("signing out everywhere doesn't say so")
	}
	if resp := h.get(other, yarr+"/"); resp.Request.URL.Host != "outpost.test" {
		t.Error("another device kept its app session")
	}
}

func TestErrorPagesAreThePortalsOwn(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Port: 1}) // nothing listens on port 1
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")

	cases := []struct {
		resp   *http.Response
		status int
		want   string
	}{
		{h.get(c, yarr+"/"), http.StatusBadGateway, "Back to apps"},
		{h.get(c, portal+"/logout"), http.StatusMethodNotAllowed, "Sign out from the button"},
		{h.post(h.client(false), yarr+"/api", yarr, url.Values{}), http.StatusUnauthorized, "Sign in first"},
		{h.get(c, portal+"/nope"), http.StatusNotFound, "Back to apps"},
	}
	for i, tc := range cases {
		if tc.resp.StatusCode != tc.status || !strings.HasPrefix(tc.resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("case %d: %d %s; want %d as a page", i, tc.resp.StatusCode, tc.resp.Header.Get("Content-Type"), tc.status)
			continue
		}
		if b := body(t, tc.resp); !strings.Contains(b, tc.want) {
			t.Errorf("case %d lacks %q", i, tc.want)
		}
	}
}

func TestDownAppPageSaysItIsDown(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Port: 1, Status: "stopped", Desired: "running", Halted: true, RestartCount: 2})
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")
	b := body(t, h.get(c, yarr+"/"))
	if !strings.Contains(b, "Stopped after 2 crashes") || strings.Contains(b, "in a moment") {
		t.Errorf("a halted app is described as slow:\n%s", b)
	}
	if strings.Contains(b, "outpost status") {
		t.Error("a user who isn't an admin was shown a CLI command")
	}
	h.addUser(auth.User{Username: "root", Admin: true})
	root := h.client(true)
	h.login(root, "root", "", "/")
	if b := body(t, h.get(root, yarr+"/")); !strings.Contains(b, "outpost status yarr") {
		t.Error("an admin's unavailable page lacks the command to look closer")
	}
}

func TestLogoutSaysSo(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")
	resp := h.post(c, portal+"/logout", portal, url.Values{})
	if b := body(t, resp); resp.Request.URL.Path != "/login" || !strings.Contains(b, "signed out of every app") {
		t.Errorf("logout ended at %s without saying so", resp.Request.URL)
	}
}

func TestPortalUsesItsName(t *testing.T) {
	h := newHarness(t, func(c *config.Gateway) { c.Name = "Lighthouse <3" })
	b := body(t, h.get(h.client(false), portal+"/login"))
	if !strings.Contains(b, "Lighthouse &lt;3") || !strings.Contains(b, "<title>Sign in · Lighthouse &lt;3</title>") {
		t.Errorf("the sign-in page doesn't carry the portal's name:\n%s", b)
	}
}

func TestFilterOnlyForALongList(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	if b := body(t, h.login(h.client(true), "alice", "", "/")); strings.Contains(b, jsPath) {
		t.Error("a short list loads the filter script")
	}
	var many []supervisor.ServiceStatus
	for _, n := range "abcdefghijklmn" {
		many = append(many, supervisor.ServiceStatus{Name: "app-" + string(n), Status: "running"})
	}
	h.withServices(many...)
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if !strings.Contains(b, `src="`+jsPath+`?v=`) || !strings.Contains(b, `id="filter"`) {
		t.Error("a long list has no filter")
	}
	resp := h.get(h.client(false), portal+jsPath)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("filter script = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestExpiredLinkOffersToContinue(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Title: "Yarr"})
	b := body(t, h.get(h.client(false), yarr+"/.outpost/callback?code=bogus"))
	if !strings.Contains(b, "Continue to Yarr") || strings.Contains(b, "Sign in again") {
		t.Errorf("expired link page:\n%s", b)
	}
}
