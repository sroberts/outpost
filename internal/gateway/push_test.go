package gateway

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/push"
)

// withPush gives h's gateway a push service. Nothing delivers: Run isn't
// started, so what Send queues stays queued and no request leaves the test.
func withPush(t *testing.T, h *harness) *push.Service {
	t.Helper()
	svc, err := push.Open(push.Options{Path: filepath.Join(t.TempDir(), "push.db"), Listen: "127.0.0.1:1", Subject: "mailto:ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetGate(h.g.PushGate())
	h.g.push = svc
	return svc
}

// browserSub is a PushSubscription's JSON, with real keys.
func browserSub(t *testing.T, endpoint string) string {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	b, _ := json.Marshal(map[string]any{
		"endpoint":       endpoint,
		"expirationTime": nil,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(a),
		},
	})
	return string(b)
}

func (h *harness) sendJSON(c *http.Client, method, rawURL, origin, contentType, body string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const appleEndpoint = "https://web.push.apple.com/QGuQyavXutnMp6x2EAUnqMg"

func TestPushSubscription_OnlyThisSignInsOwnDevice(t *testing.T) {
	h := newHarness(t)
	svc := withPush(t, h)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	sub := browserSub(t, appleEndpoint)

	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", sub); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("signed out: %d", resp.StatusCode)
	}
	h.login(c, "alice", "", "")
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, "", "application/json", sub); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no Origin: %d", resp.StatusCode)
	}
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, "http://evil.example", "application/json", sub); resp.StatusCode != http.StatusForbidden {
		t.Errorf("another site: %d", resp.StatusCode)
	}
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "text/plain", sub); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("not JSON: %d", resp.StatusCode)
	}
	// The daemon POSTs to the endpoint, so a user mustn't be able to name
	// something on its own network.
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, "http://127.0.0.1:47901/x")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a loopback endpoint: %d", resp.StatusCode)
	}
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", sub); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", resp.StatusCode, body(t, resp))
	}
	devs, _ := svc.Devices("alice")
	if len(devs) != 1 || devs[0].Platform != "apple" {
		t.Fatalf("devices %+v", devs)
	}

	page := body(t, h.get(c, portal+"/account"))
	for _, want := range []string{`id="notify"`, `data-here="1"`, "This one", "Send a test notification", `data-push-key="` + svc.PublicKey() + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("account page lacks %q", want)
		}
	}

	if resp := h.sendJSON(c, http.MethodDelete, portal+pushSubPath, portal, "application/json", `{"endpoint":"`+appleEndpoint+`"}`); resp.StatusCode != http.StatusNoContent {
		t.Errorf("unsubscribe: %d", resp.StatusCode)
	}
	if devs, _ := svc.Devices("alice"); len(devs) != 0 {
		t.Errorf("kept %+v after turning off", devs)
	}
}

// An app's session is for that app; a token never reaches the portal.
func TestPushSubscription_AppSessionAndTokensCantSubscribe(t *testing.T) {
	h := newHarness(t)
	withPush(t, h)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "yarr", "/")
	// The app's cookie lives on the app's host; sending it to the portal
	// host by hand is what a page on the app would need to do.
	resp := h.sendJSON(h.client(false), http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, appleEndpoint))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no portal session: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, portal+pushSubPath, strings.NewReader(browserSub(t, appleEndpoint)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", portal)
	req.Header.Set("Authorization", "Bearer opat_whatever")
	r2, err := h.client(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode == http.StatusNoContent {
		t.Error("a token subscribed")
	}
}

// A device signed out of Outpost stops getting notifications, and is
// forgotten: a lost phone that's signed out mustn't keep lighting up.
func TestPushSend_SignedOutDeviceIsForgotten(t *testing.T) {
	h := newHarness(t)
	svc := withPush(t, h)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	h.login(c, "alice", "", "")
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, appleEndpoint)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("subscribe: %d", resp.StatusCode)
	}
	if n, err := svc.Send("yarr", []string{"alice"}, push.Notification{Title: "Hi"}); err != nil || n != 1 {
		t.Fatalf("signed in: %d, %v", n, err)
	}
	h.post(c, portal+"/logout", portal, nil)
	if n, _ := svc.Send("yarr", []string{"alice"}, push.Notification{Title: "Hi"}); n != 0 {
		t.Errorf("sent to %d signed-out devices", n)
	}
	if devs, _ := svc.Devices("alice"); len(devs) != 0 {
		t.Errorf("kept %+v", devs)
	}
}

