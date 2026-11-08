package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// keys is an in-memory Keys.
type keys map[string]string

func (k keys) Value(name string) (string, bool) { v, ok := k[name]; return v, ok }
func (k keys) Put(name, value string) error     { k[name] = value; return nil }

// fakeBox is an inbox: messages by UID, and what was done to them.
type fakeBox struct {
	mu       sync.Mutex
	validity uint32
	msgs     map[uint32]string // raw messages
	labels   map[uint32][]string
	inbox    map[uint32]bool
	logins   []string
	bodyRead []uint32
	fileErr  error
	loginErr error
}

func newBox() *fakeBox {
	return &fakeBox{validity: 7, msgs: map[uint32]string{}, labels: map[uint32][]string{}, inbox: map[uint32]bool{}}
}

func (b *fakeBox) add(uid uint32, raw string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs[uid] = raw
	b.inbox[uid] = true
}

func (b *fakeBox) dial(_ context.Context, _, user, pass string) (session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logins = append(b.logins, user+":"+pass)
	if b.loginErr != nil {
		return nil, b.loginErr
	}
	return &fakeSession{b}, nil
}

type fakeSession struct{ b *fakeBox }

func (s *fakeSession) inbox() (uint32, error) { return s.b.validity, nil }

func (s *fakeSession) search(after uint32, _ time.Time) ([]uint32, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	var out []uint32
	for uid := range s.b.msgs {
		if s.b.inbox[uid] && uid > after {
			out = append(out, uid)
		}
	}
	return out, nil
}

func (s *fakeSession) headers(uids []uint32) (map[uint32][]byte, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	out := map[uint32][]byte{}
	for _, u := range uids {
		head, _, _ := strings.Cut(s.b.msgs[u], "\r\n\r\n")
		out[u] = []byte(head)
	}
	return out, nil
}

func (s *fakeSession) bodies(uids []uint32) (map[uint32][]byte, error) {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	out := map[uint32][]byte{}
	for _, u := range uids {
		s.b.bodyRead = append(s.b.bodyRead, u)
		out[u] = []byte(s.b.msgs[u])
	}
	return out, nil
}

func (s *fakeSession) file(uids []uint32, label string) error {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if s.b.fileErr != nil {
		return s.b.fileErr
	}
	for _, u := range uids {
		s.b.labels[u] = append(s.b.labels[u], label)
		s.b.inbox[u] = false
	}
	return nil
}

func (s *fakeSession) close() error { return nil }

// sent is what the fake sender was given.
type sent struct {
	addr, user, pass, from string
	to                     []string
	msg                    string
}

type harness struct {
	svc  *Service
	box  *fakeBox
	keys keys
	sent []sent
}

