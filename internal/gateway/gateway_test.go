package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/supervisor"
)

const (
	portal = "http://outpost.test"
	yarr   = "http://yarr.outpost.test"
	wiki   = "http://wiki.outpost.test"
	pw     = "correct horse battery"
)

// fakeServices stands in for the supervisor's List.
type fakeServices struct {
	mu   sync.Mutex
	list []supervisor.ServiceStatus
}

func (f *fakeServices) List(context.Context) ([]supervisor.ServiceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.ServiceStatus(nil), f.list...), nil
}

type harness struct {
	t       *testing.T
	g       *Gateway
	store   *auth.Store
	front   *httptest.Server
	backend *httptest.Server
	// release ends the streaming handlers.
	release chan struct{}
}

// seen is what the backend reports about a request it received.
type seen struct {
	Path   string
	Host   string
	Header http.Header
}

func newHarness(t *testing.T, mutate ...func(*config.Gateway)) *harness {
	t.Helper()
	h := &harness{t: t, release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })

	store, err := auth.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	store.Params = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	h.store = store

	h.backend = httptest.NewServer(http.HandlerFunc(h.serveBackend))
	t.Cleanup(h.backend.Close)
	port := h.backend.Listener.Addr().(*net.TCPAddr).Port

	cfg := config.Gateway{
		Enabled:        true,
		PublicURL:      portal,
		Listen:         "127.0.0.1:0",
		TrustedProxies: []string{}, // the test client is on loopback; trust it only when a test says so
	}
	for _, m := range mutate {
		m(&cfg)
	}
	svcs := &fakeServices{list: []supervisor.ServiceStatus{
		{Name: "yarr", Port: port, Status: "running"},
		{Name: "wiki", Port: port, Status: "stopped"},
		{Name: "Bad_Name", Port: port, Status: "running"},
		{Name: "gone"}, // a stale state row: no port
	}}
	g, err := New(Options{Config: cfg, Auth: store, Services: svcs, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	g.refreshRoutes(context.Background())
	h.g = g
	h.front = httptest.NewServer(g)
	t.Cleanup(h.front.Close)
	return h
}

func (h *harness) serveBackend(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/events":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-h.release:
		case <-r.Context().Done():
		}
	case "/ws":
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(brw, buf); err == nil {
			_, _ = conn.Write(buf)
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{Path: r.URL.RequestURI(), Host: r.Host, Header: r.Header})
	}
}

// client is a browser: a cookie jar, following redirects, and every host
// resolving to the gateway.
func (h *harness) client(follow bool) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	c := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", h.front.Listener.Addr().String())
			},
		},
		Timeout: 10 * time.Second,
	}
	if !follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

func (h *harness) addUser(u auth.User) {
	h.t.Helper()
	if err := h.store.AddUser(u, pw); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) get(c *http.Client, rawURL string, hdr ...string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// post sends a form from origin (empty: no Origin header).
func (h *harness) post(c *http.Client, rawURL, origin string, form url.Values) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) login(c *http.Client, user, app, next string) *http.Response {
	h.t.Helper()
	return h.post(c, portal+"/login", portal, url.Values{
		"username": {user}, "password": {pw}, "app": {app}, "next": {next},
	})
}

// backendSaw decodes the echo backend's reply.
func backendSaw(t *testing.T, resp *http.Response) seen {
	t.Helper()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("didn't reach the backend: %d %s\n%s", resp.StatusCode, resp.Request.URL, body)
	}
	var s seen
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRouting(t *testing.T) {
	h := newHarness(t)
	c := h.client(false)
	cases := []struct {
		url  string
		want int
	}{
		{portal + "/login", http.StatusOK},
		{"http://OUTPOST.test:8443/login", http.StatusOK}, // case and port don't matter
		{"http://outpost.test./login", http.StatusOK},     // nor a trailing dot
		{yarr + "/", http.StatusFound},                    // an app: off to log in
		{wiki + "/", http.StatusFound},                    // stopped still routes; the proxy says it's down
		{"http://ghost.outpost.test/", http.StatusNotFound},
		{"http://a.yarr.outpost.test/", http.StatusNotFound},
		{"http://gone.outpost.test/", http.StatusNotFound},     // no port: not routable
		{"http://bad_name.outpost.test/", http.StatusNotFound}, // not a DNS label
		{"http://elsewhere.example/", http.StatusNotFound},
		{portal + "/nope", http.StatusNotFound},
		{yarr + "/.outpost/anything", http.StatusNotFound},
	}
	for _, tc := range cases {
		if resp := h.get(c, tc.url); resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d; want %d", tc.url, resp.StatusCode, tc.want)
		}
	}
}

