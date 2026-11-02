package gateway

import (
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/sroberts/outpost/internal/config"
)

// mcpPrefix is where the portal serves each app's MCP endpoint, at
// /mcp/<name>, for MCP clients that can reach the portal and hold an access
// token. It is the one way a token works on the portal.
const mcpPrefix = "/mcp/"

// mcpService is the app a request to /mcp/<name> names. Only that exact
// shape matches, in its simplest form: the name is the whole of the last
// segment and a valid DNS label, and nothing is escaped or tucked behind a
// ";". Streamable HTTP has a single endpoint, so /mcp/<name>/anything isn't
// routed, and nor is the old SSE transport's second path.
func mcpService(r *http.Request) (string, bool) {
	p := r.URL.Path
	raw := strings.ToLower(r.URL.EscapedPath())
	if path.Clean(p) != p || raw != strings.ToLower(p) ||
		strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(raw, "%25") ||
		strings.ContainsAny(p, "\\;") {
		return "", false
	}
	name, ok := strings.CutPrefix(p, mcpPrefix)
	if !ok || !config.ValidLabel(name) {
		return "", false
	}
	return name, true
}

// serveMCP carries an MCP client's request on the portal to the app it names,
// at the app's mcp_path. It takes access tokens and nothing else: no session
// cookie, no public path, no sign-in page, since an MCP client can follow
// none of them. Every way a token fails to reach an app, including an app
// that doesn't exist or doesn't serve MCP, is the same 401, counted against
// the address, so a token can't be used to map what is there.
func (g *Gateway) serveMCP(w http.ResponseWriter, r *http.Request) {
	presented := presentedAccessTokens(r.Header)
	// Whatever happens next, the token goes no further than here.
	removeAccessTokens(r.Header)

	name, ok := mcpService(r)
	if !ok {
		// A shape no app could have says nothing about which apps exist.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if len(presented) == 0 {
		// Nothing was tried, so there is nothing to count; a valid session
		// cookie doesn't count either, as an MCP client never has one that a
		// browser didn't give it.
		g.unauthorizedToken(w)
		return
	}

	pt, ok := g.lookupAccessToken(w, r, presented, "mcp/"+name)
	if !ok {
		return
	}
	u := pt.user
	rt, found := g.routes.get(name)
	if !found || rt.MCPPath == "" || !pt.token.Reaches(name) {
		pt.reject(fmt.Sprintf("for %s to %s over MCP from %s", u.Username, name, pt.ip))
		return
	}
	if !allowed(u, rt) {
		g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", u.Username, rt.Name), false)
		http.Error(w, "this account can't open this app", http.StatusForbidden)
		return
	}
	g.touchAccessToken(pt.token)

	// The app sees the request it would get on its own address: its own
	// host, which the identity token's audience is too, at its own path.
	g.proxyAsTo(w, r, rt, u, target{host: g.appHost(rt.Name), path: rt.MCPPath, mcp: true})
}

// onPortalOrigin makes an app's MCP response safe to serve on the portal's
// origin. A browser can't send a token without a preflight, which never gets
// past the gateway, but a cache in front of the gateway could hand a stored
// response to one: an app answering with HTML would then run script as the
// portal, with the signed-in user's session behind it. So the response is
// never stored, never sniffed, and never a document with an origin, and the
// headers that would let an app set the portal origin's policy are dropped.
func onPortalOrigin(h http.Header) {
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			delete(h, k)
		}
	}
	for _, k := range []string{"Clear-Site-Data", "Strict-Transport-Security", "Refresh", "Link"} {
		h.Del(k)
	}
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
}

// oauthDiscovery reports whether p is one of the addresses an MCP client
// tries, to find out how to sign in, once a token stops working. The portal
// has no OAuth server, and its usual answer for an unknown path is a page,
// which a client could take for one.
func oauthDiscovery(p string) bool {
	for _, base := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		if p == base || strings.HasPrefix(p, base+"/") {
			return true
		}
	}
	return false
}
