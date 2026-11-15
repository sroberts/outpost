package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

const (
	testClient   = "https://claude.ai/oauth/claude-client-metadata"
	testRedirect = "https://claude.ai/api/mcp/auth_callback"
	testVerifier = "dBjftJeZ4CVP-mJ92K9xd0Y5mZ0aWQkn9Vd5NzlFqXqwqc0ZXlNLq2"
	testResource = "https://sous.tail.ts.net/mcp"
)

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// mintCode is what the portal's Allow does in step 3: a code for alice, for
// sous, for the test client.
func mintCode(t *testing.T, h *harness) string {
	t.Helper()
	code, err := h.g.oauthCodes.mint(oauthCode{
		user: "alice", app: "sous", clientID: testClient, clientName: "Claude",
		redirectURI: testRedirect, challenge: s256(testVerifier),
	}, h.g.now())
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func tokenPost(t *testing.T, pub *httptest.Server, host string, form url.Values, hdr ...string) *http.Response {
	t.Helper()
	return funnelSend(t, pub, http.MethodPost, host, oauthTokenPath, strings.NewReader(form.Encode()),
		append([]string{"Content-Type", "application/x-www-form-urlencoded"}, hdr...)...)
}

func codeForm(code string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {testClient},
		"redirect_uri": {testRedirect}, "code_verifier": {testVerifier}, "resource": {testResource},
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// An app with mcp_funnel publishes the two documents a client discovers it
// by, at the addresses MCP clients try, and they point at its own token
// endpoint and the portal's authorize page.
func TestOAuth_DiscoveryDocuments(t *testing.T) {
	_, pub := funnelMCPHarness(t)
	for _, p := range []string{oauthResourceMetaPath + "/mcp", oauthResourceMetaPath} {
		resp := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", p, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		decodeJSON(t, resp, &doc)
		if doc.Resource != testResource || len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://sous.tail.ts.net" {
			t.Errorf("%s = %+v", p, doc)
		}
	}

	resp := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", oauthServerMetaPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server metadata: %d", resp.StatusCode)
	}
	var as map[string]any
	decodeJSON(t, resp, &as)
	want := map[string]any{
		"issuer":                                "https://sous.tail.ts.net",
		"authorization_endpoint":                portal + oauthAuthorizePath,
		"token_endpoint":                        "https://sous.tail.ts.net" + oauthTokenPath,
		"client_id_metadata_document_supported": true,
	}
	for k, v := range want {
		if as[k] != v {
			t.Errorf("%s = %v; want %v", k, as[k], v)
		}
	}
	if m, _ := as["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v; MCP clients refuse to go on without S256", as["code_challenge_methods_supported"])
	}
	if _, ok := as["registration_endpoint"]; ok {
		t.Error("a registration endpoint is advertised; there is none")
	}
}

// Only an app with mcp_funnel has them, only on its node's own name, only at
// those exact paths, and the portal and OpenID addresses stay 404.
func TestOAuth_DiscoveryOnlyWhereTheMCPEndpointIs(t *testing.T) {
	_, pub := funnelMCPHarness(t)
	for _, tc := range []struct{ host, path string }{
		{"tasking.tail.ts.net", oauthServerMetaPath},   // Funnel on, mcp_funnel off
		{"tasking.tail.ts.net", oauthResourceMetaPath}, // Funnel on, mcp_funnel off
		{"tasking.tail.ts.net", oauthTokenPath},
		{"sous.tail.ts.net", "/.well-known/openid-configuration"},
		{"sous.tail.ts.net", oauthResourceMetaPath + "/other"},
		{"sous.tail.ts.net", oauthServerMetaPath + "/"},
		{"sous.tail.ts.net", "/.well-known/oauth-authorization-server%2F"},
		{"sous.tail.ts.net", "/.outpost/oauth/authorize"},
		{"elsewhere.example", oauthServerMetaPath},
	} {
		if resp := funnelSend(t, pub, http.MethodGet, tc.host, tc.path, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s%s = %d; want 404", tc.host, tc.path, resp.StatusCode)
		}
	}
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", oauthServerMetaPath, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to a document = %d", resp.StatusCode)
	}
}

