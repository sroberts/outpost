package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// feed collects what the gateway records.
type feed struct {
	mu     sync.Mutex
	events []activityEvent
}

func (f *feed) RecordEvent(_ context.Context, service, kind, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if service != ActivityService {
		return nil
	}
	f.events = append(f.events, activityEvent{kind: kind, message: message})
	return nil
}

// wait returns the events once there are n, failing after a while.
func (f *feed) wait(t *testing.T, n int) []activityEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := append([]activityEvent(nil), f.events...)
		f.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			if len(got) != n {
				t.Fatalf("got %d events, want %d: %+v", len(got), n, got)
			}
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recording gives the harness's gateway a feed, as the daemon does.
func (h *harness) recording() *feed {
	f := &feed{}
	h.g.activity = newActivity(f, h.t.Logf, func() time.Time { return h.g.now() })
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	go h.g.activity.run(ctx)
	return f
}

// Failed sign-ins are recorded once, then counted, with the name someone
// typed shown safely; a success settles the count.
func TestActivity_SignInsAndFailures(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	f := h.recording()
	c := h.client(false)

	bad := url.Values{"username": {"alice"}, "password": {"wrong password"}}
	for range 3 {
		h.post(c, portal+"/login", portal, bad)
	}
	h.post(c, portal+"/login", portal, url.Values{"username": {"mallory\x1b[2J"}, "password": {"x"}})
	// Past the first failure, attempts are throttled, and counted as such.

	got := f.wait(t, 2)
	if got[0].kind != EventSignInFail || got[0].message != `failed sign-in for "alice" from 127.0.0.1` {
		t.Errorf("first = %+v", got[0])
	}
	if strings.ContainsRune(got[1].message, '\x1b') || !strings.Contains(got[1].message, `"mallory\x1b[2j"`) {
		t.Errorf("typed name not escaped: %q", got[1].message)
	}

	// Clear the address's backoff so the real password is checked.
	h.g.throttle.reset("ip:127.0.0.1", "user:alice")
	if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	got = f.wait(t, 4)
	if !strings.HasPrefix(got[2].message, `2 throttled for "alice" from 127.0.0.1 since `) {
		t.Errorf("summary = %q", got[2].message)
	}
	if got[3].kind != EventSignIn || got[3].message != "alice signed in from 127.0.0.1" {
		t.Errorf("sign-in = %+v", got[3])
	}
}

// A refused app is recorded once, and its repeats counted, rather than once
// per request a page makes.
func TestActivity_Denials(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "", "/")
	backendSaw(t, h.get(c, yarr+"/"))
	f := h.recording()

	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", Allow: []string{"bob"}})
	for range 3 {
		if resp := h.get(c, yarr+"/"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("got %d; want 403", resp.StatusCode)
		}
	}
	got := f.wait(t, 1)
	if got[0].kind != EventDenied || got[0].message != "access denied for alice to yarr" {
		t.Errorf("denial = %+v", got[0])
	}

	// When the window closes, the repeats are one line.
	h.g.activity.mu.Lock()
	h.g.activity.flushLocked(time.Now().Add(activityWindow+time.Minute), false)
	h.g.activity.mu.Unlock()
	got = f.wait(t, 2)
	if !strings.HasPrefix(got[1].message, "access denied 2 more times for alice to yarr since ") {
		t.Errorf("summary = %q", got[1].message)
	}
}

func TestActivity_SignOutAndPassword(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	h.login(c, "alice", "", "/")
	f := h.recording()

	next := "a much better passphrase"
	h.post(c, portal+"/account", portal, url.Values{
		"action": {"password"}, "current": {pw}, "password": {next}, "confirm": {next},
	})
	h.post(c, portal+"/logout", portal, nil)
	got := f.wait(t, 2)
	if got[0].kind != EventPassword || !strings.HasPrefix(got[0].message, "alice changed their password from 127.0.0.1") {
		t.Errorf("password = %+v", got[0])
	}
	if got[1].kind != EventSignOut || got[1].message != "alice signed out from 127.0.0.1" {
		t.Errorf("sign-out = %+v", got[1])
	}
}

// With nothing to record to, as in a test or a daemon without one, the
// gateway carries on.
func TestActivity_NoSink(t *testing.T) {
	var a *activity
	a.record(EventSignIn, "x")
	a.failed(EventSignInFail, "y", false)
	a.settle(EventSignInFail, "y")
	newActivity(nil, t.Logf, time.Now).record(EventSignIn, "x")
}

func TestActivity_SummaryWording(t *testing.T) {
	since := time.Date(2026, 1, 2, 9, 5, 0, 0, time.Local)
	cases := []struct {
		rp   repeat
		want string
	}{
		{repeat{kind: EventSignInFail, subject: `for "a" from ip`, since: since, count: 1}, `1 more failed sign-in for "a" from ip since 09:05`},
		{repeat{kind: EventSignInFail, subject: `for "a" from ip`, since: since, count: 3, throttled: 2}, `3 more failed sign-ins and 2 throttled for "a" from ip since 09:05`},
		{repeat{kind: EventDenied, subject: "for a to b", since: since, count: 1}, "access denied 1 more time for a to b since 09:05"},
	}
	for _, tc := range cases {
		if got := summary(&tc.rp); got != tc.want {
			t.Errorf("got %q\nwant %q", got, tc.want)
		}
	}
}