// The whole handshake, as a browser does it: the app sends you to log in,
// the portal sends you back with a code, and the app sees who you are.
func TestHandshake(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", DisplayName: "Alice Example", Email: "alice@example.com",
		Groups: []string{"kids", "family"}})
	c := h.client(true)

	resp := h.get(c, yarr+"/feeds?unread=1")
	if resp.Request.URL.Host != "outpost.test" || resp.Request.URL.Path != "/login" {
		t.Fatalf("not sent to log in: ended at %s", resp.Request.URL)
	}
	if b := body(t, resp); !strings.Contains(b, `name="next" value="/feeds?unread=1"`) {
		t.Errorf("login form lost where we were going:\n%s", b)
	}

	got := backendSaw(t, h.login(c, "alice", "yarr", "/feeds?unread=1"))
	if got.Path != "/feeds?unread=1" {
		t.Errorf("landed on %q; want /feeds?unread=1", got.Path)
	}
	want := map[string]string{
		"X-Outpost-User":    "alice",
		"X-Outpost-Name":    "Alice Example",
		"X-Outpost-Email":   "alice@example.com",
		"X-Outpost-Groups":  "family,kids",
		"X-Forwarded-Host":  "yarr.outpost.test",
		"X-Forwarded-Proto": "http",
	}
	for k, v := range want {
		if g := got.Header.Get(k); g != v {
			t.Errorf("%s = %q; want %q", k, g, v)
		}
	}
	if got.Host != "yarr.outpost.test" {
		t.Errorf("app saw Host %q", got.Host)
	}
	if got.Header.Get("X-Outpost-Identity") == "" {
		t.Error("no identity token")
	}
	if strings.Contains(got.Header.Get("Cookie"), "outpost=") {
		t.Errorf("the app was handed Outpost's cookie: %q", got.Header.Get("Cookie"))
	}

	// A second app costs redirects but no typing.
	resp = h.get(c, wiki+"/page")
	if resp.StatusCode != http.StatusBadGateway {
		// wiki's port is the echo backend, but its route says stopped;
		// either way the request must have got past the login.
		got := backendSaw(t, resp)
		if got.Header.Get("X-Outpost-User") != "alice" || got.Path != "/page" {
			t.Errorf("second app: user %q path %q", got.Header.Get("X-Outpost-User"), got.Path)
		}
	}
}

// A cookie from the app host only works there; the portal session is only
// good at the portal.
func TestSessionsAreHostBound(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	yarrURL, _ := url.Parse(yarr)
	var appTok string
	for _, ck := range c.Jar.Cookies(yarrURL) {
		if ck.Name == "outpost" {
			appTok = ck.Value
		}
	}
	if appTok == "" {
		t.Fatal("no app cookie")
	}

	raw := h.client(false)
	// The yarr session presented at wiki, and at the portal.
	if resp := h.get(raw, wiki+"/", "Cookie", "outpost="+appTok); resp.StatusCode != http.StatusFound {
		t.Errorf("yarr's session opened wiki: %d", resp.StatusCode)
	}
	if resp := h.get(raw, portal+"/", "Cookie", "outpost="+appTok); resp.StatusCode != http.StatusFound {
		t.Errorf("an app session opened the portal: %d", resp.StatusCode)
	}
}

func TestForgedIdentityIsStripped(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	got := backendSaw(t, h.get(c, yarr+"/x",
		"X-Outpost-User", "mallory",
		"X-Outpost-Groups", "admins",
		"X-Outpost-Extra", "forged",
		"X-Forwarded-Host", "evil.example",
		"X-Forwarded-For", "203.0.113.9"))
	if u := got.Header.Get("X-Outpost-User"); u != "alice" {
		t.Errorf("X-Outpost-User = %q; want alice", u)
	}
	if g := got.Header.Get("X-Outpost-Groups"); g != "" {
		t.Errorf("X-Outpost-Groups = %q; want alice's (none)", g)
	}
	if got.Header.Get("X-Outpost-Extra") != "" {
		t.Error("an unknown X-Outpost-* header got through")
	}
	if fh := got.Header.Get("X-Forwarded-Host"); fh != "yarr.outpost.test" {
		t.Errorf("X-Forwarded-Host = %q; a client set it", fh)
	}
	if ff := got.Header.Get("X-Forwarded-For"); strings.Contains(ff, "203.0.113.9") {
		t.Errorf("X-Forwarded-For = %q; a client set it", ff)
	}
}