// The code exchange gives an hour's access token, which reaches the app's
// MCP endpoint as the user, and a refresh token; the code then can't be used
// again, and connecting is in the activity feed.
func TestOAuth_CodeExchangeReachesTheApp(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	f := h.recording()
	code := mintCode(t, h)
	resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var tok oauthTokenResponse
	decodeJSON(t, resp, &tok)
	if !strings.HasPrefix(tok.AccessToken, auth.AccessTokenPrefix) || !strings.HasPrefix(tok.RefreshToken, auth.RefreshTokenPrefix) ||
		tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.Scope != "mcp" {
		t.Errorf("token response = %+v", tok)
	}
	mcp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", strings.NewReader(`{}`), funnelBearer(tok.AccessToken)...)
	if mcp.StatusCode != http.StatusOK {
		t.Fatalf("MCP with the access token = %d", mcp.StatusCode)
	}
	var s seen
	decodeJSON(t, mcp, &s)
	if s.Header.Get("X-Outpost-User") != "alice" || s.Header.Get("Authorization") != "" {
		t.Errorf("the app saw user %q, Authorization %q", s.Header.Get("X-Outpost-User"), s.Header.Get("Authorization"))
	}

	if resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the code used again = %d", resp.StatusCode)
	}
	got := f.wait(t, 2)
	if got[0].kind != EventOAuthConnect || !strings.Contains(got[0].message, "alice connected Claude (claude.ai) to sous, from 203.0.113.7") {
		t.Errorf("feed = %+v", got)
	}
	if got[1].kind != EventTokenInternet {
		t.Errorf("then = %+v", got[1])
	}
	if grants, _ := h.store.ListGrants("alice"); len(grants) != 1 {
		t.Errorf("grants = %+v", grants)
	}
}

// Anything about the code that doesn't match what consent was given for is
// invalid_grant, and the code is spent by the attempt.
func TestOAuth_CodeExchangeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(url.Values)
		host   string
		want   string
	}{
		{"wrong verifier", func(f url.Values) { f.Set("code_verifier", strings.Repeat("x", 43)) }, "", "invalid_grant"},
		{"short verifier", func(f url.Values) { f.Set("code_verifier", "short") }, "", "invalid_grant"},
		{"no verifier", func(f url.Values) { f.Del("code_verifier") }, "", "invalid_grant"},
		{"another client", func(f url.Values) { f.Set("client_id", "https://evil.example/client") }, "", "invalid_grant"},
		{"another redirect", func(f url.Values) { f.Set("redirect_uri", "https://evil.example/cb") }, "", "invalid_grant"},
		{"another resource", func(f url.Values) { f.Set("resource", "https://tasking.tail.ts.net/mcp") }, "", "invalid_target"},
		{"unknown code", func(f url.Values) { f.Set("code", "nope") }, "", "invalid_grant"},
		{"no grant type", func(f url.Values) { f.Del("grant_type") }, "", "invalid_request"},
		{"password grant", func(f url.Values) { f.Set("grant_type", "password") }, "", "unsupported_grant_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pub := funnelMCPHarness(t)
			code := mintCode(t, h)
			form := codeForm(code)
			tc.change(form)
			resp := tokenPost(t, pub, "sous.tail.ts.net", form)
			var e struct{ Error string }
			decodeJSON(t, resp, &e)
			if resp.StatusCode != http.StatusBadRequest || e.Error != tc.want {
				t.Errorf("= %d %q; want 400 %q", resp.StatusCode, e.Error, tc.want)
			}
			if tc.want == "invalid_grant" && tc.name != "unknown code" {
				if resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code)); resp.StatusCode != http.StatusBadRequest {
					t.Errorf("the code survived a failed attempt: %d", resp.StatusCode)
				}
			}
			if grants, _ := h.store.ListGrants(""); len(grants) != 0 {
				t.Errorf("granted: %+v", grants)
			}
		})
	}
}

// A code for one app can't be exchanged at another's token endpoint, and an
// expired code is no good.
func TestOAuth_CodeIsBoundToItsAppAndMinute(t *testing.T) {
	h, pub := funnelMCPHarness(t, func(s *supervisor.ServiceStatus) {})
	// A second app with its MCP endpoint on the internet.
	h.g.tailnetHosts = func() map[string]string {
		return map[string]string{"sous": "sous.tail.ts.net", "tasking": "tasking.tail.ts.net"}
	}
	fs := h.g.services.(*fakeServices)
	fs.mu.Lock()
	for i := range fs.list {
		if fs.list[i].Name == "tasking" {
			fs.list[i].MCPFunnel = true
		}
	}
	fs.mu.Unlock()
	h.g.refreshRoutes(t.Context())

	form := codeForm(mintCode(t, h))
	form.Del("resource")
	if resp := tokenPost(t, pub, "tasking.tail.ts.net", form); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("sous's code at tasking's endpoint = %d", resp.StatusCode)
	}

	code := mintCode(t, h)
	now := h.g.now()
	h.g.now = func() time.Time { return now.Add(codeTTL) }
	if resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an expired code = %d", resp.StatusCode)
	}
}

