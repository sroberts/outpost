package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The portal's /oauth/authorize: where someone signed in lets an OAuth client
// use one app's MCP endpoint as them. It is on the portal, so only a browser
// on the tailnet reaches it; the code it hands out is exchanged on the app's
// Funnel host (oauth.go). See docs/design/mcp-oauth.md.

// maxAuthorizeForm bounds the consent form, which carries the authorize
// request's parameters through.
const maxAuthorizeForm = 16 << 10

// consentView is the consent page.
type consentView struct {
	Client     string // the client's name and its document's host
	ClientID   string
	Redirect   string // where the browser goes next, when not the client's host
	App        string
	AppTitle   string
	User       string
	Params     []consentParam // the request, carried through the form
	ClientHost string
}

type consentParam struct{ Name, Value string }

// authorizeRequest is an authorize request that has passed every check.
type authorizeRequest struct {
	rt                              route
	meta                            clientMeta
	clientID, redirectURI, state    string
	challenge, scope, resourceParam string
}

// authorizeError is why a request was refused. With redirect false it is
// shown on a page, because the redirect_uri isn't trusted yet; otherwise it
// goes back to the client as an OAuth error.
type authorizeError struct {
	redirect    bool
	code, words string
}

// readAuthorize checks an authorize request's parameters, in the order RFC
// 6749 §4.1.2.1 needs: everything that decides whether the redirect_uri can
// be trusted comes first.
func (g *Gateway) readAuthorize(ctx context.Context, v url.Values) (authorizeRequest, *authorizeError) {
	page := func(words string) (authorizeRequest, *authorizeError) {
		return authorizeRequest{}, &authorizeError{words: words}
	}
	var a authorizeRequest
	a.resourceParam = v.Get("resource")
	rt, ok := g.oauthRouteFor(a.resourceParam)
	if !ok {
		return page("The link doesn't name an app whose MCP endpoint is on the internet here. Check the connector's URL, and that the app has mcp_funnel on.")
	}
	a.rt = rt
	a.clientID = v.Get("client_id")
	if a.clientID == oauthIssuer(rt) {
		return page("The client claims to be this server.")
	}
	meta, err := g.clientMetadata(ctx, a.clientID)
	if err != nil {
		return page("The client's details couldn't be checked: " + strings.TrimSuffix(err.Error(), ".") + ".")
	}
	a.meta = meta
	a.redirectURI = v.Get("redirect_uri")
	registered := false
	for _, u := range meta.RedirectURIs {
		if u == a.redirectURI {
			registered = true
		}
	}
	ru, err := url.Parse(a.redirectURI)
	if !registered || err != nil || ru.Scheme != "https" || ru.Host == "" || ru.Fragment != "" {
		return page("The client asked to send you somewhere its own details don't list, so Outpost stopped here.")
	}

	// From here the redirect_uri is the client's own, so problems go back to it.
	back := func(code, words string) (authorizeRequest, *authorizeError) {
		return a, &authorizeError{redirect: true, code: code, words: words}
	}
	a.state = v.Get("state")
	a.challenge = v.Get("code_challenge")
	a.scope = v.Get("scope")
	switch {
	case v.Get("response_type") != "code":
		return back("unsupported_response_type", "only response_type=code")
	case v.Get("code_challenge_method") != "S256" || !validChallenge(a.challenge):
		return back("invalid_request", "PKCE with code_challenge_method=S256 is required")
	case len(a.state) > 1024:
		return back("invalid_request", "state is too long")
	}
	for _, s := range strings.Fields(a.scope) {
		if s != oauthScope {
			return back("invalid_scope", "the only scope is "+oauthScope)
		}
	}
	return a, nil
}

// oauthRouteFor is the app whose MCP endpoint's canonical URI is resource.
func (g *Gateway) oauthRouteFor(resource string) (route, bool) {
	if resource == "" {
		return route{}, false
	}
	u, err := url.Parse(resource)
	if err != nil || u.Scheme != "https" {
		return route{}, false
	}
	rt, ok := g.routes.forHost(strings.ToLower(u.Host))
	if !ok || !rt.MCPFunnel || rt.MCPPath == "" || rt.Host == "" || oauthResource(rt) != resource {
		return route{}, false
	}
	return rt, true
}

// validChallenge is an S256 code_challenge: 43 base64url characters.
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	for i := 0; i < len(c); i++ {
		switch b := c[i]; {
		case 'A' <= b && b <= 'Z', 'a' <= b && b <= 'z', '0' <= b && b <= '9', b == '-', b == '_':
		default:
			return false
		}
	}
	return true
}

