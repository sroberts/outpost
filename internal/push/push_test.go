package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gate is a Gate for tests: who may receive, and which sign-ins are live.
type gate struct {
	mu      sync.Mutex
	deny    map[string]bool // "service/user"
	signIns map[string]bool
}

func (g *gate) MayReceive(service, user string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.deny[service+"/"+user]
}

func (g *gate) SignIns(string) (map[string]bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]bool{}
	for k, v := range g.signIns {
		out[k] = v
	}
	return out, nil
}

func (g *gate) Navigate(service, path string) string {
	return "https://portal.test/.outpost/push/open?app=" + url.QueryEscape(service) + "&path=" + url.QueryEscape(path)
}

// device is a browser's side of a subscription: its keys, to read what the
// push service is handed.
type device struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newDevice(t *testing.T) device {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return device{priv: k, auth: a}
}

func (d device) keys() (p256dh, auth string) {
	return base64.RawURLEncoding.EncodeToString(d.priv.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(d.auth)
}

// open is RFC 8291 from the receiving end.
func (d device) open(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < headerLen {
		t.Fatalf("body is %d bytes", len(body))
	}
	salt, keyID := body[:16], body[21:headerLen]
	as, err := ecdh.P256().NewPublicKey(keyID)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := d.priv.ECDH(as)
	if err != nil {
		t.Fatal(err)
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, d.auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(d.priv.PublicKey().Bytes())+string(keyID), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[headerLen:], nil)
	if err != nil {
		t.Fatalf("the device can't open what it was sent: %v", err)
	}
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		t.Fatal("no last-record delimiter")
	}
	return plain[:len(plain)-1]
}

// pushServer is a push service: it records what it's handed and answers
// with the next status in line, then 201.
type pushServer struct {
	*httptest.Server
	mu       sync.Mutex
	got      []*http.Request
	bodies   [][]byte
	statuses []int
	arrived  chan struct{}
}

func newPushServer(t *testing.T, statuses ...int) *pushServer {
	p := &pushServer{statuses: statuses, arrived: make(chan struct{}, 64)}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.got = append(p.got, r)
		p.bodies = append(p.bodies, b)
		code := http.StatusCreated
		if len(p.statuses) > 0 {
			code, p.statuses = p.statuses[0], p.statuses[1:]
		}
		p.mu.Unlock()
		w.WriteHeader(code)
		p.arrived <- struct{}{}
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pushServer) wait(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-p.arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("the push service wasn't asked in time")
		}
	}
}

