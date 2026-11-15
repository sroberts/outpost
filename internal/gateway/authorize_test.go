package gateway

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

const clientRedirect = "https://claude.example/api/mcp/auth_callback"

// clientDoc serves an OAuth client's metadata document over TLS, as Claude's
// published identity would be; change rewrites it before it is sent.
type clientDoc struct {
	srv    *httptest.Server
	id     string
	change atomic.Value // func(map[string]any)
	status atomic.Int32
	hits   atomic.Int32
}

func newClientDoc(t *testing.T, h *harness) *clientDoc {
	t.Helper()
	d := &clientDoc{}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		if s := d.status.Load(); s != 0 {
			if s == http.StatusFound {
				http.Redirect(w, r, "https://elsewhere.example/doc", http.StatusFound)
				return
			}
			w.WriteHeader(int(s))
			return
		}
		doc := map[string]any{
			"client_id": d.id, "client_name": "Claude", "redirect_uris": []string{clientRedirect},
			"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"},
		}
		if f, ok := d.change.Load().(func(map[string]any)); ok && f != nil {
			f(doc)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(d.srv.Close)
	d.id = d.srv.URL + "/oauth/client.json"
	h.g.clientMetaHTTP = d.srv.Client()
	return d
}

func authorizeQuery(clientID string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {clientRedirect},
		"state": {"xyz"}, "code_challenge": {s256(testVerifier)}, "code_challenge_method": {"S256"},
		"scope": {"mcp"}, "resource": {testResource},
	}
}

// signedIn is a browser on the tailnet, signed in at the portal as user,
// that doesn't follow redirects, so where it's sent can be read.
func signedIn(t *testing.T, h *harness, user string) *http.Client {
	t.Helper()
	c := h.client(false)
	h.login(c, user, "", "/")
	return c
}

// The whole connection: consent on the portal, the code back to the client
// with state and iss, the exchange on the app's Funnel host, and an MCP call
// that reaches the app as the user.
func TestAuthorize_EndToEnd(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	f := h.recording()
	doc := newClientDoc(t, h)
	c := signedIn(t, h, "alice")

	q := authorizeQuery(doc.id)
	resp := h.get(c, portal+oauthAuthorizePath+"?"+q.Encode())
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent page = %d\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{"Connect Claude (127.0.0.1:", "as <strong>alice</strong>", `value="allow"`, `value="deny"`, "claude.example", doc.id} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("the consent page can be framed")
	}

	form := url.Values{"action": {"allow"}}
	for k, v := range q {
		form[k] = v
	}
	resp = h.post(c, portal+oauthAuthorizePath, portal, form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("Allow = %d\n%s", resp.StatusCode, body(t, resp))
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != clientRedirect {
		t.Fatalf("sent to %q", resp.Header.Get("Location"))
	}
	back := loc.Query()
	if back.Get("state") != "xyz" || back.Get("iss") != "https://sous.tail.ts.net" || back.Get("code") == "" {
		t.Errorf("redirect query = %v", back)
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("the redirect carrying the code has Referrer-Policy %q", resp.Header.Get("Referrer-Policy"))
	}

	ex := codeForm(back.Get("code"))
	ex.Set("client_id", doc.id)
	ex.Set("redirect_uri", clientRedirect)
	tresp := tokenPost(t, pub, "sous.tail.ts.net", ex)
	if tresp.StatusCode != http.StatusOK {
		t.Fatalf("exchange = %d", tresp.StatusCode)
	}
	var tok oauthTokenResponse
	decodeJSON(t, tresp, &tok)
	mcp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", strings.NewReader(`{}`), funnelBearer(tok.AccessToken)...)
	if mcp.StatusCode != http.StatusOK {
		t.Fatalf("MCP = %d", mcp.StatusCode)
	}
	var s seen
	decodeJSON(t, mcp, &s)
	if s.Header.Get("X-Outpost-User") != "alice" {
		t.Errorf("the app saw %q", s.Header.Get("X-Outpost-User"))
	}
	got := f.wait(t, 3) // the sign-in, the connection, the first use
	if !strings.Contains(got[1].message, "alice connected Claude (127.0.0.1:") {
		t.Errorf("feed = %+v", got)
	}
	if doc.hits.Load() != 1 {
		t.Errorf("the client's document was fetched %d times; want once, then cached", doc.hits.Load())
	}
}

