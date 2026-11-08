package gateway

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/mail"
	"github.com/sroberts/outpost/internal/supervisor"
)

type mailKeys map[string]string

func (k mailKeys) Value(name string) (string, bool) { v, ok := k[name]; return v, ok }
func (k mailKeys) Put(name, value string) error     { k[name] = value; return nil }

// testedMail is the mail service with the login test answered here rather
// than by Gmail: a password of "wrong" is refused.
type testedMail struct{ *mail.Service }

func (m testedMail) TestAccount(_ context.Context, address, password string) error {
	if password == "wrong" {
		return errors.New("gmail refused the login. Gmail said: Invalid credentials")
	}
	return nil
}

// mailHarness is a portal with a real mail service, which reads no inbox:
// its poller isn't run.
func mailHarness(t *testing.T) (*harness, *mail.Service, mailKeys) {
	t.Helper()
	h := newHarness(t)
	keys := mailKeys{}
	svc, err := mail.Open(mail.Options{
		Path:           filepath.Join(t.TempDir(), "mail.db"),
		Listen:         "127.0.0.1:47902",
		IMAP:           "imap.example:993",
		SMTP:           "smtp.example:587",
		Keys:           keys,
		PasswordSecret: "gmail-app-password",
		Services:       func() []string { return []string{"sous", "tasking"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	h.g.mail = testedMail{svc}
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Status: "running"},
		supervisor.ServiceStatus{Name: "tasking", Status: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, svc, keys
}

// The account is set on the Mail page; its password is kept as a secret and
// never shown again.
func TestMailPage_SetsTheAccount(t *testing.T) {
	h, svc, keys := mailHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/mail"`) {
		t.Errorf("the catalog doesn't link an admin to Mail")
	}
	b := body(t, h.get(c, portal+"/mail"))
	for _, want := range []string{"Not set up", `id="mail-password"`, "myaccount.google.com/apppasswords"} {
		if !strings.Contains(b, want) {
			t.Errorf("the Mail page lacks %q:\n%s", want, b)
		}
	}

	resp := h.post(c, portal+"/mail", portal, url.Values{"action": {"account"}, "address": {"me+x@gmail.com"}, "password": {"abcd efgh"}})
	b = body(t, resp)
	if resp.StatusCode != 400 || !strings.Contains(b, `id="account-error"`) || !strings.Contains(b, `value="me&#43;x@gmail.com"`) {
		t.Errorf("a plus address as the account: %d\n%s", resp.StatusCode, b)
	}

	// A login Gmail refuses is said beside the form, and nothing is saved.
	resp = h.post(c, portal+"/mail", portal, url.Values{"action": {"account"}, "address": {"me@gmail.com"}, "password": {"wrong"}})
	b = body(t, resp)
	if resp.StatusCode != 400 || !strings.Contains(b, "Gmail refused the login") || svc.Status().Address != "" {
		t.Errorf("a refused login: %d\n%s", resp.StatusCode, b)
	}

	b = body(t, h.post(c, portal+"/mail", portal, url.Values{"action": {"account"}, "address": {"me@gmail.com"}, "password": {"abcd efgh ijkl mnop"}}))
	if !strings.Contains(b, "Saved, and Gmail took the login.") || !strings.Contains(b, "<code>me@gmail.com</code>") {
		t.Errorf("after saving:\n%s", b)
	}
	if keys["gmail-app-password"] != "abcdefghijklmnop" {
		t.Errorf("password kept as %q", keys["gmail-app-password"])
	}
	if strings.Contains(b, "abcdefghijklmnop") || strings.Contains(b, "abcd efgh") {
		t.Error("the page shows the password")
	}
	if !svc.Status().Ready() {
		t.Error("the account isn't ready after saving")
	}

	// Not an admin's page for anyone else, and not from another site.
	alice := h.client(true)
	h.login(alice, "alice", "", "/")
	if resp := h.get(alice, portal+"/mail"); resp.StatusCode != 403 {
		t.Errorf("a non-admin got the Mail page: %d", resp.StatusCode)
	}
	if b := body(t, h.get(alice, portal+"/")); strings.Contains(b, `href="/mail"`) {
		t.Error("a non-admin is linked to Mail")
	}
	if resp := h.post(c, portal+"/mail", "https://evil.example", url.Values{"action": {"check"}}); resp.StatusCode != 403 {
		t.Errorf("a cross-site post: %d", resp.StatusCode)
	}

	// Who may send as the account is seen in one place, senders first.
	if err := svc.SetSend("tasking", true, "root"); err != nil {
		t.Fatal(err)
	}
	b = body(t, h.get(c, portal+"/mail"))
	ti, si := strings.Index(b, `href="/#set-mail-tasking"`), strings.Index(b, `href="/#set-mail-sous"`)
	if !strings.Contains(b, "1 may send mail as you.") || ti < 0 || si < 0 || ti > si {
		t.Errorf("the apps on the Mail page:\n%s", b)
	}
}

// An app's addresses, feeds, sending and token are under its menu.
func TestAppSettings_Mail(t *testing.T) {
	h, svc, _ := mailHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	sous := settingsPanel(t, body(t, h.get(c, portal+"/")), "set-mail-sous")
	if !strings.Contains(sous, "not set up") || !strings.Contains(sous, `<a href="/mail">Set one up`) || !strings.Contains(sous, "<code>+sous</code>") {
		t.Errorf("before an account, sous's mail panel:\n%s", sous)
	}
	if err := svc.SetAccount("me@gmail.com", "pw", "root"); err != nil {
		t.Fatal(err)
	}

	b := body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-alias-add"}, "app": {"sous"}, "note": {"The Weekly"}}))
	m := regexp.MustCompile(`Made the address &#43;([a-z0-9]{4})\.`).FindStringSubmatch(b)
	if m == nil || !strings.Contains(b, `id="set-mail-sous" open`) {
		t.Fatalf("after making an address:\n%s", b)
	}
	alias := m[1]
	sous = settingsPanel(t, b, "set-mail-sous")
	for _, want := range []string{
		"<code>me&#43;sous<wbr>@gmail.com</code></p>\n<p class=\"hint\">Sous's own address</p>",
		"<code>me&#43;" + alias + "<wbr>@gmail.com</code></p>\n<p class=\"hint\">The Weekly</p>",
		"Feed<span class=\"sr\"> of mail to The Weekly</span>",
		"http://127.0.0.1:47902/feeds/" + alias + ".atom?key=",
		"2 addresses",
		"No mail yet.",
	} {
		if !strings.Contains(sous, want) {
			t.Errorf("sous's mail panel lacks %q:\n%s", want, sous)
		}
	}
	if strings.Count(sous, `value="mail-alias-remove"`) != 1 {
		t.Errorf("only the alias can be removed, not the app's name:\n%s", sous)
	}

	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-send"}, "app": {"sous"}, "send": {"1"}}))
	if !strings.Contains(b, "may send mail now") || !svc.CanSend("sous") || svc.CanSend("tasking") {
		t.Errorf("after allowing sending:\n%s", b)
	}
	if !strings.Contains(settingsPanel(t, b, "set-mail-sous"), "2 addresses, sends") {
		t.Error("the summary doesn't say it sends")
	}
	h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-send"}, "app": {"sous"}})
	if svc.CanSend("sous") {
		t.Error("sending wasn't turned off")
	}

	// One app's alias can't be removed from another's menu.
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-alias-remove"}, "app": {"tasking"}, "route": {alias}}); resp.StatusCode != 400 {
		t.Errorf("removing sous's alias from tasking: %d", resp.StatusCode)
	}
	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-alias-remove"}, "app": {"sous"}, "route": {alias}}))
	if !strings.Contains(b, "Removed &#43;"+alias+".") || len(svc.Routes("sous")) != 1 {
		t.Errorf("after removing:\n%s", b)
	}

	env, _ := svc.Resolve("sous")
	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mail-token"}, "app": {"sous"}}))
	if after, _ := svc.Resolve("sous"); !strings.Contains(b, "New token made.") || after[mail.EnvToken] == env[mail.EnvToken] {
		t.Errorf("after replacing the token:\n%s", b)
	}

	// A made-up route in the redirect can't put words on the page.
	if b := body(t, h.get(c, portal+"/?app=sous&done=mail-alias-added&route=%3Cb%3E")); strings.Contains(b, "Made the address") {
		t.Error("a notice was made from a route that can't be one")
	}
}
