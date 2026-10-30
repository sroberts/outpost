package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/supervisor"
)

// pat makes a token for a user, who must exist.
func (h *harness) pat(user string, apps ...string) string {
	h.t.Helper()
	secret, _, err := h.store.CreateAccessToken(user, "test", apps, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return secret
}

// send makes a request with exactly these headers, repeats included, which
// the harness's get can't do.
func (h *harness) send(method, rawURL string, hdr http.Header) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := h.client(false).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

// assertRefused checks the plain 401: never a redirect and never a page, and
// proof the request stopped at the gateway, since the echo backend answers in
// JSON.
func assertRefused(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("redirected to %s", loc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; want plain text", ct)
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("set cookies: %v", resp.Cookies())
	}
}

// Both ways of carrying a token reach the app as the user, and the token
// doesn't reach the app at all.
func TestAccessToken_CarriersActAsTheUserAndAreStripped(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", DisplayName: "Alice A", Email: "alice@example.com", Groups: []string{"family", "kids"}})
	tok := h.pat("alice")

	for name, hdr := range map[string]http.Header{
		"bearer":         {"Authorization": {"Bearer " + tok}},
		"lower case":     {"Authorization": {"bearer " + tok}},
		"tab":            {"Authorization": {"Bearer\t" + tok}},
		"extra space":    {"Authorization": {"Bearer    " + tok}},
		"dedicated":      {"Outpost-Token": {tok}},
		"dedicated case": {"OUTPOST-TOKEN": {tok}},
	} {
		t.Run(name, func(t *testing.T) {
			resp := h.send(http.MethodGet, yarr+"/some/path?q=1", hdr)
			s := backendSaw(t, resp)
			if s.Path != "/some/path?q=1" {
				t.Errorf("path = %q", s.Path)
			}
			if got := s.Header.Get("X-Outpost-User"); got != "alice" {
				t.Errorf("X-Outpost-User = %q", got)
			}
			if got := s.Header.Get("X-Outpost-Groups"); got != "family,kids" {
				t.Errorf("X-Outpost-Groups = %q", got)
			}
			if got := s.Header.Get("Authorization"); got != "" {
				t.Errorf("the app was sent Authorization: %q", got)
			}
			if got := s.Header.Get("Outpost-Token"); got != "" {
				t.Errorf("the app was sent Outpost-Token: %q", got)
			}
			raw, err := json.Marshal(s.Header)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), tok) {
				t.Errorf("the token reached the app: %s", raw)
			}
			if len(resp.Cookies()) != 0 {
				t.Errorf("a token request set cookies: %v", resp.Cookies())
			}
		})
	}
	// Nothing about it signed anyone in.
	if list, err := h.store.SignIns("alice"); err != nil || len(list) != 0 {
		t.Errorf("sign-ins after token use = %v, %v", list, err)
	}
}

// identityClaimsOf reads a token's claims; verifying them is TestIdentityToken's
// business.
func identityClaimsOf(t *testing.T, tok string) identityClaims {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c identityClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// An app can't tell a token from a browser session.
func TestAccessToken_SameIdentityAsABrowser(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", DisplayName: "Alice A", Email: "alice@example.com", Groups: []string{"family"}})
	c := h.client(true)
	browser := backendSaw(t, h.login(c, "alice", "yarr", "/"))
	script := backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(h.pat("alice"))))

	for _, name := range []string{"X-Outpost-User", "X-Outpost-Name", "X-Outpost-Email", "X-Outpost-Groups"} {
		if got, want := script.Header.Get(name), browser.Header.Get(name); got != want || want == "" {
			t.Errorf("%s = %q; browser got %q", name, got, want)
		}
	}
	got, want := identityClaimsOf(t, script.Header.Get("X-Outpost-Identity")), identityClaimsOf(t, browser.Header.Get("X-Outpost-Identity"))
	if got.Sub != want.Sub || got.Aud != want.Aud || got.Iss != want.Iss || got.Email != want.Email || got.Name != want.Name {
		t.Errorf("identity token = %+v; browser got %+v", got, want)
	}
}

