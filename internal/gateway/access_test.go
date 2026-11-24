package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/auth"
)

// accessEntries reads back what the gateway wrote to its access log.
func accessEntries(t *testing.T, log *lockedBuffer) []applog.AccessEntry {
	t.Helper()
	var out []applog.AccessEntry
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if line == "" {
			continue
		}
		var e applog.AccessEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not one JSON object per line: %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// waitLogged waits for each response's entry: the gateway writes one once
// its handler returns, which can be after the client has the response.
func waitLogged(t *testing.T, log *lockedBuffer, resps ...*http.Response) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, resp := range resps {
		id := resp.Header.Get(RequestIDHeader)
		if id == "" {
			t.Fatalf("a response without an id: %s", resp.Request.URL)
		}
		for !strings.Contains(log.String(), `"request_id":"`+id+`"`) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached the access log:\n%s", id, log.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// Every request is in the access log, whatever the daemon's level: who it
// went through as, the token's id, the status and its id, and none of the
// credentials it carried: not the password it signed in with, its session
// cookies, a good or a bad access token, an Authorization header, nor a
// secret in its query.
func TestAccessLog_RecordsRequestsAndNoSecrets(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.access = &logs
	h.addUser(auth.User{Username: "alice"})

	c := h.client(true)
	var resps []*http.Response
	resp := h.login(c, "alice", "yarr", "/feeds?code=planted-query-secret")
	backendSaw(t, resp)
	good, tok, err := h.store.CreateAccessToken("alice", "cron", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	resps = append(resps, resp, h.send(http.MethodGet, yarr+"/api?key=planted-api-key", bearer(good)))
	backendSaw(t, resps[1])
	bad := auth.AccessTokenPrefix + "wrong-secret-value"
	if resp := h.send(http.MethodGet, yarr+"/", bearer(bad)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad token got %d", resp.StatusCode)
	} else {
		resps = append(resps, resp)
	}
	if resp := h.send(http.MethodGet, yarr+"/", http.Header{"Outpost-Token": {"planted-header-token"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad Outpost-Token got %d", resp.StatusCode)
	} else {
		resps = append(resps, resp)
	}
	if resp := h.send(http.MethodGet, yarr+"/", http.Header{"Authorization": {"Basic cGxhbnRlZDpiYXNpYw=="}}); resp.StatusCode != http.StatusFound {
		t.Errorf("no Outpost credential got %d; want the login redirect", resp.StatusCode)
	} else {
		resps = append(resps, resp)
	}
	waitLogged(t, &logs, resps...)

	out := logs.String()
	entries := accessEntries(t, &logs)
	var sawToken, sawRefused, sawPortal, sawApp bool
	for _, e := range entries {
		if e.RequestID == "" || e.Time.IsZero() || e.Method == "" || e.Host == "" || e.Status == 0 || e.IP == "" {
			t.Errorf("an entry lacks its basics: %+v", e)
		}
		if strings.Contains(e.Path, "?") {
			t.Errorf("a path kept its query: %q", e.Path)
		}
		if e.Token == strconv.FormatInt(tok.ID, 10) && e.User == "alice" && e.App == "yarr" && e.Path == "/api" && e.Status == 200 && e.Bytes > 0 {
			sawToken = true
		}
		if e.Refused == "access token rejected" && e.Status == http.StatusUnauthorized && e.User == "" {
			sawRefused = true
		}
		if e.Host == "outpost.test" && e.Path == "/login" && e.App == "" {
			sawPortal = true
		}
		if e.Host == "yarr.outpost.test" && e.Path == "/feeds" && e.User == "alice" && e.Token == "" {
			sawApp = true
		}
	}
	if !sawToken || !sawRefused || !sawPortal || !sawApp {
		t.Errorf("token %v, refused %v, portal %v, app %v in:\n%s", sawToken, sawRefused, sawPortal, sawApp, out)
	}

	secrets := []string{pw, good, "wrong-secret", "planted-query-secret", "planted-api-key", "planted-header-token", "cGxhbnRlZDpiYXNpYw", "opat_", "Bearer", "Basic", "Cookie"}
	for _, u := range []string{portal, yarr} {
		pu, _ := url.Parse(u)
		for _, ck := range c.Jar.Cookies(pu) {
			secrets = append(secrets, ck.Value)
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("%q reached the access log:\n%s", s, out)
		}
	}
}

// The gateway gives every request an id: on the response, in the access log,
// and on the copy the app gets, where a client can neither choose it nor
// take it away by naming it in Connection.
func TestRequestID_ReachesTheAppAndCantBeChosen(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.access = &logs
	h.addUser(auth.User{Username: "alice"})
	good := h.pat("alice")

	hdr := bearer(good)
	hdr.Set("X-Outpost-Request-Id", "chosen-by-client")
	hdr.Set("X_Outpost_Request_Id", "chosen-by-client")
	hdr.Set("Connection", "X-Outpost-Request-Id")
	resp := h.send(http.MethodGet, yarr+"/", hdr)
	s := backendSaw(t, resp)
	id := resp.Header.Get(RequestIDHeader)
	if id == "" || id == "chosen-by-client" {
		t.Fatalf("response id = %q", id)
	}
	if got := s.Header.Values(RequestIDHeader); len(got) != 1 || got[0] != id {
		t.Errorf("the app saw ids %q; want just %q", got, id)
	}
	waitLogged(t, &logs, resp)
	for k := range s.Header {
		if isIdentityName(k) && k != RequestIDHeader && !strings.HasPrefix(k, "X-Outpost-") {
			t.Errorf("the app was sent %s", k)
		}
	}
	if !strings.Contains(logs.String(), `"request_id":"`+id+`"`) {
		t.Errorf("the access log lacks %s:\n%s", id, logs.String())
	}

	// Each request gets its own.
	again := h.send(http.MethodGet, yarr+"/", bearer(good))
	if other := again.Header.Get(RequestIDHeader); other == "" || other == id {
		t.Errorf("a second request's id = %q, after %q", other, id)
	}
}

// A request from the internet is in the access log too, marked public, and
// its app gets no X-Outpost-* header at all, the id included.
func TestAccessLog_FunnelRequestsArePublic(t *testing.T) {
	h, pub := publicHarness(t)
	var logs lockedBuffer
	h.g.access = &logs
	resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github?sig=planted", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	waitLogged(t, &logs, resp)
	entries := accessEntries(t, &logs)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if !e.Public || e.App != "tasking" || e.IP != "203.0.113.7" || e.Path != "/webhooks/github" || e.Status != 200 {
		t.Errorf("entry = %+v", e)
	}
	if strings.Contains(logs.String(), "planted") {
		t.Errorf("the query reached the log:\n%s", logs.String())
	}
}
