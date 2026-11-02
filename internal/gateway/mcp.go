package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/sroberts/outpost/internal/config"
)

// mcpPrefix is where the portal serves each app's MCP endpoint, at
// /mcp/<name>, for MCP clients that can reach the portal and hold an access
// token. It is the one way a token works on the portal.
const mcpPrefix = "/mcp/"

// mcpService is the app a request to /mcp/<name> names. Only that exact
// shape matches, in its simplest form: the name is the whole of the last
// segment and a valid DNS label, and nothing is escaped. Streamable HTTP has a
// single endpoint, so /mcp/<name>/anything isn't routed, and nor is the old
// SSE transport's second path.
//
// A DNS label has no "/", ".", "\", ";" or "%", so it alone rules out every
// trick with segments, and a path sent with nothing escaped is its own
// escaped form, so comparing the two rules out escaping, %2f and %25 included.
func mcpService(r *http.Request) (string, bool) {
	p := r.URL.Path
	name, ok := strings.CutPrefix(p, mcpPrefix)
	if !ok || !config.ValidLabel(name) || r.URL.EscapedPath() != p {
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
	// host, which the identity token's audience is too, at its own path. The
	// host is taken from its origin rather than appHost, so an app off the
	// tailnet gets the portal's port with it, as a browser's request would
	// carry, and builds absolute URLs that reach it.
	_, host, _ := strings.Cut(g.appOrigin(rt.Name), "://")
	g.proxyAsTo(w, r, rt, u, target{host: host, path: rt.MCPPath, mcp: true})
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