// Nothing in the request gets to say who it is, with a token or alongside one.
func TestAccessToken_CantChooseIdentity(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob", Admin: true})
	tok := h.pat("alice")

	resp := h.get(h.client(false), yarr+"/",
		"Authorization", "Bearer "+tok,
		"X-Outpost-User", "bob",
		"X-Outpost-Groups", "admins",
		"X-Outpost-Identity", "forged",
		"X-Outpost-Anything", "else",
	)
	s := backendSaw(t, resp)
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	if got := s.Header.Get("X-Outpost-Groups"); got != "" {
		t.Errorf("X-Outpost-Groups = %q", got)
	}
	if got := s.Header.Get("X-Outpost-Anything"); got != "" {
		t.Errorf("X-Outpost-Anything survived: %q", got)
	}
	if got := s.Header.Get("X-Outpost-Identity"); got == "forged" || identityClaimsOf(t, got).Sub != "alice" {
		t.Errorf("X-Outpost-Identity = %q", got)
	}

	// The gateway strips X-Outpost-* before anything looks at a token, so one
	// named that way is no token at all.
	resp = h.get(h.client(false), yarr+"/", "X-Outpost-Token", tok)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("X-Outpost-Token got %d; want the login redirect for no credential", resp.StatusCode)
	}
}

func TestAccessToken_CookieIsRemovedAndOthersKept(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	s := backendSaw(t, h.get(h.client(false), yarr+"/",
		"Authorization", "Bearer "+h.pat("alice"),
		"Cookie", "theirs=1; outpost=some-session; other=2"))
	if got := s.Header.Get("Cookie"); got != "theirs=1; other=2" {
		t.Errorf("Cookie = %q", got)
	}
}

// A bearer token without the prefix is the app's, as is every other scheme, and
// the request is treated as if the gateway never looked.
func TestAccessToken_AppsOwnAuthorizationIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})

	for _, v := range []string{"Bearer app-credential", "Bearer OPAT_upper", "Basic YWxpY2U6cHc=", "Bearer", "opat_no_scheme"} {
		// No session: off to log in, exactly as before.
		resp := h.send(http.MethodGet, yarr+"/", http.Header{"Authorization": {v}})
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%q with no session got %d; want the login redirect", v, resp.StatusCode)
		}
	}

	// With a session it goes through, untouched.
	c := h.client(true)
	if resp := h.login(c, "alice", "yarr", "/"); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	s := backendSaw(t, h.get(c, yarr+"/", "Authorization", "Bearer app-credential"))
	if got := s.Header.Get("Authorization"); got != "Bearer app-credential" {
		t.Errorf("Authorization = %q", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}

	// A client needing both: the app's in Authorization, Outpost's beside it.
	s = backendSaw(t, h.get(h.client(false), yarr+"/",
		"Authorization", "Bearer app-credential",
		"Outpost-Token", h.pat("alice")))
	if got := s.Header.Get("Authorization"); got != "Bearer app-credential" {
		t.Errorf("Authorization = %q", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	if got := s.Header.Get("Outpost-Token"); got != "" {
		t.Errorf("Outpost-Token reached the app: %q", got)
	}

	// The portal has no use for it either, and isn't bothered by it.
	if resp := h.send(http.MethodGet, portal+"/login", http.Header{"Authorization": {"Bearer app-credential"}}); resp.StatusCode != http.StatusOK {
		t.Errorf("portal with an app's bearer got %d", resp.StatusCode)
	}
}

// A second Authorization line goes to the app like the first, so it can't be
// where a token hides from the gateway.
func TestAccessToken_RepeatedHeaders(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	tok := h.pat("alice")

	s := backendSaw(t, h.send(http.MethodGet, yarr+"/", http.Header{
		"Authorization": {"Bearer app-credential", "Bearer " + tok},
	}))
	if got := s.Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer app-credential" {
		t.Errorf("Authorization = %q", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}

	// Two tokens, even both good: there's no right one to pick.
	other := h.pat("alice")
	for name, hdr := range map[string]http.Header{
		"two carriers":     {"Authorization": {"Bearer " + tok}, "Outpost-Token": {other}},
		"same in both":     {"Authorization": {"Bearer " + tok}, "Outpost-Token": {tok}},
		"two dedicated":    {"Outpost-Token": {tok, other}},
		"two bearers":      {"Authorization": {"Bearer " + tok, "Bearer " + other}},
		"comma in one":     {"Outpost-Token": {tok + ", " + other}},
		"stray extra word": {"Authorization": {"Bearer " + tok + " extra"}},
	} {
		t.Run(name, func(t *testing.T) { assertRefused(t, h.send(http.MethodGet, yarr+"/", hdr)) })
		h.g.tokenThrottle.reset("ip:127.0.0.1")
	}
}

func TestAccessToken_RefusedOnThePortal(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", Admin: true})
	tok := h.pat("alice")

	for _, path := range []string{"/", "/login", "/logout", "/account", "/updates", "/setup", "/nothing", jwksPath, cssPath} {
		for name, hdr := range map[string]http.Header{"bearer": bearer(tok), "dedicated": {"Outpost-Token": {tok}}} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(fmt.Sprintf("%s %s %s", method, path, name), func(t *testing.T) {
					assertRefused(t, h.send(method, portal+path, hdr))
				})
			}
		}
	}
	if list, err := h.store.SignIns("alice"); err != nil || len(list) != 0 {
		t.Errorf("sign-ins after portal attempts = %v, %v", list, err)
	}
}