// testService is a Service whose push services are p, with deliveries
// running until the test ends.
func testService(t *testing.T, p *pushServer) (*Service, *gate) {
	t.Helper()
	s, err := Open(Options{Path: filepath.Join(t.TempDir(), "push.db"), Listen: "127.0.0.1:1", Subject: "mailto:ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	g := &gate{deny: map[string]bool{}, signIns: map[string]bool{"si-1": true}}
	s.SetGate(g)
	s.retryBase = time.Millisecond
	if p != nil {
		host := p.Listener.Addr().String()
		s.client = p.Client()
		s.allow = func(u *url.URL) (string, bool) { return "apple", u.Host == host }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return s, g
}

func subscribe(t *testing.T, s *Service, p *pushServer, user, signIn, path string) device {
	t.Helper()
	d := newDevice(t)
	k, a := d.keys()
	if err := s.Subscribe(user, signIn, p.URL+path, k, a, "Mozilla/5.0 (iPhone)"); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPushService_OnlyPushServicesAreReached(t *testing.T) {
	for _, tc := range []struct {
		url      string
		platform string
	}{
		{"https://web.push.apple.com/QGuQ", "apple"},
		{"https://fcm.googleapis.com/fcm/send/abc", "fcm"},
		{"https://updates.push.services.mozilla.com/wpush/v2/abc", "mozilla"},
		{"https://wns2-par02p.notify.windows.com/w/?token=abc", "windows"},
		{"http://web.push.apple.com/QGuQ", ""},
		{"https://web.push.apple.com:8443/QGuQ", ""},
		{"https://127.0.0.1/x", ""},
		{"https://push.apple.com.example.net/x", ""},
		{"https://user@web.push.apple.com/x", ""},
		{"https://evilpush.apple.com/x", ""},
	} {
		u, _ := url.Parse(tc.url)
		got, ok := pushService(u)
		if got != tc.platform || ok != (tc.platform != "") {
			t.Errorf("%s: got %q, %v; want %q", tc.url, got, ok, tc.platform)
		}
	}
}

// The endpoint arrives from a browser, so a signed-in user could name
// anything; the daemon must not be made to POST to its own network.
func TestSubscribe_RefusesAnEndpointThatIsntAPushService(t *testing.T) {
	s, _ := testService(t, nil)
	d := newDevice(t)
	k, a := d.keys()
	for _, ep := range []string{"http://127.0.0.1:47901/openai/v1/chat", "https://example.com/push", "not a url"} {
		err := s.Subscribe("ann", "si-1", ep, k, a, "")
		if !IsBadRequest(err) {
			t.Errorf("%s: err %v, want a refusal", ep, err)
		}
	}
	if err := s.Subscribe("ann", "si-1", "https://web.push.apple.com/x", "short", a, ""); !IsBadRequest(err) {
		t.Errorf("a bad p256dh: err %v", err)
	}
	if devs, _ := s.Devices("ann"); len(devs) != 0 {
		t.Errorf("kept %d", len(devs))
	}
}

func TestSend_DeliversADeclarativeNotificationTheDeviceCanRead(t *testing.T) {
	p := newPushServer(t)
	s, _ := testService(t, p)
	d := subscribe(t, s, p, "ann", "si-1", "/sub/1")

	n, err := s.Send("tasking", []string{"ann"}, Notification{Title: "Due today", Body: "Water the plants", Path: "/tasks/4", Urgency: "high", TTL: 600, Topic: "due-4"})
	if err != nil || n != 1 {
		t.Fatalf("Send: %d, %v", n, err)
	}
	p.wait(t, 1)

	r := p.got[0]
	for k, want := range map[string]string{"Content-Encoding": "aes128gcm", "TTL": "600", "Urgency": "high", "Topic": "due-4"} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") || !strings.HasSuffix(r.Header.Get("Authorization"), ", k="+s.PublicKey()) {
		t.Errorf("Authorization %q", r.Header.Get("Authorization"))
	}

	var msg struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title, Body, Navigate string
		} `json:"notification"`
		Mutable bool `json:"mutable"`
	}
	if err := json.Unmarshal(d.open(t, p.bodies[0]), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.WebPush != 8030 || msg.Notification.Title != "Due today" || msg.Notification.Body != "Water the plants" || msg.Mutable {
		t.Errorf("message %+v", msg)
	}
	if want := "https://portal.test/.outpost/push/open?app=tasking&path=%2Ftasks%2F4"; msg.Notification.Navigate != want {
		t.Errorf("navigate %q, want %q", msg.Notification.Navigate, want)
	}
	waitFor(t, func() bool { return s.Status().Delivered == 1 })
	devs, _ := s.Devices("ann")
	if len(devs) != 1 || devs[0].LastSuccess.IsZero() {
		t.Errorf("devices %+v: the success wasn't recorded", devs)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSend_NobodyWhoCantOpenItHasMutedItOrIsSignedOut(t *testing.T) {
	p := newPushServer(t)
	s, g := testService(t, p)
	subscribe(t, s, p, "ann", "si-1", "/ann")
	subscribe(t, s, p, "bob", "si-1", "/bob")
	subscribe(t, s, p, "cat", "si-gone", "/cat")
	g.deny["tasking/bob"] = true

	// cat's device was signed out, so it's forgotten rather than sent to.
	if n, err := s.Send("tasking", nil, Notification{Title: "Hi"}); err != nil || n != 1 {
		t.Fatalf("everyone: %d, %v; want only ann's device", n, err)
	}
	if devs, _ := s.Devices("cat"); len(devs) != 0 {
		t.Errorf("a signed-out device was kept: %+v", devs)
	}

	if err := s.SetMuted("ann", "tasking", true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Send("tasking", []string{"ann", "bob"}, Notification{Title: "Hi"}); n != 0 {
		t.Errorf("sent to %d devices past a mute and a refusal", n)
	}
	// Muting one app leaves the others.
	if n, _ := s.Send("yarr", []string{"ann"}, Notification{Title: "Hi"}); n != 1 {
		t.Errorf("yarr to ann: %d devices", n)
	}
}

func TestDeliver_GoneSubscriptionIsForgotten(t *testing.T) {
	p := newPushServer(t, http.StatusGone)
	s, _ := testService(t, p)
	subscribe(t, s, p, "ann", "si-1", "/x")
	if _, err := s.Send(Outpost, []string{"ann"}, Notification{Title: "Hi"}); err != nil {
		t.Fatal(err)
	}
	p.wait(t, 1)
	waitFor(t, func() bool { return s.Status().Gone == 1 })
	if devs, _ := s.Devices("ann"); len(devs) != 0 {
		t.Errorf("kept %+v after 410", devs)
	}
}

func TestDeliver_ServerErrorsAreRetriedThenGivenUp(t *testing.T) {
	p := newPushServer(t, 503, 500, 502, 502)
	s, _ := testService(t, p)
	subscribe(t, s, p, "ann", "si-1", "/x")
	if _, err := s.Send(Outpost, []string{"ann"}, Notification{Title: "Hi"}); err != nil {
		t.Fatal(err)
	}
	p.wait(t, maxAttempts)
	waitFor(t, func() bool { return s.Status().Failed == 1 })
	select {
	case <-p.arrived:
		t.Errorf("asked a %dth time", maxAttempts+1)
	case <-time.After(50 * time.Millisecond):
	}
	devs, _ := s.Devices("ann")
	if len(devs) != 1 || !devs[0].Failing {
		t.Errorf("devices %+v: want one, marked failing", devs)
	}
}

func TestDeliver_RetriedServerErrorThatRecovers(t *testing.T) {
	p := newPushServer(t, 429)
	s, _ := testService(t, p)
	subscribe(t, s, p, "ann", "si-1", "/x")
	if _, err := s.Send(Outpost, []string{"ann"}, Notification{Title: "Hi"}); err != nil {
		t.Fatal(err)
	}
	p.wait(t, 2)
	waitFor(t, func() bool { return s.Status().Delivered == 1 })
}

func TestNotification_Checks(t *testing.T) {
	for _, n := range []Notification{
		{},
		{Title: "x", Path: "https://evil.example/"},
		{Title: "x", Path: "//evil.example/"},
		{Title: "x", Path: "/\\evil.example"},
		{Title: "x", Urgency: "now"},
		{Title: "x", Topic: "has space"},
		{Title: "x", TTL: -1},
		{Title: strings.Repeat("x", maxTitle+1)},
	} {
		if err := n.check(); !IsBadRequest(err) {
			t.Errorf("%+v: err %v, want a refusal", n, err)
		}
	}
	n := Notification{Title: " Hi "}
	if err := n.check(); err != nil {
		t.Fatal(err)
	}
	if n.Title != "Hi" || n.Path != "/" || n.TTL != DefaultTTL || n.Urgency != "normal" {
		t.Errorf("defaults %+v", n)
	}
}

func TestAPI_SendTakesTheAppsTokenAndNoOther(t *testing.T) {
	p := newPushServer(t)
	s, _ := testService(t, p)
	subscribe(t, s, p, "ann", "si-1", "/x")
	env, _ := s.Resolve("tasking")
	if env[EnvURL] != "http://127.0.0.1:1" || !strings.HasPrefix(env[EnvToken], tokenPrefix) {
		t.Fatalf("env %v", env)
	}
	again, _ := s.Resolve("tasking")
	if again[EnvToken] != env[EnvToken] {
		t.Error("the token changed between starts")
	}

	call := func(tok, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/send", bytes.NewBufferString(body))
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := call("", `{"title":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := call("opush_forged", `{"title":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("forged token: %d", w.Code)
	}
	if w := call(env[EnvToken], `{"title":"x","navigate":"https://evil.example"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", w.Code)
	}
	if w := call(env[EnvToken], `{"title":"x","path":"//evil.example"}`); w.Code != http.StatusBadRequest {
		t.Errorf("another host's path: %d", w.Code)
	}
	w := call(env[EnvToken], `{"to":["ann"],"title":"Due","body":"Now","path":"/t/1"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"devices":1`) {
		t.Errorf("send: %d %s", w.Code, w.Body)
	}
	p.wait(t, 1)
}

func TestSend_AnAppIsLimited(t *testing.T) {
	s, _ := testService(t, nil)
	for i := range sendLimit {
		if _, err := s.Send("tasking", []string{"nobody"}, Notification{Title: "x"}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := s.Send("tasking", []string{"nobody"}, Notification{Title: "x"}); err != ErrLimit {
		t.Errorf("past the limit: %v", err)
	}
	if _, err := s.Send(Outpost, []string{"nobody"}, Notification{Title: "x"}); err != nil {
		t.Errorf("Outpost's own were limited: %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := s.Send("tasking", []string{"nobody"}, Notification{Title: "x"}); err != nil {
		t.Errorf("an hour on: %v", err)
	}
}

func TestSubscribe_SameEndpointMovesToItsNewOwner(t *testing.T) {
	p := newPushServer(t)
	s, _ := testService(t, p)
	d := newDevice(t)
	k, a := d.keys()
	ep := p.URL + "/shared"
	if err := s.Subscribe("ann", "si-1", ep, k, a, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe("bob", "si-1", ep, k, a, ""); err != nil {
		t.Fatal(err)
	}
	ann, _ := s.Devices("ann")
	bob, _ := s.Devices("bob")
	if len(ann) != 0 || len(bob) != 1 {
		t.Errorf("ann %d, bob %d; the endpoint should be bob's alone", len(ann), len(bob))
	}
}

func TestOpen_KeepsTheVAPIDKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.db")
	open := func() *Service {
		s, err := Open(Options{Path: path, Listen: "127.0.0.1:1", Subject: "https://outpost.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a := open()
	key := a.PublicKey()
	_ = a.Close()
	b := open()
	defer func() { _ = b.Close() }()
	if b.PublicKey() != key {
		t.Error("a new VAPID key on reopening would strand every subscription")
	}
	if _, err := Open(Options{Path: path, Listen: "127.0.0.1:1", Subject: "ops@example.com"}); err == nil {
		t.Error("a contact that isn't mailto: or https: was taken")
	}
}