// Someone not signed in goes to the login first, and comes back here.
func TestAuthorize_SignInFirst(t *testing.T) {
	h, _ := funnelMCPHarness(t)
	doc := newClientDoc(t, h)
	resp := h.get(h.client(false), portal+oauthAuthorizePath+"?"+authorizeQuery(doc.id).Encode())
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") || !strings.Contains(loc, url.QueryEscape(oauthAuthorizePath)) {
		t.Errorf("signed out = %d %q", resp.StatusCode, loc)
	}
	// A form posted without a session gets nothing.
	form := authorizeQuery(doc.id)
	form.Set("action", "allow")
	if resp := h.post(h.client(false), portal+oauthAuthorizePath, portal, form); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("POST signed out = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Until the client and its redirect_uri check out, a problem is shown on a
// page and nothing is sent anywhere.
func TestAuthorize_RefusalsBeforeTheRedirectIsTrusted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(q url.Values, d *clientDoc)
		want   string
	}{
		{"no resource", func(q url.Values, _ *clientDoc) { q.Del("resource") }, "doesn't name an app"},
		{"an app without mcp_funnel", func(q url.Values, _ *clientDoc) { q.Set("resource", "https://tasking.tail.ts.net/mcp") }, "doesn't name an app"},
		{"another path", func(q url.Values, _ *clientDoc) { q.Set("resource", "https://sous.tail.ts.net/other") }, "doesn't name an app"},
		{"http client id", func(q url.Values, d *clientDoc) { q.Set("client_id", strings.Replace(d.id, "https:", "http:", 1)) }, "https URL"},
		{"client id without a path", func(q url.Values, d *clientDoc) { q.Set("client_id", d.srv.URL+"/") }, "no path"},
		{"client id with dot segments", func(q url.Values, d *clientDoc) { q.Set("client_id", d.srv.URL+"/a/../b") }, "dot segments"},
		{"the issuer as client", func(q url.Values, _ *clientDoc) { q.Set("client_id", "https://sous.tail.ts.net") }, "claims to be this server"},
		{"unregistered redirect", func(q url.Values, _ *clientDoc) { q.Set("redirect_uri", "https://evil.example/cb") }, "don't list"},
		{"document names another client", func(_ url.Values, d *clientDoc) {
			d.change.Store(func(m map[string]any) { m["client_id"] = "https://other.example/client" })
		}, "different client"},
		{"document with a secret", func(_ url.Values, d *clientDoc) {
			d.change.Store(func(m map[string]any) { m["client_secret"] = "s3cret" })
		}, "secret"},
		{"document too big", func(_ url.Values, d *clientDoc) {
			d.change.Store(func(m map[string]any) { m["client_uri"] = strings.Repeat("x", clientMetaMaxBody) })
		}, "larger than 5 KB"},
		{"document missing", func(_ url.Values, d *clientDoc) { d.status.Store(http.StatusNotFound) }, "not 200"},
		{"document redirects", func(_ url.Values, d *clientDoc) { d.status.Store(http.StatusFound) }, "not 200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := funnelMCPHarness(t)
			doc := newClientDoc(t, h)
			c := signedIn(t, h, "alice")
			q := authorizeQuery(doc.id)
			tc.change(q, doc)
			resp := h.get(c, portal+oauthAuthorizePath+"?"+q.Encode())
			page := html.UnescapeString(body(t, resp))
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" || !strings.Contains(page, tc.want) {
				t.Errorf("= %d %q; want a 400 page saying %q\n%s", resp.StatusCode, resp.Header.Get("Location"), tc.want, page)
			}
		})
	}
}