// The gateway's own endpoints on an app host belong to browser sessions, and
// the callback would mint one.
func TestAccessToken_RefusedOnReservedPaths(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	tok := h.pat("alice")
	for _, path := range []string{"/.outpost/callback?code=x", "/.outpost/logout", cssPath, "/.outpost/other"} {
		assertRefused(t, h.send(http.MethodGet, yarr+path, bearer(tok)))
	}
	if list, err := h.store.SignIns("alice"); err != nil || len(list) != 0 {
		t.Errorf("sign-ins = %v, %v", list, err)
	}
}

// Every way a token can have stopped working looks the same from outside.
func TestAccessToken_DeadTokensAreRejected(t *testing.T) {
	cases := []struct {
		name string
		kill func(h *harness, tok string, id int64)
	}{
		{"revoked", func(h *harness, _ string, id int64) {
			if _, err := h.store.RevokeAccessToken(id); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"disabled", func(h *harness, _ string, _ int64) {
			if err := h.store.SetDisabled("alice", true); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"re-enabled", func(h *harness, _ string, _ int64) {
			if err := h.store.SetDisabled("alice", true); err != nil {
				h.t.Fatal(err)
			}
			if err := h.store.SetDisabled("alice", false); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"deleted", func(h *harness, _ string, _ int64) {
			if err := h.store.RemoveUser("alice"); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"logged out", func(h *harness, _ string, _ int64) {
			if _, err := h.store.RevokeUserSessions("alice"); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"password changed", func(h *harness, _ string, _ int64) {
			if err := h.store.SetPassword("alice", "a whole new password"); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addUser(auth.User{Username: "alice"})
			tok, created, err := h.store.CreateAccessToken("alice", "", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(tok))) // works until then
			tc.kill(h, tok, created.ID)
			assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))
			assertRefused(t, h.send(http.MethodGet, yarr+"/", http.Header{"Outpost-Token": {tok}}))
		})
	}

	t.Run("expired", func(t *testing.T) {
		h := newHarness(t)
		h.addUser(auth.User{Username: "alice"})
		// Expiry has a resolution of a second, so this one is over at once.
		tok, _, err := h.store.CreateAccessToken("alice", "", nil, time.Nanosecond)
		if err != nil {
			t.Fatal(err)
		}
		assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))
	})

	t.Run("never issued", func(t *testing.T) {
		h := newHarness(t)
		h.addUser(auth.User{Username: "alice"})
		for _, hdr := range []http.Header{
			bearer(auth.AccessTokenPrefix + "made-up"),
			bearer(auth.AccessTokenPrefix),
			{"Outpost-Token": {auth.AccessTokenPrefix + "made-up"}},
			{"Outpost-Token": {"no-prefix"}}, // the dedicated header has no other meaning
			{"Outpost-Token": {""}},
		} {
			assertRefused(t, h.send(http.MethodGet, yarr+"/", hdr))
		}
	})

	t.Run("a session is no token and a token is no session", func(t *testing.T) {
		h := newHarness(t)
		h.addUser(auth.User{Username: "alice"})
		session, err := h.store.CreateSession("alice", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		assertRefused(t, h.send(http.MethodGet, yarr+"/", http.Header{"Outpost-Token": {session}}))
		resp := h.send(http.MethodGet, yarr+"/", http.Header{"Cookie": {"outpost=" + h.pat("alice")}})
		if resp.StatusCode != http.StatusFound {
			t.Errorf("a token as a cookie got %d; want the login redirect", resp.StatusCode)
		}
	})
}

func TestAccessToken_ScopeLimitsApps(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running"},
	)
	h.addUser(auth.User{Username: "alice", Admin: true}) // admin: only the scope stands in the way
	tok := h.pat("alice", "yarr")

	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))
	assertRefused(t, h.send(http.MethodGet, wiki+"/", bearer(tok)))

	// An unscoped one reaches both.
	all := h.pat("alice")
	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(all)))
	backendSaw(t, h.send(http.MethodGet, wiki+"/", bearer(all)))
}

