package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// OAuth for an app's MCP endpoint on the internet, as designed in
// docs/design/mcp-oauth.md. Each app with mcp_funnel on is its own
// authorization server, its issuer the address its node reports. The
// documents a client discovers it by, and the token endpoint, are on that
// host's Funnel, beside the MCP endpoint and nothing else of the app. The
// authorization endpoint is on the portal, which isn't on the internet: the
// person granting access opens it in their own browser, on the tailnet.

const (
	// Where on an app's Funnel host the OAuth addresses are. The protected
	// resource's metadata is also answered at its path-based address,
	// oauthResourceMetaPath followed by the app's mcp_path, the one MCP
	// clients try first.
	oauthResourceMetaPath = "/.well-known/oauth-protected-resource"
	oauthServerMetaPath   = "/.well-known/oauth-authorization-server"
	oauthTokenPath        = "/.outpost/oauth/token"
	// oauthAuthorizePath is on the portal.
	oauthAuthorizePath = "/oauth/authorize"

	// oauthScope is the one scope: the app's MCP endpoint, all of it.
	oauthScope = "mcp"

	// maxTokenForm bounds a token request's body. Its fields are a code or
	// refresh token, a URL or two and a verifier.
	maxTokenForm = 16 << 10
	// maxOAuthCodes bounds the codes held for consent given but not yet
	// exchanged. Only someone signed in to the portal can make one.
	maxOAuthCodes = 1000
)

// ClientLabel names an OAuth client for a person: the name its metadata
// document gives, which the client chose, beside the host that document is
// served from, which it can't choose. "Claude (claude.ai)", or the host alone
// when there is no name. See docs/design/mcp-oauth.md.
func ClientLabel(name, clientID string) string {
	host := clientID
	if u, err := url.Parse(clientID); err == nil && u.Host != "" {
		host = u.Host
	}
	if name == "" || name == host {
		return host
	}
	return name + " (" + host + ")"
}

// oauthIssuer is the authorization server identifier of an app on the
// internet: its Funnel host.
func oauthIssuer(rt route) string { return "https://" + rt.Host }

// oauthResource is the canonical URI of an app's MCP endpoint, which a
// client names in its requests' resource parameter.
func oauthResource(rt route) string { return oauthIssuer(rt) + rt.MCPPath }

// funnelOAuthRoute is the app a request from the internet is for, when it is
// one of that app's OAuth addresses, under the same conditions as
// funnelMCPRoute: the node's own name, mcp_funnel on, nothing escaped.
func (g *Gateway) funnelOAuthRoute(r *http.Request) (route, bool) {
	host := hostname(r.Host)
	rt, ok := g.routes.forHost(host)
	if !ok || !rt.MCPFunnel || rt.MCPPath == "" || rt.Host == "" || rt.Host != host {
		return route{}, false
	}
	if r.URL.EscapedPath() != r.URL.Path {
		return route{}, false
	}
	switch r.URL.Path {
	case oauthResourceMetaPath, oauthResourceMetaPath + rt.MCPPath, oauthServerMetaPath, oauthTokenPath:
		return rt, true
	}
	return route{}, false
}

