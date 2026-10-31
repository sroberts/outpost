package gateway

import (
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// A burst of wrong passwords sent together used to all pass the throttle,
// because each was checked before the first was recorded. Now the first
// takes the attempt and the rest are told to wait, however many there are.
func TestLoginThrottle_BurstGetsOneAttempt(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	now := time.Now()
	h.g.now = func() time.Time { return now }

	const burst = 24
	var wg sync.WaitGroup
	codes := make(chan int, burst)
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := h.post(h.client(false), portal+"/login", portal,
				url.Values{"username": {"alice"}, "password": {"wrong password!"}})
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	var checked, throttled int
	for c := range codes {
		switch c {
		case http.StatusUnauthorized:
			checked++
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Errorf("got %d", c)
		}
	}
	if checked != 1 || throttled != burst-1 {
		t.Errorf("%d guesses checked and %d throttled; want 1 and %d", checked, throttled, burst-1)
	}
}

// Signing in to one account mustn't clear the address's backoff: whoever
// holds any account could otherwise spray one guess per name for ever.
func TestLoginThrottle_SuccessKeepsTheAddressBackoff(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	now := time.Now()
	h.g.now = func() time.Time { return now }
	c := h.client(false)

	wrong := func(user string) int {
		return h.post(c, portal+"/login", portal, url.Values{"username": {user}, "password": {"wrong password!"}}).StatusCode
	}
	if got := wrong("alice"); got != http.StatusUnauthorized {
		t.Fatalf("first guess = %d", got)
	}
	now = now.Add(2 * time.Second)
	if resp := h.login(c, "bob", "", "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bob signing in = %d", resp.StatusCode)
	}
	if d := h.g.throttle.wait(now, "user:bob"); d != 0 {
		t.Errorf("bob's own key waits %v after signing in", d)
	}
	// The address's earlier failure still counts, so this one is the second.
	if got := wrong("carol"); got != http.StatusUnauthorized {
		t.Fatalf("guess after a sign-in = %d", got)
	}
	if d := h.g.throttle.wait(now, "ip:127.0.0.1"); d != 2*time.Second {
		t.Errorf("address backoff after a sign-in and a second failure = %v; want 2s", d)
	}
}

func TestThrottle_BeginAndPass(t *testing.T) {
	var th throttle
	now := time.Now()
	if d := th.begin(now, "a", "b"); d != 0 {
		t.Fatalf("first begin waits %v", d)
	}
	if d := th.begin(now, "a"); d != throttleBase {
		t.Errorf("begin while one is in flight waits %v; want %v", d, throttleBase)
	}
	if d := th.wait(now, "b"); d != throttleBase {
		t.Errorf("wait while one is in flight = %v; want %v", d, throttleBase)
	}
	th.pass("a", "b")
	if d := th.begin(now, "a", "b"); d != 0 {
		t.Errorf("begin after a pass waits %v", d)
	}
	if _, ok := th.m["a"]; !ok {
		t.Fatal("no entry while in flight")
	}
	th.fail(now, "a", "b")
	if d := th.wait(now, "a"); d != throttleBase {
		t.Errorf("after one failure: wait %v; want %v", d, throttleBase)
	}
	if th.m["a"].pending != 0 {
		t.Errorf("pending after fail = %d", th.m["a"].pending)
	}
	// A pass on a key with failures keeps them.
	now = now.Add(time.Second)
	if d := th.begin(now, "a"); d != 0 {
		t.Fatalf("begin after the backoff waits %v", d)
	}
	th.pass("a")
	if th.m["a"] == nil || th.m["a"].fails != 1 {
		t.Error("pass cleared the key's failures")
	}
	// A pass on a key with none drops it, so a clean key leaves no trace.
	th.begin(now, "c")
	th.pass("c")
	if _, ok := th.m["c"]; ok {
		t.Error("a passed key with no failures was kept")
	}
	// pass and fail on a key that was never begun don't go negative.
	th.pass("zz")
	th.fail(now, "zz")
	if th.m["zz"].pending != 0 {
		t.Errorf("pending = %d", th.m["zz"].pending)
	}
}
