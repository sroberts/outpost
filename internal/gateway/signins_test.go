package gateway

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

const (
	firefoxMac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14.5; rv:131.0) Gecko/20100101 Firefox/131.0"
	safariIOS  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

// loginAs signs a fresh browser in, claiming userAgent.
func (h *harness) loginAs(userAgent string) *http.Client {
	h.t.Helper()
	c := h.client(true)
	c.Transport = uaTransport{ua: userAgent, next: c.Transport}
	resp := h.login(c, "alice", "", "/")
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("login: %d", resp.StatusCode)
	}
	return c
}

type uaTransport struct {
	ua   string
	next http.RoundTripper
}

func (t uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.ua)
	return t.next.RoundTrip(r)
}

var endButton = regexp.MustCompile(`name="id" value="([0-9a-f]+)">\s*<button class="ghost small" type="submit" aria-label="([^"]*)">`)

func TestAccount_ListsAndEndsSignIns(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	phone := h.loginAs(safariIOS)
	backendSaw(t, h.get(phone, yarr+"/"))
	laptop := h.loginAs(firefoxMac)

	page := html.UnescapeString(body(t, h.get(laptop, portal+"/account")))
	if !strings.Contains(page, "Firefox on Mac</span>") || !strings.Contains(page, "You're using this one") {
		t.Errorf("this device isn't marked:\n%s", page)
	}
	if strings.Index(page, "Firefox on Mac") > strings.Index(page, "Safari on iPhone") {
		t.Error("this device isn't listed first")
	}
	if !strings.Contains(page, "Apps: yarr") || !strings.Contains(page, `from <span class="addr">127.0.0.1</span>`) {
		t.Errorf("the phone's sign-in is missing its apps or address:\n%s", page)
	}
	// Only the other device has a button; this one is signed out from the
	// rail, or with every other device kept.
	ids := endButton.FindAllStringSubmatch(page, -1)
	if len(ids) != 1 || !strings.HasPrefix(ids[0][2], "Sign out Safari on iPhone, signed in today at ") {
		t.Fatalf("end buttons = %q", ids)
	}
	if !strings.Contains(page, "Sign out every other device") {
		t.Error("no way to sign out the others")
	}

	// Ending it from another site does nothing.
	end := url.Values{"action": {"end"}, "id": {ids[0][1]}}
	if resp := h.post(laptop, portal+"/account", "http://evil.test", end); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin end: %d", resp.StatusCode)
	}
	resp := h.post(laptop, portal+"/account", portal, end)
	got := html.UnescapeString(body(t, resp))
	if !strings.Contains(got, "Safari on iPhone, signed in today at ") || !strings.Contains(got, "is signed out, along with any app it opened.") {
		t.Fatalf("after ending the phone:\n%s", got)
	}
	if strings.Contains(got, "Sign out every other device") {
		t.Error("offered to sign out others with none left")
	}
	// The phone is out of the portal and the app it opened.
	phone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if r := h.get(phone, yarr+"/"); r.StatusCode != http.StatusFound {
		t.Errorf("the phone still reaches yarr: %d", r.StatusCode)
	}

	// Ending this one by its ID is signing out.
	this := auth.HashToken(cookieValue(t, laptop))
	resp = h.post(laptop, portal+"/account", portal, url.Values{"action": {"end"}, "id": {this}})
	if resp.Request.URL.Path != "/login" {
		t.Errorf("ending this device landed on %s", resp.Request.URL)
	}
}

// Signing out every other device keeps this one.
func TestAccount_SignOutOthers(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	phone := h.loginAs(safariIOS)
	backendSaw(t, h.get(phone, yarr+"/"))
	h.loginAs(firefoxMac)
	laptop := h.loginAs(firefoxMac)

	resp := h.post(laptop, portal+"/account", portal, url.Values{"action": {"others"}})
	if got := body(t, resp); !strings.Contains(got, "2 other devices are signed out. You&#39;re still signed in here.") {
		t.Fatalf("after signing out others:\n%s", got)
	}
	phone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if r := h.get(phone, yarr+"/"); r.StatusCode != http.StatusFound {
		t.Errorf("the phone still reaches yarr: %d", r.StatusCode)
	}
	if r := h.get(laptop, portal+"/account"); r.Request.URL.Path != "/account" {
		t.Errorf("this device was signed out too: %s", r.Request.URL)
	}
}

// cookieValue is the gateway cookie a browser holds for the portal.
func cookieValue(t *testing.T, c *http.Client) string {
	t.Helper()
	u, _ := url.Parse(portal)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "outpost" {
			return ck.Value
		}
	}
	t.Fatal("no gateway cookie")
	return ""
}

// Someone else's sign-in ID does nothing, and says nothing about it.
func TestAccount_CantEndAnotherUsersSignIn(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	bobTok, err := h.store.CreateSession("bob", "", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	alice := h.loginAs(firefoxMac)
	resp := h.post(alice, portal+"/account", portal, url.Values{"action": {"end"}, "id": {auth.HashToken(bobTok)}})
	if !strings.Contains(html.UnescapeString(body(t, resp)), "That device was already signed out.") {
		t.Errorf("landed on %s", resp.Request.URL)
	}
	if _, _, err := h.store.LookupSession(bobTok); err != nil {
		t.Errorf("alice ended bob's sign-in: %v", err)
	}
}

func TestDescribeAgent(t *testing.T) {
	cases := map[string]string{
		firefoxMac: "Firefox on Mac",
		safariIOS:  "Safari on iPhone",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0": "Edge on Windows",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0 Mobile Safari/537.36":                      "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/129.0 Mobile/15E148":     "Chrome on iPhone",
		"curl/8.7.1": "curl",
		"":           "Unknown browser",
		"something":  "Unknown browser",
	}
	for ua, want := range cases {
		if got := DescribeAgent(ua); got != want {
			t.Errorf("DescribeAgent(%q) = %q; want %q", ua, got, want)
		}
	}
}

func TestActiveAgo(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		time.Hour:      "Active in the last day",
		30 * time.Hour: "Active in the last two days",
		75 * time.Hour: "Active about 3 days ago",
	} {
		if got := activeAgo(now, now.Add(-d)); got != want {
			t.Errorf("%s ago: %q; want %q", d, got, want)
		}
	}
}

func TestSignedInAt(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	for at, want := range map[time.Time]string{
		time.Date(2026, 10, 30, 9, 5, 0, 0, time.Local):   "today at 09:05",
		time.Date(2026, 10, 29, 23, 59, 0, 0, time.Local): "yesterday at 23:59",
		time.Date(2026, 10, 2, 7, 0, 0, 0, time.Local):    "2 Oct at 07:00",
		time.Date(2025, 12, 31, 7, 0, 0, 0, time.Local):   "31 Dec 2025",
	} {
		if got := signedInAt(now, at); got != want {
			t.Errorf("signedInAt(%s) = %q; want %q", at, got, want)
		}
	}
}