func TestOtherCookiesReachTheApp(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	yarrURL, _ := url.Parse(yarr)
	c.Jar.SetCookies(yarrURL, []*http.Cookie{{Name: "app_pref", Value: "dark"}})

	got := backendSaw(t, h.get(c, yarr+"/"))
	if ck := got.Header.Get("Cookie"); ck != "app_pref=dark" {
		t.Errorf("app's Cookie header = %q; want only its own cookie", ck)
	}
}

// The identity token verifies against the published keys, for this app.
func TestIdentityToken(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", Groups: []string{"family"}})
	c := h.client(true)
	tok := backendSaw(t, h.login(c, "alice", "yarr", "/")).Header.Get("X-Outpost-Identity")

	// Fetched the way a service on this machine would: by address.
	resp := h.get(h.client(false), "http://127.0.0.1"+jwksPath)
	var set struct {
		Keys []struct{ Kid, X, Alg string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	var hdr struct{ Alg, Kid string }
	if b, err := enc.DecodeString(parts[0]); err != nil || json.Unmarshal(b, &hdr) != nil {
		t.Fatalf("bad header: %v", err)
	}
	if hdr.Alg != "EdDSA" {
		t.Errorf("alg = %q", hdr.Alg)
	}
	var pub ed25519.PublicKey
	for _, k := range set.Keys {
		if k.Kid == hdr.Kid {
			x, err := enc.DecodeString(k.X)
			if err != nil {
				t.Fatal(err)
			}
			pub = x
		}
	}
	if pub == nil {
		t.Fatalf("kid %q isn't in the JWKS", hdr.Kid)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("signature doesn't verify")
	}
	var claims identityClaims
	if b, err := enc.DecodeString(parts[1]); err != nil || json.Unmarshal(b, &claims) != nil {
		t.Fatalf("bad claims: %v", err)
	}
	if claims.Sub != "alice" || claims.Aud != "yarr.outpost.test" || claims.Iss != portal {
		t.Errorf("claims = %+v", claims)
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "family" {
		t.Errorf("groups = %v", claims.Groups)
	}
	if life := claims.Exp - claims.Iat; life != int64(tokenLife.Seconds()) {
		t.Errorf("token lives %ds; want %v", life, tokenLife)
	}
}

func TestCallback_CodeIsSingleUseAndAppBound(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)

	resp := h.login(c, "alice", "yarr", "/")
	loc, err := resp.Location()
	if err != nil || loc.Host != "yarr.outpost.test" || loc.Path != "/.outpost/callback" {
		t.Fatalf("login didn't hand off to yarr: %d %v", resp.StatusCode, loc)
	}

	// Presented at the wrong app, the code is refused and spent.
	stolen := wiki + "/.outpost/callback?" + loc.RawQuery
	if resp := h.get(h.client(false), stolen); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("code accepted on another app's host: %d", resp.StatusCode)
	}
	if resp := h.get(c, loc.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("code survived being presented elsewhere: %d", resp.StatusCode)
	}

	// A fresh one works once.
	loc, _ = h.login(c, "alice", "yarr", "/").Location()
	if resp := h.get(c, loc.String()); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback = %d", resp.StatusCode)
	}
	if resp := h.get(h.client(false), loc.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a used code worked again: %d", resp.StatusCode)
	}
}

func TestNextCantLeaveTheHost(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	for _, next := range []string{"//evil.example", "https://evil.example/", "/\\evil.example", "/\t/evil.example"} {
		c := h.client(false)
		resp := h.login(c, "alice", "", next)
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Errorf("next=%q: redirected to %q; want /", next, loc)
		}
		loc, _ := h.login(c, "alice", "yarr", next).Location()
		if loc.Host != "yarr.outpost.test" || loc.Query().Get("next") != "" {
			t.Errorf("next=%q: handed off to %s", next, loc)
		}
	}
	// An app that isn't routed is ignored, not redirected to.
	resp := h.login(h.client(false), "alice", "evil.example", "/")
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("app=evil.example: redirected to %q", loc)
	}
}

