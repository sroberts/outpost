package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy in every session token and one-time code.
const tokenBytes = 32

// NewToken returns a fresh random token: 32 bytes, base64url without padding,
// so it can go in a cookie or a query string as is.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is what the database stores in place of a token, so a copy of
// auth.db doesn't yield usable cookies. A plain SHA-256 is enough: the input
// is 256 random bits, not something a dictionary could guess.
func HashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