// oauthAuthorize serves /oauth/authorize: the consent page on GET, the
// answer on POST.
func (g *Gateway) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	post := r.Method == http.MethodPost
	if !post && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, POST")
		g.message(w, r, http.StatusMethodNotAllowed, "Nothing here", "There's no page at this address.", g.portalLink(), "Back to apps")
		return
	}
	if post && !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	v := r.URL.Query()
	if post {
		r.Body = http.MaxBytesReader(w, r.Body, maxAuthorizeForm)
		if err := r.ParseForm(); err != nil {
			g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Start connecting again from the app.", g.portalLink(), "Back to apps")
			return
		}
		v = r.PostForm
	} else {
		g.logf("gateway: oauth authorize: client_id=%q redirect_uri=%q resource=%q scope=%q",
			v.Get("client_id"), v.Get("redirect_uri"), v.Get("resource"), v.Get("scope"))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	a, aerr := g.readAuthorize(ctx, v)
	if aerr != nil && !aerr.redirect {
		g.message(w, r, http.StatusBadRequest, "Can't connect", aerr.words, g.portalLink(), "Back to apps")
		return
	}
	if aerr != nil {
		g.authorizeRedirect(w, r, a, url.Values{"error": {aerr.code}, "error_description": {aerr.words}})
		return
	}

	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		if post {
			// A form from a session that has since ended: start again.
			g.message(w, r, http.StatusForbidden, "Signed out", "Sign in and start connecting again from the app.", g.portalLink(), "Sign in")
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	g.touch(tok, s)
	if !allowed(u, a.rt) {
		g.denied(w, r, a.rt, u)
		return
	}
	label := ClientLabel(a.meta.ClientName, a.clientID)

	if !post {
		g.renderConsent(w, r, u.Username, a, v)
		return
	}
	if v.Get("action") != "allow" {
		g.logf("gateway: oauth: %s didn't let %s use %s", u.Username, label, a.rt.Name)
		g.authorizeRedirect(w, r, a, url.Values{"error": {"access_denied"}, "error_description": {"the user didn't allow it"}})
		return
	}
	code, err := g.oauthCodes.mint(oauthCode{
		user: u.Username, app: a.rt.Name, clientID: a.clientID, clientName: a.meta.ClientName,
		redirectURI: a.redirectURI, challenge: a.challenge,
	}, g.now())
	if err != nil {
		g.logf("gateway: oauth: %v", err)
		g.message(w, r, http.StatusServiceUnavailable, "Try again", "Too many connections are waiting to finish. Try again in a minute.", g.portalLink(), "Back to apps")
		return
	}
	g.logf("gateway: oauth: %s let %s use %s", u.Username, label, a.rt.Name)
	g.authorizeRedirect(w, r, a, url.Values{"code": {code}})
}

// authorizeRedirect sends the browser back to the client with the answer,
// state and iss (RFC 9207) added to its redirect_uri's own query.
func (g *Gateway) authorizeRedirect(w http.ResponseWriter, r *http.Request, a authorizeRequest, answer url.Values) {
	u, err := url.Parse(a.redirectURI)
	if err != nil {
		g.message(w, r, http.StatusBadRequest, "Can't connect", "The client's redirect address can't be read.", g.portalLink(), "Back to apps")
		return
	}
	q := u.Query()
	for k, vs := range answer {
		q[k] = vs
	}
	if a.state != "" {
		q.Set("state", a.state)
	}
	q.Set("iss", oauthIssuer(a.rt))
	u.RawQuery = q.Encode()
	// The code is in the URL; the page it came from has nothing to add.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (g *Gateway) renderConsent(w http.ResponseWriter, r *http.Request, user string, a authorizeRequest, v url.Values) {
	cv := &consentView{
		Client: ClientLabel(a.meta.ClientName, a.clientID), ClientID: a.clientID,
		App: a.rt.Name, AppTitle: firstNonEmpty(a.rt.Title, titleCase(a.rt.Name)), User: user,
	}
	if cu, err := url.Parse(a.clientID); err == nil {
		cv.ClientHost = cu.Host
	}
	if ru, err := url.Parse(a.redirectURI); err == nil && ru.Host != cv.ClientHost {
		cv.Redirect = ru.Host
	}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "scope", "resource"} {
		if val := v.Get(k); val != "" {
			cv.Params = append(cv.Params, consentParam{k, val})
		}
	}
	// render's headers keep it out of frames. Its Referrer-Policy stays
	// same-origin, as on every portal page, so the form's POST still carries
	// the Origin sameOrigin checks.
	g.render(w, r, http.StatusOK, "authorize", pageData{Title: "Connect " + cv.Client + "?", Consent: cv})
}