// Once the redirect_uri is the client's own, a malformed request goes back
// to it as an OAuth error, with state and iss.
func TestAuthorize_ErrorsGoBackToTheClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(url.Values)
		want   string
	}{
		{"implicit flow", func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		{"no PKCE", func(q url.Values) { q.Del("code_challenge") }, "invalid_request"},
		{"plain PKCE", func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		{"bad challenge", func(q url.Values) { q.Set("code_challenge", "short") }, "invalid_request"},
		{"another scope", func(q url.Values) { q.Set("scope", "mcp admin") }, "invalid_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := funnelMCPHarness(t)
			doc := newClientDoc(t, h)
			c := signedIn(t, h, "alice")
			q := authorizeQuery(doc.id)
			tc.change(q)
			resp := h.get(c, portal+oauthAuthorizePath+"?"+q.Encode())
			loc, _ := url.Parse(resp.Header.Get("Location"))
			if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Host != "claude.example" ||
				loc.Query().Get("error") != tc.want || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") == "" {
				t.Errorf("= %d %q; want back to the client with %s", resp.StatusCode, resp.Header.Get("Location"), tc.want)
			}
		})
	}
}

// Don't allow says so to the client; a form from another site, or from
// someone the app's allow list refuses, gets no code.
func TestAuthorize_DenyCrossOriginAndAllowList(t *testing.T) {
	h, _ := funnelMCPHarness(t, func(s *supervisor.ServiceStatus) { s.Allow = []string{"alice"} })
	doc := newClientDoc(t, h)
	form := authorizeQuery(doc.id)

	alice := signedIn(t, h, "alice")
	form.Set("action", "deny")
	resp := h.post(alice, portal+oauthAuthorizePath, portal, form)
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Errorf("deny = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	form.Set("action", "allow")
	if resp := h.post(alice, portal+oauthAuthorizePath, "https://evil.example", form); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("from another site = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	bob := signedIn(t, h, "bob")
	if resp := h.get(bob, portal+oauthAuthorizePath+"?"+authorizeQuery(doc.id).Encode()); resp.StatusCode != http.StatusForbidden {
		t.Errorf("consent for someone the allow list refuses = %d", resp.StatusCode)
	}
	if resp := h.post(bob, portal+oauthAuthorizePath, portal, form); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Errorf("Allow from someone the allow list refuses = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.g.oauthCodes.mu.Lock()
	n := len(h.g.oauthCodes.m)
	h.g.oauthCodes.mu.Unlock()
	if n != 0 {
		t.Errorf("%d code(s) made", n)
	}
}

// The real fetcher won't reach anything but a public address on 443, so a
// client_id can't be pointed at the machine, the LAN or the tailnet.
func TestClientMeta_RefusesNonPublicAddresses(t *testing.T) {
	h, _ := funnelMCPHarness(t)
	doc := newClientDoc(t, h)
	h.g.clientMetaHTTP = nil // the real one
	if _, err := h.g.clientMetadata(t.Context(), doc.id); err == nil || !strings.Contains(err.Error(), "refusing to dial") {
		t.Errorf("fetching from loopback = %v; want refused at dial", err)
	}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true, "160.79.104.10": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"100.100.100.100": false, "100.64.0.1": false, "169.254.169.254": false, "0.0.0.0": false,
		"::1": false, "fd7a:115c:a1e0::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
		"64:ff9b::a00:1": false, "2002:a00:1::": false, "224.0.0.1": false, "255.255.255.255": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v; want %v", addr, got, want)
		}
	}
	if err := publicDialControl("tcp", "8.8.8.8:80", nil); err == nil {
		t.Error("port 80 was allowed")
	}
	for _, addr := range []string{"127.0.0.1:443", "100.100.100.100:443", "[fd7a:115c:a1e0::1]:443", "169.254.169.254:443"} {
		if err := publicDialControl("tcp", addr, nil); err == nil {
			t.Errorf("dialling %s was allowed", addr)
		}
	}
	if err := publicDialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("a public address on 443: %v", err)
	}
}