// The allow list applies on top of a token's scope, naming an app in the scope
// grants nothing, and it is read at each request.
func TestAccessToken_AllowListStillDecides(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}})
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	f := h.recording()

	for _, tok := range []string{h.pat("alice"), h.pat("alice", "yarr")} {
		resp := h.send(http.MethodGet, yarr+"/", bearer(tok))
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d; want 403", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q; want plain text, not the browser's page", ct)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("redirected to %s", loc)
		}
	}
	got := f.wait(t, 1)
	if got[0].kind != EventDenied || got[0].message != "access denied for alice to yarr" {
		t.Errorf("feed = %+v", got)
	}

	bobs := h.pat("bob")
	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(bobs)))
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"alice"}})
	if resp := h.send(http.MethodGet, yarr+"/", bearer(bobs)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("narrowing the list left a token in: %d", resp.StatusCode)
	}

	// A group gained after the token was made counts.
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"group:family"}})
	if _, err := h.store.UpdateGroups("bob", []string{"family"}, nil); err != nil {
		t.Fatal(err)
	}
	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(bobs)))
}

func TestAccessToken_PublicPaths(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}, PublicPaths: []string{"/webhook/*"}})
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})

	// With nothing presented, as ever.
	s := backendSaw(t, h.send(http.MethodGet, yarr+"/webhook/x", http.Header{"Authorization": {"Bearer app-credential"}}))
	if got := s.Header.Get("Authorization"); got != "Bearer app-credential" {
		t.Errorf("Authorization = %q", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "" {
		t.Errorf("a public path got an identity: %q", got)
	}

	// A wrong token doesn't turn anonymous because the path happens to be public.
	assertRefused(t, h.send(http.MethodGet, yarr+"/webhook/x", bearer(auth.AccessTokenPrefix+"wrong")))
	assertRefused(t, h.send(http.MethodPost, yarr+"/webhook/x", http.Header{"Outpost-Token": {"wrong"}}))

	// Nor does one that's good but not for here.
	scoped := h.pat("bob", "wiki")
	assertRefused(t, h.send(http.MethodGet, yarr+"/webhook/x", bearer(scoped)))
	resp := h.send(http.MethodGet, yarr+"/webhook/x", bearer(h.pat("alice")))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token not on the allow list got %d; want 403", resp.StatusCode)
	}

	// A good one is judged, removed, and then the path works as it does for a
	// signed-in browser: no identity, so an app can go on assuming none there.
	tok := h.pat("bob")
	s = backendSaw(t, h.send(http.MethodGet, yarr+"/webhook/x", http.Header{
		"Authorization":  {"Bearer " + tok},
		"X-Outpost-User": {"alice"},
		"Cookie":         {"outpost=abc; theirs=1"},
	}))
	if got := s.Header.Get("Authorization"); got != "" {
		t.Errorf("the token reached the app: %q", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	if got := s.Header.Get("Cookie"); got != "theirs=1" {
		t.Errorf("Cookie = %q", got)
	}

	// A path that only looks public gets the identity an ordinary one does.
	s = backendSaw(t, h.send(http.MethodGet, yarr+"/webhook/%2e%2e/admin", bearer(tok)))
	if got := s.Header.Get("X-Outpost-User"); got != "bob" {
		t.Errorf("X-Outpost-User = %q", got)
	}
}

// A browser session in the same request doesn't rescue a bad token, and isn't
// used or extended by a good one.
func TestAccessToken_ATokenOverridesASession(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	c := h.client(true)
	if resp := h.login(c, "alice", "yarr", "/"); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}

	assertRefused(t, h.get(c, yarr+"/", "Authorization", "Bearer "+auth.AccessTokenPrefix+"wrong"))

	s := backendSaw(t, h.get(c, yarr+"/", "Authorization", "Bearer "+h.pat("bob")))
	if got := s.Header.Get("X-Outpost-User"); got != "bob" {
		t.Errorf("X-Outpost-User = %q; want the token's user", got)
	}

	// POST too, which has its own page for a browser.
	req, err := http.NewRequest(http.MethodPost, yarr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Outpost-Token", "wrong")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assertRefused(t, resp)
}

func TestAccessToken_Throttled(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	now := time.Now()
	h.g.now = func() time.Time { return now }
	good := h.pat("alice")
	bad := bearer(auth.AccessTokenPrefix + "wrong")

	for i := 0; i < h.g.tokenThrottle.free+1; i++ {
		assertRefused(t, h.send(http.MethodGet, yarr+"/", bad))
	}
	resp := h.send(http.MethodGet, yarr+"/", bad)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want 429", resp.StatusCode)
	}
	if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || n < 1 {
		t.Errorf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	// A good token from there waits too, or the answer would tell them apart.
	if resp := h.send(http.MethodGet, yarr+"/", bearer(good)); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a good token while throttled got %d", resp.StatusCode)
	}

	// Tokens have nothing to do with signing in, in either direction.
	if resp := h.login(h.client(true), "alice", "", "/"); resp.StatusCode != http.StatusOK {
		t.Errorf("sign-in while tokens are throttled got %d", resp.StatusCode)
	}
	if got := h.g.throttle.wait(now, "ip:127.0.0.1"); got != 0 {
		t.Errorf("tokens touched the login throttle: %v", got)
	}

	// A browser that presents no token isn't held up.
	if resp := h.send(http.MethodGet, yarr+"/", nil); resp.StatusCode != http.StatusFound {
		t.Errorf("no token while throttled got %d", resp.StatusCode)
	}

	// Once it's over, a good token works, but leaves the failures on
	// record: clearing them would let anyone holding one guess unslowed.
	now = now.Add(2 * throttleBase)
	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(good)))
	assertRefused(t, h.send(http.MethodGet, yarr+"/", bad))
	if resp := h.send(http.MethodGet, yarr+"/", bad); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a good token reset the guessing: next bad token got %d; want 429", resp.StatusCode)
	}
}