func TestCSRF(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	form := url.Values{"username": {"alice"}, "password": {pw}}

	for _, origin := range []string{"", "http://evil.example", "https://outpost.test", "null"} {
		if resp := h.post(h.client(false), portal+"/login", origin, form); resp.StatusCode != http.StatusForbidden {
			t.Errorf("login with Origin %q = %d; want 403", origin, resp.StatusCode)
		}
	}
	// Referer stands in when there's no Origin.
	req, _ := http.NewRequest(http.MethodPost, portal+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", portal+"/login")
	resp, err := h.client(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login with a same-origin Referer = %d; want 303", resp.StatusCode)
	}

	// Logout too, on the portal and on an app host.
	if resp := h.post(h.client(false), portal+"/logout", "http://evil.example", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin logout = %d", resp.StatusCode)
	}
	if resp := h.post(h.client(false), yarr+"/.outpost/logout", portal, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("app logout from the portal's origin = %d", resp.StatusCode)
	}
}

func TestLoginThrottle(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	now := time.Now()
	h.g.now = func() time.Time { return now }
	c := h.client(false)

	bad := url.Values{"username": {"alice"}, "password": {"wrong password!"}}
	if resp := h.post(c, portal+"/login", portal, bad); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", resp.StatusCode)
	}
	// Right password, too soon.
	if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("immediate retry = %d; want 429", resp.StatusCode)
	}
	// An unknown user is told the same as a wrong password.
	resp := h.post(h.client(false), portal+"/login", portal, url.Values{"username": {"nobody"}, "password": {"x"}})
	if b := body(t, resp); resp.StatusCode != http.StatusTooManyRequests && !strings.Contains(b, "Wrong username or password.") {
		t.Errorf("unknown user: %d\n%s", resp.StatusCode, b)
	}

	now = now.Add(2 * time.Second)
	if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after the backoff = %d; want 303", resp.StatusCode)
	}
}

// Logging out from any app host ends the whole login.
func TestAppLogoutEndsEverything(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	h.get(c, wiki+"/") // takes a wiki session through the portal

	if resp := h.post(c, yarr+"/.outpost/logout", yarr, nil); resp.Request.URL.Path != "/login" {
		t.Fatalf("logout ended at %s", resp.Request.URL)
	}
	raw := h.client(false)
	raw.Jar = c.Jar
	for _, u := range []string{portal + "/", yarr + "/", wiki + "/"} {
		if resp := h.get(raw, u); resp.StatusCode != http.StatusFound {
			t.Errorf("GET %s after logout = %d; want a redirect to log in", u, resp.StatusCode)
		}
	}
}

func TestPortalLogout(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	h.post(c, portal+"/logout", portal, nil)

	raw := h.client(false)
	raw.Jar = c.Jar
	if resp := h.get(raw, yarr+"/"); resp.StatusCode != http.StatusFound {
		t.Errorf("app session outlived portal logout: %d", resp.StatusCode)
	}
}

func TestDisabledUserIsShutOut(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	if err := h.store.SetDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	raw := h.client(false)
	raw.Jar = c.Jar
	if resp := h.get(raw, yarr+"/"); resp.StatusCode != http.StatusFound {
		t.Errorf("disabled user still reached the app: %d", resp.StatusCode)
	}
}

