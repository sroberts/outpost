// Package apptoken makes and recognises the tokens the daemon's loopback
// services (AI, mail) hand to the apps they serve.
//
// A token isn't stored. It is an HMAC of the app's name and a generation
// under a key the service keeps, so the same token is handed to the app every
// time it starts, including after the daemon restarts, without keeping it in a
// form that could be read back. Regenerating bumps the generation.
package apptoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
)

// Mint is service's token at generation gen under key, with prefix in front
// so a token says which service it is for.
func Mint(prefix string, key []byte, service string, gen int64) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(service))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(gen, 10)))
	return prefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Issued remembers which app each handed-out token belongs to. It keeps the
// tokens' hashes, not the tokens.
type Issued struct {
	prefix string
	mu     sync.Mutex
	m      map[[32]byte]string
}

// NewIssued is an empty set for tokens that start with prefix.
func NewIssued(prefix string) *Issued {
	return &Issued{prefix: prefix, m: map[[32]byte]string{}}
}

// Add records tok as service's.
func (i *Issued) Add(tok, service string) {
	i.mu.Lock()
	i.m[sha256.Sum256([]byte(tok))] = service
	i.mu.Unlock()
}

// Remove forgets tok, which stops working at once.
func (i *Issued) Remove(tok string) {
	i.mu.Lock()
	delete(i.m, sha256.Sum256([]byte(tok)))
	i.mu.Unlock()
}

// Service is the app tok was handed to.
func (i *Issued) Service(tok string) (string, bool) {
	if !strings.HasPrefix(tok, i.prefix) {
		return "", false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	svc, ok := i.m[sha256.Sum256([]byte(tok))]
	return svc, ok
}