// Refreshing rotates; the old refresh token used again ends the connection
// and says why in the feed.
func TestOAuth_RefreshRotatesAndReuseEndsIt(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	f := h.recording()
	var first oauthTokenResponse
	decodeJSON(t, tokenPost(t, pub, "sous.tail.ts.net", codeForm(mintCode(t, h))), &first)

	refresh := func(tok string) *http.Response {
		return tokenPost(t, pub, "sous.tail.ts.net", url.Values{
			"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {testClient}, "resource": {testResource},
		})
	}
	resp := refresh(first.RefreshToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh = %d", resp.StatusCode)
	}
	var second oauthTokenResponse
	decodeJSON(t, resp, &second)
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Error("refresh didn't rotate")
	}

	if resp := refresh(first.RefreshToken); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the old refresh token again = %d", resp.StatusCode)
	}
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(second.AccessToken)...); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after reuse, the newest access token = %d; want 401", resp.StatusCode)
	}
	if resp := refresh(second.RefreshToken); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("after reuse, the newest refresh token = %d", resp.StatusCode)
	}
	got := f.wait(t, 2)
	if got[1].kind != EventOAuthConnect || !strings.Contains(got[1].message, "used twice") {
		t.Errorf("feed = %+v", got)
	}
}

// Refused requests are counted against their address and get 429 while it
// owes time; a good exchange from that address still goes through, since
// Claude's addresses are shared.
func TestOAuth_TokenEndpointThrottlesRefusalsOnly(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	shared := []string{"X-Forwarded-For", "160.79.104.10"}
	var last int
	for range publicThrottleFree + 2 {
		last = tokenPost(t, pub, "sous.tail.ts.net", codeForm("guess"), shared...).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after repeated bad codes: %d; want 429", last)
	}
	if resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(mintCode(t, h)), shared...); resp.StatusCode != http.StatusOK {
		t.Errorf("a good code from the backed-off address = %d", resp.StatusCode)
	}
	// Discovery isn't throttled either.
	if resp := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", oauthServerMetaPath, nil, shared...); resp.StatusCode != http.StatusOK {
		t.Errorf("discovery from the backed-off address = %d", resp.StatusCode)
	}
	if resp := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", oauthTokenPath, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on the token endpoint = %d", resp.StatusCode)
	}
	big := codeForm("x")
	big.Set("pad", strings.Repeat("a", maxTokenForm))
	if resp := tokenPost(t, pub, "sous.tail.ts.net", big); resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("an oversized form = %d %s", resp.StatusCode, b)
	}
}

// A user shut out between consent and exchange gets nothing.
func TestOAuth_NoGrantForADisabledUser(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	code := mintCode(t, h)
	if err := h.store.SetDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("exchange for a disabled user = %d", resp.StatusCode)
	}
}

func TestPKCE(t *testing.T) {
	// The challenge computed apart from Go, by Python's hashlib and base64.
	if !pkceMatches("38GKQryqeoLsGKjuSEfgfyYFj2l3eAc4ZOQ9EhXNUIo", testVerifier) {
		t.Error("a verifier doesn't match its own S256 challenge")
	}
	if pkceMatches("38GKQryqeoLsGKjuSEfgfyYFj2l3eAc4ZOQ9EhXNUIo", testVerifier+"x") {
		t.Error("another verifier matched")
	}
	if pkceMatches("", testVerifier) || pkceMatches(s256("short"), "short") {
		t.Error("an empty challenge or a short verifier matched")
	}
}

// A verifier may only use RFC 7636's unreserved characters.
func TestPKCE_VerifierCharacters(t *testing.T) {
	for _, v := range []string{
		strings.Repeat("a", 42) + " ", strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "é", strings.Repeat("a", 42) + "/",
	} {
		if pkceMatches(s256(v), v) {
			t.Errorf("verifier %q matched", v)
		}
	}
	if v := strings.Repeat("aZ09-._~", 6); !pkceMatches(s256(v), v) {
		t.Error("a verifier of unreserved characters didn't match")
	}
}

// The server's own failure is a JSON server_error, not invalid_grant, and
// isn't held against the client's address.
func TestOAuth_ServerFailureIsNotTheClients(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	shared := []string{"X-Forwarded-For", "160.79.104.10"}
	codes := make([]string, publicThrottleFree+2)
	for i := range codes {
		codes[i] = mintCode(t, h)
	}
	h.store.Close() // every grant now fails as auth.db would if it couldn't be written
	for _, code := range codes {
		resp := tokenPost(t, pub, "sous.tail.ts.net", codeForm(code), shared...)
		var e struct{ Error string }
		decodeJSON(t, resp, &e)
		if resp.StatusCode != http.StatusInternalServerError || e.Error != "server_error" {
			t.Fatalf("= %d %q; want 500 server_error", resp.StatusCode, e.Error)
		}
	}
	if d := h.g.publicThrottle.wait(h.g.now(), "160.79.104.10"); d > 0 {
		t.Errorf("the server's failures were counted against the address: owes %v", d)
	}
}

// The discovery documents aren't cached, since they go when mcp_funnel does.
func TestOAuth_DiscoveryIsNotCached(t *testing.T) {
	_, pub := funnelMCPHarness(t)
	for _, p := range []string{oauthServerMetaPath, oauthResourceMetaPath + "/mcp"} {
		if got := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", p, nil).Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q", p, got)
		}
	}
}