func TestPushGate_OnlyThoseWhoCanOpenTheApp(t *testing.T) {
	h := newHarness(t)
	fs := h.g.services.(*fakeServices)
	fs.mu.Lock()
	fs.list[1].Allow = []string{"bob"} // wiki
	fs.mu.Unlock()
	h.g.refreshRoutes(context.Background())
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "gone"})
	if err := h.store.SetDisabled("gone", true); err != nil {
		t.Fatal(err)
	}

	gate := h.g.PushGate()
	for _, tc := range []struct {
		svc, user string
		want      bool
	}{
		{"yarr", "alice", true},
		{"wiki", "alice", false},
		{"wiki", "bob", true},
		{"wiki", "root", true},
		{"ghost", "root", false},
		{push.Outpost, "alice", true},
		{"yarr", "gone", false},
		{push.Outpost, "gone", false},
		{"yarr", "nobody", false},
	} {
		if got := gate.MayReceive(tc.svc, tc.user); got != tc.want {
			t.Errorf("MayReceive(%q, %q) = %v, want %v", tc.svc, tc.user, got, tc.want)
		}
	}
	if got := gate.Navigate("yarr", "/a b?c=1"); got != portal+pushOpenPath+"?app=yarr&path=%2Fa+b%3Fc%3D1" {
		t.Errorf("an app's link: %s", got)
	}
	if got := gate.Navigate(push.Outpost, "/account"); got != portal+"/account" {
		t.Errorf("Outpost's link: %s", got)
	}
}

func TestPushOpen_GoesToTheAppAndNowhereElse(t *testing.T) {
	h := newHarness(t)
	c := h.client(false)
	resp := h.get(c, portal+pushOpenPath+"?"+url.Values{"app": {"yarr"}, "path": {"/items/4?x=1"}}.Encode())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != yarr+"/items/4?x=1" {
		t.Errorf("open: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, q := range []url.Values{
		{"app": {"yarr"}, "path": {"//evil.example/"}},
		{"app": {"yarr"}, "path": {"https://evil.example/"}},
		{"app": {"yarr"}, "path": {"/\\evil.example"}},
		{"app": {"ghost"}, "path": {"/"}},
	} {
		if resp := h.get(c, portal+pushOpenPath+"?"+q.Encode()); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%v: %d %q", q, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// A browser fetches the manifest without cookies, so it must be public, and
// it's what makes the portal installable, the only way iOS gets push.
func TestManifest_PublicAndInstallable(t *testing.T) {
	h := newHarness(t)
	c := h.client(false)
	resp := h.get(c, portal+manifestPath)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var m struct {
		Name, ShortName, StartURL, Scope, Display string
		Icons                                     []struct{ Src, Sizes string }
	}
	raw := body(t, resp)
	if err := json.Unmarshal([]byte(strings.NewReplacer(`"short_name"`, `"ShortName"`, `"start_url"`, `"StartURL"`).Replace(raw)), &m); err != nil {
		t.Fatal(err)
	}
	if m.Display != "standalone" || m.StartURL != "/" || m.Scope != "/" || m.Name == "" || m.ShortName == "" {
		t.Errorf("manifest %s", raw)
	}
	has180 := false
	for _, ic := range m.Icons {
		if ic.Sizes == "180x180" {
			has180 = true
			if r := h.get(c, portal+ic.Src); r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "image/png" {
				t.Errorf("icon %s: %d", ic.Src, r.StatusCode)
			}
		}
	}
	if !has180 {
		t.Error("no 180x180 icon, which iOS needs")
	}
	if r := h.get(c, portal+swPath); r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") {
		t.Errorf("service worker: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if page := body(t, h.get(c, portal+"/login")); !strings.Contains(page, `rel="manifest"`) || !strings.Contains(page, `rel="apple-touch-icon"`) {
		t.Error("the sign-in page doesn't link the manifest and icon, so it can't be added to the Home Screen")
	}
}

func TestPushAccount_MuteAppsAndTest(t *testing.T) {
	h := newHarness(t)
	svc := withPush(t, h)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	h.login(c, "alice", "", "")

	resp := h.post(c, portal+"/account", portal, url.Values{"action": {"push-test"}})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "push=none") {
		t.Errorf("test with no device: %d %s", resp.StatusCode, loc)
	}
	h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, appleEndpoint))
	resp = h.post(c, portal+"/account", portal, url.Values{"action": {"push-test"}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "push=sent") {
		t.Errorf("test: %s", loc)
	}

	resp = h.post(c, portal+"/account", portal, url.Values{
		"action": {"push-apps"},
		"listed": {outpostSender, "yarr", "wiki"},
		"on":     {"yarr"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	muted, _ := svc.Muted("alice")
	if !muted[push.Outpost] || !muted["wiki"] || muted["yarr"] {
		t.Errorf("muted %v; want Outpost and wiki", muted)
	}
	if n, _ := svc.Send("wiki", []string{"alice"}, push.Notification{Title: "x"}); n != 0 {
		t.Errorf("a muted app reached %d devices", n)
	}

	devs, _ := svc.Devices("alice")
	resp = h.post(c, portal+"/account", portal, url.Values{"action": {"push-forget"}, "id": {devs[0].ID}})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "push=forgot") {
		t.Errorf("forget: %s", loc)
	}
	if devs, _ := svc.Devices("alice"); len(devs) != 0 {
		t.Errorf("kept %+v", devs)
	}
}

// Without the push service, nothing about it shows or answers.
func TestPush_AbsentWithoutTheService(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	h.login(c, "alice", "", "")
	if page := body(t, h.get(c, portal+"/account")); strings.Contains(page, `id="notify"`) || strings.Contains(page, "data-push-key") {
		t.Error("the account page offers notifications with no push service")
	}
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, appleEndpoint)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("subscription endpoint: %d", resp.StatusCode)
	}
}
