package gateway

import (
	"errors"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// codeTTL is how long a one-time code from the portal is good for. It only
// has to survive one redirect.
const codeTTL = 60 * time.Second

// maxPendingCodes bounds what a logged-in client can make the gateway hold.
const maxPendingCodes = 10000

// pendingCode is what a one-time code stands for: this user, signed in to
// this portal session, may open this app.
type pendingCode struct {
	user    string
	app     string
	parent  string // the portal session token
	expires time.Time
}

// codeStore holds codes in memory, keyed by their hash. They last a minute,
// a restart ending them is harmless, and keeping them off disk means a copy
// of auth.db never holds one.
type codeStore struct {
	mu sync.Mutex
	m  map[string]pendingCode
}

var errTooManyCodes = errors.New("too many sign-ins in progress; try again shortly")

func (c *codeStore) mint(p pendingCode, now time.Time) (string, error) {
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]pendingCode{}
	}
	if len(c.m) >= maxPendingCodes {
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxPendingCodes {
			return "", errTooManyCodes
		}
	}
	p.expires = now.Add(codeTTL)
	c.m[auth.HashToken(tok)] = p
	return tok, nil
}

// redeem uses up a code. It's gone after the first try, right app or not:
// a code turning up on the wrong host is no reason to keep it alive.
func (c *codeStore) redeem(code, app string, now time.Time) (pendingCode, bool) {
	h := auth.HashToken(code)
	c.mu.Lock()
	p, ok := c.m[h]
	delete(c.m, h)
	c.mu.Unlock()
	if !ok || !now.Before(p.expires) || p.app != app {
		return pendingCode{}, false
	}
	return p, true
}
