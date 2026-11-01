package gateway

import (
	"fmt"
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
	// pending is how many attempts are being checked right now. A password
	// check takes long enough that a burst sent together would all pass
	// wait before the first of them is recorded as a failure.
	pending int
}

// wait reports how long until every key may try again; zero means now.
func (t *throttle) wait(now time.Time, keys ...string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.longest(now, keys)
}

func (t *throttle) longest(now time.Time, keys []string) time.Duration {
	var longest time.Duration
	for _, k := range keys {
		if b, ok := t.m[k]; ok {
			d := b.until.Sub(now)
			if b.pending > 0 {
				// One at a time per key: the one in flight is as good as a
				// failure until it's known not to be.
				d = max(d, throttleBase)
			}
			if d > longest {
				longest = d
			}
		}
	}
	return longest
}

// begin is wait followed, when the keys are clear, by taking the attempt:
// in one step, so two attempts can't both find them clear. Every begin that
// returns zero is ended by fail or pass.
func (t *throttle) begin(now time.Time, keys ...string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d := t.longest(now, keys); d > 0 {
		return d
	}
	for _, k := range keys {
		if b := t.entry(now, k); b != nil {
			b.pending++
		}
	}
	return 0
}

// entry is the record for k, made if there's room for one. Nil means the
// table is full of recent failures, and the other keys have to do.
func (t *throttle) entry(now time.Time, k string) *backoff {
	if t.m == nil {
		t.m = map[string]*backoff{}
	}
	if b, ok := t.m[k]; ok {
		return b
	}
	if len(t.m) >= throttleKeys {
		t.forget(now)
		if len(t.m) >= throttleKeys {
			// Full of recent failures: fall back on the other keys
			// rather than grow without bound.
			return nil
		}
	}
	b := &backoff{}
	t.m[k] = b
	return b
}

// fail records a failure against each key, ending the attempt begin took.
func (t *throttle) fail(now time.Time, keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		b := t.entry(now, k)
		if b == nil {
			continue
		}
		b.pending = max(b.pending-1, 0)
		// forget only runs when the table is full, and a success no longer
		// clears an address, so without this an address's failures would add
		// up for as long as the daemon runs: enough typos over months, and one
		// more locks everyone behind it out for a minute.
		if now.Sub(b.last) > throttleForget {
			b.fails = 0
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

// pass ends an attempt begin took without clearing what the key has
// against it: a success from an address says nothing about the failures
// that came from it, which may be someone else's.
func (t *throttle) pass(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		b, ok := t.m[k]
		if !ok {
			continue
		}
		b.pending = max(b.pending-1, 0)
		if b.pending == 0 && b.fails == 0 {
			delete(t.m, k)
		}
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

// waitWords says how long a throttled client has to wait, rounded up.
func waitWords(d time.Duration) string {
	if n := int(d.Seconds() + 0.999); n != 1 {
		return fmt.Sprintf("%d seconds", n)
	}
	return "1 second"
}
