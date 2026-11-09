package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sroberts/outpost"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/version"
)

// The contract is public, served whole, and revalidated by its revision.
func TestContractSpec_PublicWithRevisionETag(t *testing.T) {
	h := newHarness(t)

	resp := h.get(h.client(false), portal+contractSpecPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != outpost.ServiceSpec() {
		t.Error("body isn't the embedded contract")
	}
	etag := resp.Header.Get("ETag")
	if etag != `"`+outpost.ContractRevision()+`"` {
		t.Errorf("ETag = %q", etag)
	}

	again := h.get(h.client(false), portal+contractSpecPath, "If-None-Match", etag)
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation status = %d; want 304", again.StatusCode)
	}
}

// The description says which build runs, so nobody anonymous gets it, and the
// answer is a plain 401 a script can read, not the sign-in page.
func TestCapabilities_RefusesAnonymous(t *testing.T) {
	h := newHarness(t)
	assertRefused(t, h.get(h.client(false), portal+capabilitiesPath))
}

func TestCapabilities_TokenOrSessionGetsTheDescription(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})

	check := func(t *testing.T, resp *http.Response) {
		t.Helper()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d; want 200", resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q; want no-store", cc)
		}
		var doc capabilities
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		if doc.Outpost.Version != version.Get().Version {
			t.Errorf("version = %q", doc.Outpost.Version)
		}
		if doc.Contract.Revision != outpost.ContractRevision() {
			t.Errorf("revision = %q", doc.Contract.Revision)
		}
		if doc.Contract.URL != portal+contractSpecPath {
			t.Errorf("contract url = %q", doc.Contract.URL)
		}
		if doc.Identity.JWKSURL != portal+jwksPath {
			t.Errorf("jwks url = %q", doc.Identity.JWKSURL)
		}
		if len(doc.Features) == 0 {
			t.Error("no features listed")
		}
	}

	t.Run("token, scoped to some app", func(t *testing.T) {
		check(t, h.send(http.MethodGet, portal+capabilitiesPath, bearer(h.pat("alice", "yarr"))))
	})
	t.Run("signed in", func(t *testing.T) {
		c := h.client(true)
		if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
		check(t, h.get(c, portal+capabilitiesPath))
	})
}

// A wrong token is refused even beside a good session: it must not read as
// the session, any more than it would on an app.
func TestCapabilities_BadTokenStandsOrFallsByIt(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	assertRefused(t, h.get(c, portal+capabilitiesPath, "Authorization", "Bearer "+auth.AccessTokenPrefix+"nope"))
}

func TestCapabilities_DisabledUsersTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	tok := h.pat("alice")
	if err := h.store.SetDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, h.send(http.MethodGet, portal+capabilitiesPath, bearer(tok)))
}

// Only the portal answers these: on an app's host the path is the app's.
func TestContract_AppHostPathsBelongToTheApp(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	tok := h.pat("alice")
	for _, p := range []string{contractSpecPath, capabilitiesPath} {
		if got := backendSaw(t, h.send(http.MethodGet, yarr+p, bearer(tok))).Path; got != p {
			t.Errorf("backend saw %q; want %q", got, p)
		}
	}
}

func TestContract_OnlyReads(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{contractSpecPath, capabilitiesPath} {
		resp := h.send(http.MethodPost, portal+p, nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d; want 405", p, resp.StatusCode)
		}
	}
}
