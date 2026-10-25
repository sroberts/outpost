package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// SigningKey is one Ed25519 key for identity tokens.
type SigningKey struct {
	ID        string // kid: base64url(sha256(pub))[:16]
	Private   ed25519.PrivateKey
	CreatedAt time.Time
	RetiredAt time.Time // zero while current
}

// Public returns the key's public half.
func (k SigningKey) Public() ed25519.PublicKey {
	return k.Private.Public().(ed25519.PublicKey)
}

// keyID derives a kid from the public key, so the same key always has the
// same id and an id can't be reused for a different key.
func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}

// GenerateKey makes a new signing key.
func GenerateKey(now time.Time) (SigningKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKey{}, fmt.Errorf("generate signing key: %w", err)
	}
	return SigningKey{ID: keyID(pub), Private: priv, CreatedAt: now}, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS renders the public halves of keys as a JSON Web Key Set (RFC 8037),
// which is what an app fetches to verify X-Outpost-Identity.
func JWKS(keys []SigningKey) ([]byte, error) {
	set := struct {
		Keys []jwk `json:"keys"`
	}{Keys: make([]jwk, 0, len(keys))}
	for _, k := range keys {
		if len(k.Private) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signing key %s is malformed", k.ID)
		}
		set.Keys = append(set.Keys, jwk{
			Kty: "OKP",
			Crv: "Ed25519",
			X:   base64.RawURLEncoding.EncodeToString(k.Public()),
			Kid: k.ID,
			Alg: "EdDSA",
			Use: "sig",
		})
	}
	return json.Marshal(set)
}