// Out of scope counts as a rejected token, a refusal by the allow list doesn't:
// the token was good and nobody is guessing.
func TestAccessToken_WhatCountsAgainstTheAddress(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}},
		supervisor.ServiceStatus{Name: "wiki", Status: "running"},
	)
	h.addUser(auth.User{Username: "alice"})
	denied, scoped := h.pat("alice"), h.pat("alice", "wiki")
	for i := 0; i < h.g.tokenThrottle.free+3; i++ {
		if resp := h.send(http.MethodGet, yarr+"/", bearer(denied)); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("allow-list refusal %d got %d", i, resp.StatusCode)
		}
	}
	for i := 0; i < h.g.tokenThrottle.free+1; i++ {
		assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(scoped)))
	}
	if resp := h.send(http.MethodGet, yarr+"/", bearer(scoped)); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("out-of-scope past the allowance got %d; want 429", resp.StatusCode)
	}
}

func TestAccessToken_ActivityFeed(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "running"},
	)
	h.addUser(auth.User{Username: "alice"})
	f := h.recording()
	scoped := h.pat("alice", "wiki")
	bad := auth.AccessTokenPrefix + "wrong-secret-value"

	assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(bad)))
	assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(bad)))
	assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(scoped)))
	assertRefused(t, h.send(http.MethodGet, portal+"/", bearer(scoped)))
	got := f.wait(t, 3)
	want := []string{
		"rejected access token from 127.0.0.1",
		"rejected access token for alice to yarr from 127.0.0.1",
		"rejected access token outside an app from 127.0.0.1",
	}
	for i, w := range want {
		if got[i].kind != EventTokenFail || got[i].message != w {
			t.Errorf("event %d = %+v; want %q", i, got[i], w)
		}
	}

	// The repeat becomes one line when its window closes.
	h.g.activity.mu.Lock()
	h.g.activity.flushLocked(time.Now().Add(activityWindow+time.Minute), false)
	h.g.activity.mu.Unlock()
	got = f.wait(t, 4)
	if !strings.HasPrefix(got[3].message, "1 more rejected access token from 127.0.0.1 since ") {
		t.Errorf("summary = %q", got[3].message)
	}
	for _, e := range got {
		if strings.Contains(e.message, "opat_") || strings.Contains(e.message, "wrong-secret") {
			t.Errorf("a token reached the feed: %q", e.message)
		}
	}
}

