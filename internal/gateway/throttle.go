package gateway

import (
	"sync"
	"time"
)

const (
	throttleBase = time.Second
	throttleMax  = 60 * time.Second
	// throttleForget is how long after its last failure a key is dropped.
	throttleForget = 15 * time.Minute
	// throttleKeys bounds the table, which anyone can grow by guessing.
	throttleKeys = 10000
)

// throttle backs off failed attempts per key (a username, a client address):
// 1s after the first failure past the free allowance, doubling to a minute.
type throttle struct {
	// free is how many failures a key gets before backing off starts.
	free int

	mu sync.Mutex
	m  map[string]*backoff
}

type backoff struct {
	fails int
	until time.Time
	last  time.Time
}

// wait reports how long until every key may try again; zero means now.
func (t *throttle) wait(now time.Time, keys ...string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var longest time.Duration
	for _, k := range keys {
		if b, ok := t.m[k]; ok {
			if d := b.until.Sub(now); d > longest {
				longest = d
			}
		}
	}
	return longest
}

// fail records a failure against each key.
func (t *throttle) fail(now time.Time, keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*backoff{}
	}
	if len(t.m) >= throttleKeys {
		t.forget(now)
	}
	for _, k := range keys {
		b, ok := t.m[k]
		if !ok {
			if len(t.m) >= throttleKeys {
				// Full of recent failures: fall back on the other keys
				// rather than grow without bound.
				continue
			}
			b = &backoff{}
			t.m[k] = b
		}
		b.fails++
		b.last = now
		over := b.fails - t.free
		if over <= 0 {
			continue
		}
		d := throttleMax
		if over <= 7 { // 1s << 6 is already past the cap
			d = min(throttleBase<<(over-1), throttleMax)
		}
		b.until = now.Add(d)
	}
}

// reset clears keys after a success.
func (t *throttle) reset(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		delete(t.m, k)
	}
}

func (t *throttle) forget(now time.Time) {
	for k, b := range t.m {
		if now.Sub(b.last) > throttleForget {
			delete(t.m, k)
		}
	}
}
