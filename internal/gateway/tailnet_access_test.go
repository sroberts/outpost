package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

const nodeSecret = "s3cret-for-nodes"

// accessHarness is the usual harness with node questions turned on, and a
// checker that asks them the way a node does.
func accessHarness(t *testing.T) (*harness, *outposttsnet.AccessChecker) {
	t.Helper()
	h := newHarness(t)
	h.g.nodeSecret = nodeSecret
	c, err := outposttsnet.NewAccessChecker(h.front.URL, "", nodeSecret)
	if err != nil {
		t.Fatal(err)
	}
	return h, c
}

func (h *harness) setAllow(service string, allow ...string) {
	h.t.Helper()
	svcs := h.g.services.(*fakeServices)
	svcs.mu.Lock()
	for i := range svcs.list {
		if svcs.list[i].Name == service {
			svcs.list[i].Allow = allow
		}
	}
	svcs.mu.Unlock()
	h.g.refreshRoutes(context.Background())
}

func refusedFor(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, outposttsnet.ErrAccessRefused) {
		t.Fatalf("err = %v; want a refusal", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q doesn't say %q", err, want)
	}
}

func TestTailnetAccess_LinkedLoginIsItsUser(t *testing.T) {
	h, c := accessHarness(t)
	h.addUser(auth.User{Username: "alice"})
	if err := h.store.SetTailnetLogin("alice", "Alice@GitHub"); err != nil {
		t.Fatal(err)
	}
	user, err := c.Check(context.Background(), "yarr", "alice@github")
	if err != nil || user != "alice" {
		t.Fatalf("Check = %q, %v; want alice", user, err)
	}
}

func TestTailnetAccess_EmailCountsWithoutALink(t *testing.T) {
	h, c := accessHarness(t)
	h.addUser(auth.User{Username: "bob", Email: "Bob@Example.com"})
	user, err := c.Check(context.Background(), "yarr", "bob@example.com")
	if err != nil || user != "bob" {
		t.Fatalf("Check = %q, %v; want bob", user, err)
	}
}

func TestTailnetAccess_Refusals(t *testing.T) {
	h, c := accessHarness(t)
	h.addUser(auth.User{Username: "admin", Admin: true})
	h.addUser(auth.User{Username: "carol", Email: "carol@example.com"})
	h.addUser(auth.User{Username: "dave", Email: "dave@example.com"})
	if err := h.store.SetDisabled("dave", true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err := c.Check(ctx, "yarr", "stranger@example.com")
	refusedFor(t, err, "not an Outpost user's")

	_, err = c.Check(ctx, "yarr", "dave@example.com")
	refusedFor(t, err, "disabled")

	_, err = c.Check(ctx, "nosuch", "carol@example.com")
	refusedFor(t, err, "no such service")

	_, err = c.Check(ctx, "yarr", "")
	refusedFor(t, err, "no tailnet login")

	// The app's allow list applies, as it does to a signed-in request.
	h.setAllow("yarr", "group:ops")
	_, err = c.Check(ctx, "yarr", "carol@example.com")
	refusedFor(t, err, "allow list")
	if _, err := h.store.UpdateGroups("carol", []string{"ops"}, nil); err != nil {
		t.Fatal(err)
	}
	if user, err := c.Check(ctx, "yarr", "carol@example.com"); err != nil || user != "carol" {
		t.Errorf("carol in ops: Check = %q, %v; want carol", user, err)
	}
}

func TestTailnetAccess_OnlyTheDaemonsNodesMayAsk(t *testing.T) {
	h, _ := accessHarness(t)
	h.addUser(auth.User{Username: "erin", Email: "erin@example.com"})
	form := url.Values{"service": {"yarr"}, "login": {"erin@example.com"}}

	ask := func(secret string) int {
		req, err := http.NewRequest(http.MethodPost, h.front.URL+outposttsnet.AccessPath, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if secret != "" {
			req.Header.Set(outposttsnet.AccessSecretHeader, secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := ask(""); got != http.StatusNotFound {
		t.Errorf("no secret = %d; want 404", got)
	}
	if got := ask("wrong"); got != http.StatusNotFound {
		t.Errorf("wrong secret = %d; want 404", got)
	}
	if got := ask(nodeSecret); got != http.StatusOK {
		t.Errorf("right secret = %d; want 200", got)
	}

	// With no secret configured nothing may ask, not even with an empty one.
	h.g.nodeSecret = ""
	if got := ask(""); got != http.StatusNotFound {
		t.Errorf("gateway without a node secret = %d; want 404", got)
	}
}

func TestTailnetAccess_NotFromAnotherMachine(t *testing.T) {
	if fromLoopback("100.64.0.7:41000") || fromLoopback("[fd7a:115c:a1e0::1]:22") || fromLoopback("garbage") {
		t.Error("a tailnet address counted as loopback")
	}
	if !fromLoopback("127.0.0.1:5000") || !fromLoopback("[::1]:5000") || !fromLoopback("[::ffff:127.0.0.1]:5000") {
		t.Error("loopback did not count as loopback")
	}
}