// Whatever happens to a token, it isn't written down.
func TestAccessToken_NeverLogged(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}},
		supervisor.ServiceStatus{Name: "down", Status: "running", Port: 1}, // the proxy's error path
	)
	h.addUser(auth.User{Username: "alice"})
	var (
		mu   sync.Mutex
		logs strings.Builder
	)
	h.g.logf = func(format string, v ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logs, format+"\n", v...)
	}
	good := h.pat("alice")
	bad := auth.AccessTokenPrefix + "wrong-secret-value"
	for _, tok := range []string{good, bad} {
		for _, u := range []string{yarr + "/", portal + "/", "http://down.outpost.test/", yarr + "/.outpost/x"} {
			_ = h.send(http.MethodGet, u, bearer(tok))
		}
		h.g.tokenThrottle.reset("ip:127.0.0.1")
	}
	mu.Lock()
	defer mu.Unlock()
	if s := logs.String(); strings.Contains(s, "opat_") || strings.Contains(s, good) || strings.Contains(s, "wrong-secret") {
		t.Errorf("a token reached the log:\n%s", s)
	}
}

// The login redirect's next and the rest of the browser flow aren't given a
// token to carry.
func TestAccessToken_NoTokenInAQueryString(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	resp := h.send(http.MethodGet, yarr+"/?access_token="+url.QueryEscape(h.pat("alice")), nil)
	if resp.StatusCode != http.StatusFound {
		t.Errorf("a token in the URL got %d; only headers carry one", resp.StatusCode)
	}
}

