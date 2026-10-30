package gateway

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

func TestAccountListsSignInsAndEndsOne(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	here, phone := h.client(true), h.client(true)
	h.login(here, "alice", "", "/")
	phoneUA := "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1"
	req := func(c *http.Client, method, rawURL string, form url.Values) *http.Response {
		t.Helper()
		r, err := http.NewRequest(method, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("User-Agent", phoneUA)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", portal)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	req(phone, http.MethodPost, portal+"/login", url.Values{"username": {"alice"}, "password": {pw}, "app": {"yarr"}})
	backendSaw(t, h.get(phone, yarr+"/"))

	b := body(t, h.get(here, portal+"/account"))
	for _, want := range []string{"Safari on iPhone", "You're using this one", "Active in the last day", "Apps: yarr"} {
		if !strings.Contains(b, want) {
			t.Errorf("account page lacks %q:\n%s", want, b)
		}
	}
	if n := strings.Count(b, `name="action" value="end"`); n != 1 {
		t.Errorf("%d end buttons; want one, and none for this device", n)
	}

	list, err := h.store.SignIns("alice")
	if err != nil {
		t.Fatal(err)
	}
	var target string
	for _, in := range list {
		if strings.Contains(in.UserAgent, "iPhone") {
			target = in.ID
		}
	}
	if target == "" {
		t.Fatalf("no phone sign-in in %+v", list)
	}

	end := url.Values{"action": {"end"}, "id": {target}}
	if resp := h.post(h.client(true), portal+"/account", "http://evil.test", end); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin end = %d", resp.StatusCode)
	}
	if resp := h.get(phone, portal+"/"); resp.Request.URL.Path != "/" {
		t.Fatal("a refused request ended the session")
	}

	resp := h.post(here, portal+"/account", portal, end)
	if resp.Request.URL.Path != "/account" || !strings.Contains(body(t, resp), "is signed out, along with any app it opened.") {
		t.Errorf("end landed at %s", resp.Request.URL)
	}
	if resp := h.get(phone, portal+"/"); resp.Request.URL.Path != "/login" {
		t.Error("the ended device is still signed in")
	}
	if resp := h.get(phone, yarr+"/"); resp.Request.URL.Host != "outpost.test" {
		t.Error("the ended device kept its app session")
	}
	if resp := h.get(here, portal+"/"); resp.Request.URL.Path != "/" {
		t.Error("ending another device signed this one out")
	}
}

func TestAccountCannotEndAnotherUsersSession(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	alice, bob := h.client(true), h.client(true)
	h.login(alice, "alice", "", "/")
	h.login(bob, "bob", "", "/")
	list, err := h.store.SignIns("bob")
	if err != nil || len(list) != 1 {
		t.Fatalf("bob's sign-ins = %+v, %v", list, err)
	}
	h.post(alice, portal+"/account", portal, url.Values{"action": {"end"}, "id": {list[0].ID}})
	if resp := h.get(bob, portal+"/"); resp.Request.URL.Path != "/" {
		t.Error("one user ended another's session by naming it")
	}
}

func TestDescribeAgentNamesDevices(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1":   "Safari on iPhone",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 CriOS/130.0 Mobile/15E148 Safari/604.1":    "Chrome on iPhone",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36":     "Chrome on Mac",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15": "Safari on Mac",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0": "Edge on Windows",
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                                "Firefox on Linux",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36":     "Chrome on Android",
		"":           "Unknown browser",
		"curl/8.7.1": "curl",
	} {
		if got := DescribeAgent(ua); got != want {
			t.Errorf("DescribeAgent(%q) = %q; want %q", ua, got, want)
		}
	}
}

func TestLastUsedDoesNotClaimMoreThanItKnows(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if got := activeAgo(now, now.Add(-30*time.Hour)); got != "Active in the last two days" {
		t.Errorf("recent = %q", got)
	}
	if got := activeAgo(now, now.Add(-5*24*time.Hour)); got != "Active about 5 days ago" {
		t.Errorf("older = %q", got)
	}
}

func TestEndingASessionIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	feed := h.recording()
	here, other := h.client(true), h.client(true)
	h.login(here, "alice", "", "/")
	h.login(other, "alice", "", "/")
	feed.wait(t, 2) // the two sign-ins

	list, err := h.store.SignIns("alice")
	if err != nil || len(list) != 2 {
		t.Fatalf("sign-ins = %+v, %v", list, err)
	}
	here2 := h.get(here, portal+"/account")
	id := list[0].ID
	if !strings.Contains(body(t, here2), `name="id" value="`+id+`"`) {
		id = list[1].ID
	}
	h.post(here, portal+"/account", portal, url.Values{"action": {"end"}, "id": {id}})
	got := feed.wait(t, 3)
	if got[2].kind != EventSignOut || !strings.Contains(got[2].message, "alice signed out another device") {
		t.Errorf("event = %+v", got[2])
	}
	// Nothing was ended, so nothing happened to record.
	h.post(here, portal+"/account", portal, url.Values{"action": {"end"}, "id": {id}})
	h.get(here, portal+"/account")
	feed.wait(t, 3)
}