func TestUnauthenticatedPostIsRefusedNotRedirected(t *testing.T) {
	h := newHarness(t)
	resp := h.post(h.client(false), yarr+"/api", yarr, url.Values{"a": {"b"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST without a session = %d; want 401", resp.StatusCode)
	}
}

func TestPortalPage(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", DisplayName: "<Alice>"})
	c := h.client(true)
	resp := h.login(c, "alice", "", "/")
	b := body(t, resp)
	for _, want := range []string{`href="http://yarr.outpost.test/"`, `href="http://wiki.outpost.test/"`, "&lt;Alice&gt;"} {
		if !strings.Contains(b, want) {
			t.Errorf("portal lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, "bad_name") || strings.Contains(b, "Bad_Name") {
		t.Error("portal links an unroutable service")
	}
	for _, hdr := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Cache-Control"} {
		if resp.Header.Get(hdr) == "" {
			t.Errorf("portal page lacks %s", hdr)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", resp.Header.Get("Content-Security-Policy"))
	}
}

func TestSetup(t *testing.T) {
	h := newHarness(t)
	link := h.g.SetupURL()
	if link == "" {
		t.Fatal("no setup link with no users")
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("token")

	if resp := h.get(h.client(false), portal+"/setup?token=wrong"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("setup with a wrong token = %d", resp.StatusCode)
	}
	c := h.client(true)
	if resp := h.get(c, link); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup page = %d", resp.StatusCode)
	}
	short := url.Values{"token": {tok}, "username": {"root"}, "password": {"short"}, "confirm": {"short"}}
	if resp := h.post(c, portal+"/setup", portal, short); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short password = %d", resp.StatusCode)
	}
	mismatch := url.Values{"token": {tok}, "username": {"root"}, "password": {pw}, "confirm": {pw + "x"}}
	if resp := h.post(c, portal+"/setup", portal, mismatch); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("mismatched passwords = %d", resp.StatusCode)
	}

	good := url.Values{"token": {tok}, "username": {"root"}, "password": {pw}, "confirm": {pw}}
	resp := h.post(c, portal+"/setup", portal, good)
	if resp.Request.URL.Path != "/" || resp.StatusCode != http.StatusOK {
		t.Fatalf("setup ended at %s (%d)", resp.Request.URL, resp.StatusCode)
	}
	if user, err := h.store.GetUser("root"); err != nil || !user.Admin {
		t.Errorf("root: %+v, %v; want an admin", user, err)
	}
	if h.g.SetupURL() != "" {
		t.Error("setup link still live after the first admin")
	}
	if resp := h.post(h.client(false), portal+"/setup", portal, good); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second setup = %d", resp.StatusCode)
	}
}

// A user added from the CLI closes the setup link too.
func TestSetupClosesOnceAnyoneExists(t *testing.T) {
	h := newHarness(t)
	link := h.g.SetupURL()
	h.addUser(auth.User{Username: "alice", Admin: true})
	if resp := h.get(h.client(false), link); resp.StatusCode != http.StatusNotFound {
		t.Errorf("setup after a CLI user = %d", resp.StatusCode)
	}
	if h.g.SetupURL() != "" {
		t.Error("link still offered")
	}
}

// X-Forwarded-Host decides the host only when a trusted proxy sends it.
func TestForwardedHostNeedsATrustedProxy(t *testing.T) {
	untrusted := newHarness(t)
	if resp := untrusted.get(untrusted.client(false), "http://internal:8443/login",
		"X-Forwarded-Host", "outpost.test"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("untrusted X-Forwarded-Host honoured: %d", resp.StatusCode)
	}

	trusted := newHarness(t, func(c *config.Gateway) { c.TrustedProxies = []string{"127.0.0.1/32"} })
	if resp := trusted.get(trusted.client(false), "http://internal:8443/login",
		"X-Forwarded-Host", "outpost.test"); resp.StatusCode != http.StatusOK {
		t.Errorf("trusted X-Forwarded-Host ignored: %d", resp.StatusCode)
	}
}

func TestServerSentEventsStream(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	resp := h.get(c, yarr+"/events")
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Errorf("first line = %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event didn't arrive while the stream was open: the proxy is buffering")
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	yarrURL, _ := url.Parse(yarr)
	var tok string
	for _, ck := range c.Jar.Cookies(yarrURL) {
		if ck.Name == "outpost" {
			tok = ck.Value
		}
	}

	conn, err := net.Dial("tcp", h.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: yarr.outpost.test\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nCookie: outpost="+tok+"\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "hello" {
		t.Errorf("echo = %q, %v", echo, err)
	}
}

// https: the cookie is __Host- and Secure, and the portal sends HSTS.
func TestSecureCookies(t *testing.T) {
	h := newHarness(t, func(c *config.Gateway) { c.PublicURL = "https://outpost.test" })
	h.addUser(auth.User{Username: "alice"})
	resp := h.post(h.client(false), "http://outpost.test/login", "https://outpost.test",
		url.Values{"username": {"alice"}, "password": {pw}})
	var found *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "__Host-outpost" {
			found = ck
		}
	}
	if found == nil || !found.Secure || !found.HttpOnly || found.Path != "/" || found.Domain != "" ||
		found.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie = %+v", found)
	}
	page := h.get(h.client(false), "http://outpost.test/login")
	if page.Header.Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS on an https portal")
	}
}