func newHarness(t *testing.T, apps ...string) *harness {
	t.Helper()
	h := &harness{box: newBox(), keys: keys{}}
	svc, err := Open(Options{
		Path:           filepath.Join(t.TempDir(), "mail.db"),
		Listen:         "127.0.0.1:47902",
		IMAP:           "imap.example:993",
		SMTP:           "smtp.example:587",
		Keys:           h.keys,
		PasswordSecret: "gmail-app-password",
		Services:       func() []string { return apps },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.dial = h.box.dial
	svc.send = func(_ context.Context, addr, user, pass, from string, to []string, msg []byte) error {
		h.sent = append(h.sent, sent{addr, user, pass, from, to, string(msg)})
		return nil
	}
	h.svc = svc
	return h
}

func (h *harness) setUp(t *testing.T) {
	t.Helper()
	if err := h.svc.SetAccount("Scott.Roberts@gmail.com", "abcd efgh ijkl mnop", "admin"); err != nil {
		t.Fatalf("SetAccount: %v", err)
	}
}

func newsletter(to, subject string) string {
	return "Delivered-To: " + to + "\r\n" +
		"From: The Weekly <hello@weekly.example>\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <abc." + subject + "@weekly.example>\r\n" +
		"Date: Sat, 07 Nov 2026 09:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello in text.\r\n" +
		"--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello in <b>HTML</b>.</p>\r\n" +
		"--b--\r\n"
}

func TestAccountRoute(t *testing.T) {
	a, err := parseAccount("Scott.Roberts@gmail.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rcpt, route string
		ok          bool
	}{
		{"scott.roberts+yarr@gmail.com", "yarr", true},
		{"ScottRoberts+Yarr@googlemail.com", "yarr", true},
		{"s.c.o.t.t.r.o.b.e.r.t.s+k7q2@GMAIL.com", "k7q2", true},
		{"scott.roberts@gmail.com", "", false},
		{"scott.roberts+@gmail.com", "", false},
		{"someone+yarr@gmail.com", "", false},
		{"scott.roberts+yarr@example.com", "", false},
	} {
		got, ok := a.route(c.rcpt)
		if got != c.route || ok != c.ok {
			t.Errorf("route(%q) = %q, %v; want %q, %v", c.rcpt, got, ok, c.route, c.ok)
		}
	}
	if _, err := parseAccount("me+x@gmail.com"); err == nil {
		t.Error("an account with a + part was accepted")
	}
}

func TestNewAlias_HasADigitAndNoLookalikes(t *testing.T) {
	for range 200 {
		a, err := newAlias()
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != aliasLen || !strings.ContainsAny(a, "23456789") || strings.ContainsAny(a, "01ilo") {
			t.Fatalf("alias %q", a)
		}
	}
}

// Mail to an app's name and to its alias is kept for it, labelled and
// archived; mail to no route is left alone, and its body never read.
func TestCheck_RoutesLabelsAndArchives(t *testing.T) {
	h := newHarness(t, "yarr", "tasking")
	h.setUp(t)
	alias, err := h.svc.AddAlias("yarr", "The Weekly", "admin")
	if err != nil {
		t.Fatal(err)
	}
	h.box.add(1, newsletter("scott.roberts+yarr@gmail.com", "one"))
	h.box.add(2, newsletter("scottroberts+"+alias.Route+"@gmail.com", "two"))
	h.box.add(3, newsletter("scott.roberts@gmail.com", "personal"))
	h.box.add(4, newsletter("scott.roberts+nobody@gmail.com", "unknown"))
	h.box.add(5, newsletter("scott.roberts+tasking@gmail.com", "task"))

	routed, _, err := h.svc.check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if routed != 3 {
		t.Errorf("routed %d; want 3", routed)
	}
	if got := h.box.logins; len(got) != 1 || got[0] != "Scott.Roberts@gmail.com:abcdefghijklmnop" {
		t.Errorf("logins = %q; want the account with the app password, spaces taken out", got)
	}
	for uid, want := range map[uint32]string{1: "Outpost/yarr", 2: "Outpost/yarr", 5: "Outpost/tasking"} {
		if got := h.box.labels[uid]; len(got) != 1 || got[0] != want || h.box.inbox[uid] {
			t.Errorf("message %d: labels %q, in inbox %v; want %s and archived", uid, got, h.box.inbox[uid], want)
		}
	}
	for _, uid := range []uint32{3, 4} {
		if len(h.box.labels[uid]) != 0 || !h.box.inbox[uid] || slices.Contains(h.box.bodyRead, uid) {
			t.Errorf("message %d to no route was touched or read", uid)
		}
	}
	msgs, err := h.svc.Messages("yarr", 0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("yarr's messages = %v, %v; want 2", msgs, err)
	}
	m := msgs[0]
	if m.Route != "yarr" || m.Subject != "one" || m.From.Name != "The Weekly" || m.From.Address != "hello@weekly.example" ||
		m.Text != "Hello in text." || m.HTML != "<p>Hello in <b>HTML</b>.</p>" || m.MessageID != "abc.one@weekly.example" ||
		m.To != "Scott.Roberts+yarr@gmail.com" {
		t.Errorf("message = %+v", m)
	}
	if msgs[1].Route != alias.Route {
		t.Errorf("second message's route = %q; want the alias %q", msgs[1].Route, alias.Route)
	}

	// The next check starts after what was seen, and routes nothing twice.
	h.box.add(6, newsletter("scott.roberts+yarr@gmail.com", "six"))
	routed, _, err = h.svc.check(context.Background())
	if err != nil || routed != 1 {
		t.Errorf("second check routed %d, %v; want 1", routed, err)
	}
	if n, _ := h.svc.Held("yarr"); n != 3 {
		t.Errorf("yarr holds %d; want 3", n)
	}
}

// A new UIDVALIDITY means the old UIDs are meaningless: reading starts again,
// and what was kept already isn't kept twice.
func TestCheck_UIDValidityChangeStartsOver(t *testing.T) {
	h := newHarness(t, "yarr")
	h.setUp(t)
	h.box.add(10, newsletter("scott.roberts+yarr@gmail.com", "a"))
	if _, _, err := h.svc.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.box.validity = 8
	h.box.add(3, newsletter("scott.roberts+yarr@gmail.com", "b"))
	if routed, _, err := h.svc.check(context.Background()); err != nil || routed != 1 {
		t.Errorf("after a new UIDVALIDITY, routed %d, %v; want the one new message", routed, err)
	}
}

// Labelling can fail after the mail is kept; it's reported, and the mail
// isn't routed a second time.
func TestCheck_LabelFailureKeepsMailOnce(t *testing.T) {
	h := newHarness(t, "yarr")
	h.setUp(t)
	h.box.fileErr = errors.New("NO label refused")
	h.box.add(1, newsletter("scott.roberts+yarr@gmail.com", "a"))
	_, _, err := h.svc.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("check = %v; want an error saying the mail was kept", err)
	}
	h.box.fileErr = nil
	if routed, _, err := h.svc.check(context.Background()); err != nil || routed != 0 {
		t.Errorf("next check routed %d, %v; want nothing new", routed, err)
	}
}

func TestCheckOnce_RecordsStatus(t *testing.T) {
	h := newHarness(t, "yarr")
	if h.svc.checkOnce(context.Background()); len(h.box.logins) != 0 {
		t.Error("checked with no account set up")
	}
	h.setUp(t)
	h.box.loginErr = loginError(errors.New("Invalid credentials"))
	h.svc.checkOnce(context.Background())
	st := h.svc.Status()
	if !st.Ready() || !strings.Contains(st.Error, "app password") || st.Checked.IsZero() {
		t.Errorf("status = %+v; want the login error", st)
	}
	h.box.loginErr = nil
	h.box.add(1, newsletter("scott.roberts+yarr@gmail.com", "a"))
	h.svc.checkOnce(context.Background())
	if st := h.svc.Status(); st.Error != "" || st.Routed != 1 || st.LastRouted.IsZero() {
		t.Errorf("status = %+v; want one routed and no error", st)
	}
}

func TestSetAccount(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.SetAccount("me@gmail.com", "", "admin"); err == nil {
		t.Error("an account with no password was accepted")
	}
	if err := h.svc.SetAccount("me+x@gmail.com", "pw", "admin"); err == nil {
		t.Error("a plus address was accepted as the account")
	}
	if err := h.svc.SetAccount("me@gmail.com", "pw", "admin"); err != nil {
		t.Fatal(err)
	}
	// The saved password stays when only the address changes.
	if err := h.svc.SetAccount("you@gmail.com", "", "admin"); err != nil {
		t.Fatal(err)
	}
	if st := h.svc.Status(); st.Address != "you@gmail.com" || !st.HasPassword || !st.FromPortal {
		t.Errorf("status = %+v", st)
	}
}

func TestRoutes_AndAliases(t *testing.T) {
	h := newHarness(t, "yarr", "Odd Name")
	if rs := h.svc.Routes("yarr"); len(rs) != 1 || rs[0].Address != "" || rs[0].Alias {
		t.Errorf("before an account, routes = %+v; want the name with no address", rs)
	}
	h.setUp(t)
	a, err := h.svc.AddAlias("yarr", "Stratechery", "admin")
	if err != nil {
		t.Fatal(err)
	}
	rs := h.svc.Routes("yarr")
	if len(rs) != 2 || rs[0].Address != "Scott.Roberts+yarr@gmail.com" || rs[1].Route != a.Route || rs[1].Note != "Stratechery" || !rs[1].Alias {
		t.Errorf("routes = %+v", rs)
	}
	if rs := h.svc.Routes("Odd Name"); len(rs) != 0 {
		t.Errorf("a name that can't be in an address got a route: %+v", rs)
	}
	if err := h.svc.RemoveAlias("tasking", a.Route, "admin"); err == nil {
		t.Error("removed another app's alias")
	}
	if err := h.svc.RemoveAlias("yarr", a.Route, "admin"); err != nil {
		t.Fatal(err)
	}
	if rs := h.svc.Routes("yarr"); len(rs) != 1 {
		t.Errorf("after removing, routes = %+v", rs)
	}
}

func TestSend(t *testing.T) {
	h := newHarness(t, "tasking")
	ctx := context.Background()
	if _, err := h.svc.Send(ctx, "tasking", Outgoing{To: []string{"a@example.com"}, Subject: "hi", Text: "x"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("unallowed send = %v; want ErrNotAllowed", err)
	}
	if err := h.svc.SetSend("tasking", true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Send(ctx, "tasking", Outgoing{To: []string{"a@example.com"}, Subject: "hi", Text: "x"}); !errors.Is(err, ErrNotReady) {
		t.Errorf("send with no account = %v; want ErrNotReady", err)
	}
	h.setUp(t)
	id, err := h.svc.Send(ctx, "tasking", Outgoing{
		To: []string{"Ann <ann@example.com>"}, Bcc: []string{"hidden@example.com"},
		Subject: "Your list\r\nBcc: evil@example.com", Text: "Plain", HTML: "<p>Rich</p>", FromName: "Tasking",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(h.sent) != 1 {
		t.Fatalf("sent %d", len(h.sent))
	}
	got := h.sent[0]
	if got.addr != "smtp.example:587" || got.from != "Scott.Roberts@gmail.com" || got.user != "Scott.Roberts@gmail.com" {
		t.Errorf("envelope = %+v", got)
	}
	if !slices.Equal(got.to, []string{"ann@example.com", "hidden@example.com"}) {
		t.Errorf("recipients = %q", got.to)
	}
	for _, want := range []string{
		`From: "Tasking" <Scott.Roberts+tasking@gmail.com>`,
		"Reply-To: Scott.Roberts+tasking@gmail.com",
		`To: "Ann" <ann@example.com>`,
		"Message-ID: <" + id + ">",
		"multipart/alternative",
	} {
		if !strings.Contains(got.msg, want) {
			t.Errorf("message lacks %q:\n%s", want, got.msg)
		}
	}
	if strings.Contains(got.msg, "hidden@example.com") || strings.Contains(got.msg, "\r\nBcc:") {
		t.Errorf("a Bcc reached the headers:\n%s", got.msg)
	}

	// Another app's route can't be sent from.
	if _, err := h.svc.Send(ctx, "tasking", Outgoing{To: []string{"a@example.com"}, Subject: "x", Text: "x", Route: "yarr"}); err == nil {
		t.Error("sent from a route that isn't the app's")
	}
}

func TestSend_ReplyThreadsAndAddresses(t *testing.T) {
	h := newHarness(t, "tasking")
	h.setUp(t)
	_ = h.svc.SetSend("tasking", true, "admin")
	h.box.add(1, newsletter("scott.roberts+tasking@gmail.com", "question"))
	if _, _, err := h.svc.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs, _ := h.svc.Messages("tasking", 0, 1)
	if _, err := h.svc.Send(context.Background(), "tasking", Outgoing{InReplyTo: msgs[0].ID, Text: "Answer"}); err != nil {
		t.Fatal(err)
	}
	got := h.sent[0]
	if !slices.Equal(got.to, []string{"hello@weekly.example"}) {
		t.Errorf("reply went to %q", got.to)
	}
	for _, want := range []string{"In-Reply-To: <abc.question@weekly.example>", "Subject: Re: question"} {
		if !strings.Contains(got.msg, want) {
			t.Errorf("reply lacks %q:\n%s", want, got.msg)
		}
	}
}

func TestSend_DailyLimit(t *testing.T) {
	h := newHarness(t, "tasking")
	h.setUp(t)
	_ = h.svc.SetSend("tasking", true, "admin")
	for range dailyLimit {
		if err := h.svc.store.recordSent("tasking", "tasking", []string{"a@example.com"}, "s", "id"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.Send(context.Background(), "tasking", Outgoing{To: []string{"a@example.com"}, Subject: "x", Text: "x"}); !errors.Is(err, ErrLimit) {
		t.Errorf("send past the limit = %v; want ErrLimit", err)
	}
}

func call(t *testing.T, h http.Handler, method, path, token, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestEndpoint(t *testing.T) {
	h := newHarness(t, "yarr", "tasking")
	h.setUp(t)
	env, _ := h.svc.Resolve("yarr")
	tok := env[EnvToken]
	if env[EnvURL] != "http://127.0.0.1:47902" || !strings.HasPrefix(tok, "omail_") {
		t.Fatalf("env = %v", env)
	}
	other, _ := h.svc.Resolve("tasking")

	if code, _ := call(t, h.svc, "GET", "/v1/me", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := call(t, h.svc, "GET", "/v1/me", "omail_wrong", ""); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	code, body := call(t, h.svc, "GET", "/v1/me", tok, "")
	if code != 200 || !strings.Contains(body, `"address":"Scott.Roberts+yarr@gmail.com"`) || !strings.Contains(body, `"send":false`) {
		t.Errorf("me = %d %s", code, body)
	}

	h.box.add(1, newsletter("scott.roberts+yarr@gmail.com", "one"))
	h.box.add(2, newsletter("scott.roberts+tasking@gmail.com", "other"))
	if _, _, err := h.svc.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, body = call(t, h.svc, "GET", "/v1/messages", tok, "")
	var list struct {
		Messages []Message
		Next     int64
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || code != 200 || len(list.Messages) != 1 || list.Messages[0].Subject != "one" {
		t.Fatalf("messages = %d %s", code, body)
	}
	id := list.Messages[0].ID
	if code, body = call(t, h.svc, "GET", "/v1/messages?after="+itoa(list.Next), tok, ""); !strings.Contains(body, `"messages":[]`) {
		t.Errorf("after the last: %d %s", code, body)
	}
	// One app can't read or delete another's.
	if code, _ = call(t, h.svc, "GET", "/v1/messages/"+itoa(id), other[EnvToken], ""); code != http.StatusNotFound {
		t.Errorf("another app read it: %d", code)
	}
	if code, _ = call(t, h.svc, "DELETE", "/v1/messages/"+itoa(id), other[EnvToken], ""); code != http.StatusNotFound {
		t.Errorf("another app deleted it: %d", code)
	}
	if code, _ = call(t, h.svc, "DELETE", "/v1/messages/"+itoa(id), tok, ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if code, _ = call(t, h.svc, "GET", "/v1/messages/"+itoa(id), tok, ""); code != http.StatusNotFound {
		t.Errorf("after delete: %d", code)
	}

	send := `{"to":["a@example.com"],"subject":"hi","text":"x"}`
	if code, _ = call(t, h.svc, "POST", "/v1/send", tok, send); code != http.StatusForbidden {
		t.Errorf("unallowed send: %d", code)
	}
	_ = h.svc.SetSend("yarr", true, "admin")
	if code, body = call(t, h.svc, "POST", "/v1/send", tok, send); code != 200 || !strings.Contains(body, "message_id") {
		t.Errorf("send: %d %s", code, body)
	}
	if code, _ = call(t, h.svc, "POST", "/v1/send", tok, `{"to":["nope"],"subject":"hi","text":"x"}`); code != http.StatusBadRequest {
		t.Errorf("bad recipient: %d", code)
	}

	// A regenerated token stops working at once.
	if err := h.svc.Regenerate("yarr", "admin"); err != nil {
		t.Fatal(err)
	}
	if code, _ = call(t, h.svc, "GET", "/v1/me", tok, ""); code != http.StatusUnauthorized {
		t.Errorf("old token after regenerate: %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFeed(t *testing.T) {
	h := newHarness(t, "yarr")
	h.setUp(t)
	a, _ := h.svc.AddAlias("yarr", "The Weekly", "admin")
	h.box.add(1, newsletter("scott.roberts+"+a.Route+"@gmail.com", "Issue 12"))
	h.box.add(2, newsletter("scott.roberts+yarr@gmail.com", "Not this feed"))
	if _, _, err := h.svc.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(a.Feed, h.svc.URL())
	code, body := call(t, h.svc, "GET", path, "", "")
	if code != 200 {
		t.Fatalf("feed: %d %s", code, body)
	}
	for _, want := range []string{"<title>The Weekly</title>", "<title>Issue 12</title>", "&lt;p&gt;Hello in &lt;b&gt;HTML&lt;/b&gt;.&lt;/p&gt;", `type="html"`} {
		if !strings.Contains(body, want) {
			t.Errorf("feed lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Not this feed") {
		t.Error("the alias's feed has mail sent to another route")
	}
	// Without the key, or with another route's, there's nothing there.
	if code, _ := call(t, h.svc, "GET", "/feeds/"+a.Route+".atom", "", ""); code != http.StatusNotFound {
		t.Errorf("no key: %d", code)
	}
	yarrKey := h.svc.store.feedSig("yarr")
	if code, _ := call(t, h.svc, "GET", "/feeds/"+a.Route+".atom?key="+yarrKey, "", ""); code != http.StatusNotFound {
		t.Errorf("another route's key: %d", code)
	}
	// An entry's link shows the message, sandboxed.
	msgs, _ := h.svc.Messages("yarr", 0, 1)
	req := httptest.NewRequest("GET", "/feeds/"+a.Route+"/"+itoa(msgs[0].ID)+"?key="+h.svc.store.feedSig(a.Route), nil)
	rec := httptest.NewRecorder()
	h.svc.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || !strings.Contains(rec.Body.String(), "<b>HTML</b>") {
		t.Errorf("message view: %d %q %s", rec.Code, rec.Header().Get("Content-Security-Policy"), rec.Body.String())
	}
	// Once the alias is gone, so is its feed.
	_ = h.svc.RemoveAlias("yarr", a.Route, "admin")
	if code, _ := call(t, h.svc, "GET", path, "", ""); code != http.StatusNotFound {
		t.Errorf("feed of a removed alias: %d", code)
	}
}

func TestStore_KeepsOnlyTheNewest(t *testing.T) {
	h := newHarness(t, "yarr")
	for i := range keep + 5 {
		m := &Message{service: "yarr", Route: "yarr", Subject: "s"}
		if _, err := h.svc.store.put("7:"+itoa(int64(i)), m); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := h.svc.Held("yarr"); n != keep {
		t.Errorf("held %d; want %d", n, keep)
	}
}

func TestRecipients_ReadsDeliveredToFirst(t *testing.T) {
	got := recipients([]byte("To: Someone <a@example.com>, b@example.com\r\nDelivered-To: me+x@gmail.com\r\nCc: =?utf-8?q?Jos=C3=A9?= <c@example.com>"))
	want := []string{"me+x@gmail.com", "a@example.com", "b@example.com", "c@example.com"}
	if !slices.Equal(got, want) {
		t.Errorf("recipients = %q; want %q", got, want)
	}
}

// Mail delivered to the account itself isn't routed by what its sender wrote
// in To or Cc.
func TestCheck_DeliveredToDecides(t *testing.T) {
	h := newHarness(t, "yarr")
	h.setUp(t)
	forged := strings.Replace(newsletter("scott.roberts+yarr@gmail.com", "forged"),
		"Delivered-To: scott.roberts+yarr@gmail.com", "Delivered-To: scott.roberts@gmail.com", 1)
	h.box.add(1, forged)
	noDelivered := strings.Replace(newsletter("scott.roberts+yarr@gmail.com", "plain"),
		"Delivered-To: scott.roberts+yarr@gmail.com\r\n", "", 1)
	h.box.add(2, noDelivered)
	if _, _, err := h.svc.check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.box.inbox[1] || slices.Contains(h.box.bodyRead, 1) {
		t.Error("mail delivered to the account was routed by its To header")
	}
	if h.box.inbox[2] {
		t.Error("mail with no Delivered-To wasn't routed by its To header")
	}
}

func TestTestAccount(t *testing.T) {
	h := newHarness(t)
	if err := h.svc.TestAccount(context.Background(), "me@gmail.com", ""); err == nil {
		t.Error("tested with no password at all")
	}
	if err := h.svc.TestAccount(context.Background(), "me@gmail.com", "ab cd"); err != nil {
		t.Fatal(err)
	}
	if got := h.box.logins; len(got) != 1 || got[0] != "me@gmail.com:abcd" {
		t.Errorf("logins = %q", got)
	}
	h.box.loginErr = loginError(errors.New("Invalid credentials"))
	if err := h.svc.TestAccount(context.Background(), "me@gmail.com", "x"); err == nil || !strings.Contains(err.Error(), "Gmail said: Invalid credentials") {
		t.Errorf("a refused login = %v", err)
	}
	if h.svc.Status().Address != "" {
		t.Error("testing saved the account")
	}
}
