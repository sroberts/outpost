package gateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

const jwksPath = "/.well-known/jwks.json"

const (
	// tokenLife is how long an identity token is valid. It travels with one
	// request, so it only has to outlast clock skew and a slow app.
	tokenLife = 60 * time.Second
	// tokenReuse is how long one token serves a (user, app) pair, so
	// signing isn't per-request work.
	tokenReuse = 30 * time.Second
	// maxCachedTokens bounds the cache; past it, stale entries are dropped.
	maxCachedTokens = 1024
)

// identityClaims is the X-Outpost-Identity payload.
type identityClaims struct {
	Iss    string   `json:"iss"`
	Aud    string   `json:"aud"`
	Sub    string   `json:"sub"`
	Name   string   `json:"name,omitempty"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups"`
	Iat    int64    `json:"iat"`
	Exp    int64    `json:"exp"`
}

// signToken makes a compact JWS (RFC 7515) signed with EdDSA (RFC 8037).
func signToken(key auth.SigningKey, c identityClaims) (string, error) {
	header, err := json.Marshal(struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid"`
	}{"EdDSA", "JWT", key.ID})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	sig := ed25519.Sign(key.Private, []byte(signing))
	return signing + "." + enc.EncodeToString(sig), nil
}

type cachedToken struct {
	token  string
	minted time.Time
}

type tokenCache struct {
	mu sync.Mutex
	m  map[string]cachedToken
}

// identityToken returns a token for u at app, reusing a recent one.
func (g *Gateway) identityToken(u auth.User, app string) (string, error) {
	now := g.now()
	key := u.Username + "\x00" + app
	g.tokens.mu.Lock()
	if c, ok := g.tokens.m[key]; ok && now.Sub(c.minted) < tokenReuse && !now.Before(c.minted) {
		g.tokens.mu.Unlock()
		return c.token, nil
	}
	g.tokens.mu.Unlock()

	sk, err := g.auth.CurrentKey()
	if err != nil {
		return "", err
	}
	groups := u.Groups
	if groups == nil {
		groups = []string{}
	}
	tok, err := signToken(sk, identityClaims{
		Iss:    g.portalOrigin,
		Aud:    g.appHost(app),
		Sub:    u.Username,
		Name:   u.DisplayName,
		Email:  u.Email,
		Groups: groups,
		Iat:    now.Unix(),
		Exp:    now.Add(tokenLife).Unix(),
	})
	if err != nil {
		return "", err
	}

	g.tokens.mu.Lock()
	defer g.tokens.mu.Unlock()
	if g.tokens.m == nil {
		g.tokens.m = map[string]cachedToken{}
	}
	if len(g.tokens.m) >= maxCachedTokens {
		for k, c := range g.tokens.m {
			if now.Sub(c.minted) >= tokenReuse {
				delete(g.tokens.m, k)
			}
		}
	}
	if len(g.tokens.m) < maxCachedTokens {
		g.tokens.m[key] = cachedToken{token: tok, minted: now}
	}
	return tok, nil
}

// serveJWKS publishes the public keys apps verify tokens with.
func (g *Gateway) serveJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	keys, err := g.auth.PublishedKeys()
	if err != nil {
		g.logf("gateway: jwks: %v", err)
		http.Error(w, "keys unavailable", http.StatusInternalServerError)
		return
	}
	body, err := auth.JWKS(keys)
	if err != nil {
		g.logf("gateway: jwks: %v", err)
		http.Error(w, "keys unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Short enough that a rotated key is picked up well inside its day of
	// grace.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body) // a client that hung up has nothing to be told
}
