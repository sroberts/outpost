package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sroberts/outpost"
	"github.com/sroberts/outpost/internal/version"
)

// The service contract and a description of this server, so a developer, or a
// coding agent working for one, can ask a running Outpost what it supports
// instead of guessing from a copy of the docs that may be out of date.
const (
	// contractSpecPath is the contract itself. It is the text in the public
	// repository, so it is public here too.
	contractSpecPath = "/.well-known/outpost-service-spec.md"
	// capabilitiesPath says which build is running and what is turned on.
	// The version is shown to nobody who isn't signed in, so this takes a
	// session or an access token like the portal's pages do.
	capabilitiesPath = "/.well-known/outpost.json"
)

// isContractPath reports whether p is one of the contract's addresses, which
// the portal answers before it refuses access tokens.
func isContractPath(p string) bool {
	return p == contractSpecPath || p == capabilitiesPath
}

// serveContract answers the contract's addresses on the portal.
func (g *Gateway) serveContract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == contractSpecPath {
		g.serveContractSpec(w, r)
		return
	}
	g.serveCapabilities(w, r)
}

// serveContractSpec serves the contract with its revision as the ETag, so a
// client that has the current one gets a 304 and nothing else.
func (g *Gateway) serveContractSpec(w http.ResponseWriter, r *http.Request) {
	rev := outpost.ContractRevision()
	etag := `"` + rev + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Outpost-Contract-Revision", rev)
	// Revalidated each time: the ETag makes that cheap, and an upgrade
	// changes the text.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(outpost.ServiceSpec())) // a client that hung up has nothing to be told
}

// capabilities is the document at capabilitiesPath. Features name the
// contract's section, so a reader can go straight to what applies.
type capabilities struct {
	Outpost  capOutpost   `json:"outpost"`
	Contract capContract  `json:"contract"`
	Identity capIdentity  `json:"identity"`
	Features []capFeature `json:"features"`
}

type capOutpost struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

type capContract struct {
	Revision string `json:"revision"`
	URL      string `json:"url"`
}

type capIdentity struct {
	Issuer  string `json:"issuer"`
	JWKSURL string `json:"jwks_url"`
}

// capFeature is a part of the contract this build implements. It says
// nothing about whether an operator has set the feature up: that is theirs to
// do per service, and an app should cope with it being absent either way.
type capFeature struct {
	Name    string `json:"name"`
	Section string `json:"section"`
}

// contractFeatures are what an app can build on, in the contract's order.
var contractFeatures = []capFeature{
	{Name: "tcp_forwards", Section: "1. Protocols that aren't HTTP"},
	{Name: "gateway_identity", Section: "3. Behind the login gateway"},
	{Name: "public_paths", Section: "3. Public paths and webhooks"},
	{Name: "secrets", Section: "3. Secrets"},
	{Name: "github_release_install", Section: "14. Publishing releases outpost add can install"},
	{Name: "ai_models", Section: "15. Using the operator's AI models"},
	{Name: "email", Section: "16. Using email"},
}

// serveCapabilities describes this server to someone signed in to the portal
// or holding an access token. A request with a token stands or falls by it,
// as everywhere else: a bad one is never served as the session beside it.
// Any token of a user still allowed in will do, whatever apps it is scoped
// to, since it only reads what the portal already shows that user.
func (g *Gateway) serveCapabilities(w http.ResponseWriter, r *http.Request) {
	if presented := presentedAccessTokens(r.Header); len(presented) > 0 {
		removeAccessTokens(r.Header)
		pt, ok := g.lookupAccessToken(w, r, presented, "capabilities")
		if !ok {
			return
		}
		g.touchAccessToken(pt.token)
	} else {
		tok, s, _, ok := g.session(r)
		// An app's session is for that app, not the portal.
		if !ok || s.App != "" {
			// A plain answer rather than the sign-in page: whatever asks
			// for JSON can't follow it.
			g.unauthorizedToken(w)
			return
		}
		g.touch(tok, s)
	}

	v := version.Get()
	// The same issuer services are told in OUTPOST_ISSUER.
	issuer := strings.TrimSuffix(g.cfg.PublicURL, "/")
	doc := capabilities{
		Outpost:  capOutpost{Version: v.Version, Commit: v.Commit},
		Contract: capContract{Revision: outpost.ContractRevision(), URL: g.portalOrigin + contractSpecPath},
		Identity: capIdentity{Issuer: issuer, JWKSURL: issuer + jwksPath},
		Features: contractFeatures,
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		g.logf("gateway: capabilities: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// It says which build is running, which only signed-in users are shown.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(append(body, '\n')) // as above
}