// serveFunnelOAuth answers an app's OAuth addresses on the internet.
func (g *Gateway) serveFunnelOAuth(w http.ResponseWriter, r *http.Request, rt route, client string) {
	if r.URL.Path == oauthTokenPath {
		g.serveOAuthToken(w, r, rt, client)
		return
	}
	// The documents are the same for anyone and say nothing a client
	// couldn't learn from the 401, so they aren't throttled: Claude's
	// addresses are shared, and a stranger shouldn't be able to keep the
	// owner's connector from finding them.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		g.publicThrottle.fail(g.now(), client)
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var doc any
	if r.URL.Path == oauthServerMetaPath {
		doc = map[string]any{
			"issuer":                                         oauthIssuer(rt),
			"authorization_endpoint":                         g.portalOrigin + oauthAuthorizePath,
			"token_endpoint":                                 oauthIssuer(rt) + oauthTokenPath,
			"response_types_supported":                       []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":               []string{"S256"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"scopes_supported":                               []string{oauthScope},
			"client_id_metadata_document_supported":          true,
			"authorization_response_iss_parameter_supported": true,
		}
	} else {
		doc = map[string]any{
			"resource":                 oauthResource(rt),
			"authorization_servers":    []string{oauthIssuer(rt)},
			"bearer_methods_supported": []string{"header"},
			"scopes_supported":         []string{oauthScope},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	// Not cached: they stop existing the moment mcp_funnel is turned off.
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(doc) // the client has gone if this fails
}

// oauthTokenResponse is a successful token response (RFC 6749 §5.1).
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// serveOAuthToken is an app's token endpoint. A request that gets tokens is
// never held back by the throttle, for the reason serveFunnelMCP gives; one
// that doesn't is counted against its address, and gets a 429 while that
// address owes time.
func (g *Gateway) serveOAuthToken(w http.ResponseWriter, r *http.Request, rt route, client string) {
	now := g.now()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	// failed is the server's own trouble, such as auth.db being busy: not
	// counted against the address, and said in the form clients parse.
	failed := func(what string, err error) {
		g.lg("service", rt.Name, "err", err).Warnf("gateway: oauth %s: %s: %v", rt.Name, what, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "server_error", "error_description": "try again shortly"}) // the client has gone if this fails
	}
	refuse := func(status int, code, description string) {
		if d := g.publicThrottle.wait(now, client); d > 0 {
			w.Header().Set("Retry-After", retryAfter(d))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		g.publicThrottle.fail(now, client)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description}) // the client has gone if this fails
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		refuse(http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenForm)
	if err := r.ParseForm(); err != nil {
		refuse(http.StatusBadRequest, "invalid_request", "the form couldn't be read")
		return
	}
	f := r.PostForm
	// RFC 8707: a client names the resource it wants a token for, and only
	// this app's MCP endpoint is on offer here.
	if res := f.Get("resource"); res != "" && res != oauthResource(rt) {
		refuse(http.StatusBadRequest, "invalid_target", "this server issues tokens only for "+oauthResource(rt))
		return
	}
	clientID := f.Get("client_id")
	var (
		access, refresh string
		grant           auth.Grant
		err             error
	)
	switch f.Get("grant_type") {
	case "authorization_code":
		code, ok := g.oauthCodes.take(f.Get("code"), now)
		switch {
		case !ok:
			refuse(http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
			return
		case code.app != rt.Name || code.clientID != clientID || code.redirectURI != f.Get("redirect_uri"):
			refuse(http.StatusBadRequest, "invalid_grant", "the code was issued for another client, app or redirect_uri")
			return
		case !pkceMatches(code.challenge, f.Get("code_verifier")):
			refuse(http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code_challenge")
			return
		}
		access, refresh, grant, err = g.auth.CreateGrant(code.user, rt.Name, code.clientID, code.clientName)
		if errors.Is(err, auth.ErrInvalidGrant) {
			g.lg("service", rt.Name, "user", code.user, "err", err).Warnf("gateway: oauth %s: no grant for %s: %v", rt.Name, code.user, err)
			refuse(http.StatusBadRequest, "invalid_grant", "access can't be granted for this account")
			return
		}
		if err != nil {
			failed("grant for "+code.user, err)
			return
		}
		g.activity.record(EventOAuthConnect, "%s connected %s to %s, from %s",
			grant.Username, ClientLabel(grant.ClientName, grant.ClientID), rt.Name, client)
	case "refresh_token":
		access, refresh, grant, err = g.auth.RefreshGrant(f.Get("refresh_token"), clientID, rt.Name)
		switch {
		case errors.Is(err, auth.ErrRefreshReused):
			g.activity.record(EventOAuthConnect, "%s's connection of %s to %s was ended: a refresh token was used twice, from %s, so a copy of it exists",
				grant.Username, ClientLabel(grant.ClientName, grant.ClientID), rt.Name, client)
			refuse(http.StatusBadRequest, "invalid_grant", "the refresh token is no longer valid")
			return
		case errors.Is(err, auth.ErrInvalidGrant):
			refuse(http.StatusBadRequest, "invalid_grant", "the refresh token is no longer valid")
			return
		case err != nil:
			failed("refresh", err)
			return
		}
	case "":
		refuse(http.StatusBadRequest, "invalid_request", "grant_type is required")
		return
	default:
		refuse(http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(oauthTokenResponse{ // the client has gone if this fails
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int(auth.GrantAccessTTL / time.Second),
		RefreshToken: refresh, Scope: oauthScope,
	})
}

// pkceMatches checks a PKCE code_verifier against an S256 code_challenge
// (RFC 7636 §4.6). A verifier is 43 to 128 characters.
func pkceMatches(challenge, verifier string) bool {
	if n := len(verifier); n < 43 || n > 128 || challenge == "" {
		return false
	}
	// §4.1 restricts a verifier to unreserved characters; anything else is
	// a client that isn't doing PKCE as specified.
	for i := 0; i < len(verifier); i++ {
		switch c := verifier[i]; {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) == 1
}

// oauthCode is what an authorization code stands for: this user let this
// client use this app, sending the code to this redirect_uri, with this
// PKCE challenge.
type oauthCode struct {
	user, app              string
	clientID, clientName   string
	redirectURI, challenge string
	expires                time.Time
}

// oauthCodeStore holds authorization codes in memory, keyed by their hash,
// for codeTTL. Like the sign-in codes in codeStore, a restart ending them
// costs only pressing Allow again.
type oauthCodeStore struct {
	mu sync.Mutex
	m  map[string]oauthCode
}

func (c *oauthCodeStore) mint(p oauthCode, now time.Time) (string, error) {
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]oauthCode{}
	}
	if len(c.m) >= maxOAuthCodes {
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxOAuthCodes {
			return "", errTooManyCodes
		}
	}
	p.expires = now.Add(codeTTL)
	c.m[auth.HashToken(tok)] = p
	return tok, nil
}

// take uses up a code: it's gone after the first try, whether or not the
// rest of the request matches, so a code can't be tried twice.
func (c *oauthCodeStore) take(code string, now time.Time) (oauthCode, bool) {
	if code == "" {
		return oauthCode{}, false
	}
	h := auth.HashToken(code)
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[h]
	delete(c.m, h)
	if !ok || !now.Before(p.expires) {
		return oauthCode{}, false
	}
	return p, true
}