// The account page's two ways of shutting everything out end tokens and say so
// in the feed; ending one sign-in leaves them alone.
func TestAccessToken_AccountPage(t *testing.T) {
	const newPassword = "a whole new password"
	cases := []struct {
		name string
		form url.Values
		why  string
	}{
		{"sign out everywhere", url.Values{"action": {"everywhere"}}, "signed out everywhere"},
		{"change password", url.Values{"action": {"password"}, "current": {pw}, "password": {newPassword}, "confirm": {newPassword}}, "password changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.addUser(auth.User{Username: "alice"})
			f := h.recording()
			c := h.client(true)
			if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusOK {
				t.Fatal(resp.Status)
			}
			tok := h.pat("alice")
			backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))
			if resp := h.post(c, portal+"/account", portal, tc.form); resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: %s", tc.name, resp.Status)
			}
			assertRefused(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))

			want := "1 access token for alice revoked: " + tc.why
			got := f.wait(t, 4) // signed in, the action itself, the revocation, the refusal above
			if e := got[2]; e.kind != EventTokenRevoke || e.message != want {
				t.Errorf("feed = %+v; want %q", got, want)
			}
		})
	}

	t.Run("ending one sign-in", func(t *testing.T) {
		h := newHarness(t)
		h.addUser(auth.User{Username: "alice"})
		c := h.client(true)
		if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
		other, err := h.store.CreateSession("alice", "", "", "other device")
		if err != nil {
			t.Fatal(err)
		}
		tok := h.pat("alice")
		form := url.Values{"action": {"end"}, "id": {auth.HashToken(other)}}
		if resp := h.post(c, portal+"/account", portal, form); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
		if _, _, err := h.store.LookupSession(other); err == nil {
			t.Fatal("the session wasn't ended, so this proves nothing")
		}
		backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(tok)))
	})
}

// A node's address is the same app by another name, so the same rules apply.
func TestAccessToken_OnATailnetAddress(t *testing.T) {
	h := tailnetHarness(t)
	h.addUser(auth.User{Username: "alice"})
	node := "http://" + yarrNode + "/"

	s := backendSaw(t, h.send(http.MethodGet, node, bearer(h.pat("alice", "yarr"))))
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	if got := s.Header.Get("Authorization"); got != "" {
		t.Errorf("the token reached the app: %q", got)
	}
	assertRefused(t, h.send(http.MethodGet, node, bearer(h.pat("alice", "wiki"))))
}

// The throttle keys on the real client: a forwarded address from a trusted
// proxy, such as a tailnet node, and the peer otherwise, so nobody can choose
// their own way out of it.
func TestAccessToken_ThrottleKeysOnTheClient(t *testing.T) {
	bad := auth.AccessTokenPrefix + "wrong"
	exhaust := func(h *harness, from string) {
		h.t.Helper()
		for i := 0; i < h.g.tokenThrottle.free+1; i++ {
			assertRefused(h.t, h.send(http.MethodGet, yarr+"/", http.Header{
				"Authorization": {"Bearer " + bad}, "X-Forwarded-For": {from},
			}))
		}
	}

	t.Run("behind a trusted proxy", func(t *testing.T) {
		h := newHarness(t, func(c *config.Gateway) { c.TrustedProxies = []string{"127.0.0.1/32"} })
		h.addUser(auth.User{Username: "alice"})
		good := h.pat("alice")
		exhaust(h, "192.0.2.1")
		resp := h.send(http.MethodGet, yarr+"/", http.Header{"Authorization": {"Bearer " + good}, "X-Forwarded-For": {"192.0.2.1"}})
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("the address that failed got %d; want 429", resp.StatusCode)
		}
		backendSaw(t, h.send(http.MethodGet, yarr+"/", http.Header{"Authorization": {"Bearer " + good}, "X-Forwarded-For": {"192.0.2.2"}}))
	})

	t.Run("claiming another address", func(t *testing.T) {
		h := newHarness(t)
		h.addUser(auth.User{Username: "alice"})
		exhaust(h, "192.0.2.1")
		resp := h.send(http.MethodGet, yarr+"/", http.Header{"Authorization": {"Bearer " + bad}, "X-Forwarded-For": {"192.0.2.2"}})
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("an untrusted client changed its address and got %d; want 429", resp.StatusCode)
		}
	})
}

func TestBearerAccessToken(t *testing.T) {
	p := auth.AccessTokenPrefix
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"Bearer " + p + "x", p + "x", true},
		{"BEARER " + p + "x", p + "x", true},
		{"bearer\t" + p + "x", p + "x", true},
		{"  Bearer   " + p + "x  ", p + "x", true},
		{"Bearer " + p + "x y", p + "x y", true},
		{"Bearer " + p, p, true},
		{"Bearer x", "", false},
		{"Bearer", "", false},
		{"Basic " + p + "x", "", false},
		{p + "x", "", false},
		{"", "", false},
	} {
		got, ok := bearerAccessToken(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%q = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
