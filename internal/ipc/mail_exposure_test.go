package ipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/mail"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

type fakeMail struct {
	mu       sync.Mutex
	log      []string
	status   mail.Status
	testFail error
	send     map[string]bool
}

func (f *fakeMail) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func (f *fakeMail) Status() mail.Status    { return f.status }
func (f *fakeMail) PasswordSecret() string { return "outpost-mail-password" }
func (f *fakeMail) SetAccount(address, password, by string) error {
	f.note("account " + address + " " + password + " by " + by)
	f.status.Address, f.status.HasPassword = address, true
	return nil
}
func (f *fakeMail) TestAccount(_ context.Context, address, password string) error { return f.testFail }
func (f *fakeMail) CheckWait(context.Context) error                               { f.note("check"); return nil }
func (f *fakeMail) Routes(service string) []mail.Route {
	return []mail.Route{{Route: service, Address: "me+" + service + "@gmail.com"}}
}
func (f *fakeMail) AddAlias(service, note, by string) (mail.Route, error) {
	f.note("alias " + service + " " + note + " by " + by)
	return mail.Route{Route: "k3x9", Alias: true, Note: note, Address: "me+k3x9@gmail.com"}, nil
}
func (f *fakeMail) RemoveAlias(service, route, by string) error {
	f.note("unalias " + service + " " + route)
	return nil
}
func (f *fakeMail) CanSend(service string) bool { return f.send[service] }
func (f *fakeMail) SetSend(service string, allowed bool, by string) error {
	f.send[service] = allowed
	return nil
}
func (f *fakeMail) Held() (map[string]mail.Held, error) {
	return map[string]mail.Held{"sous": {Count: 2, Newest: time.Now()}}, nil
}
func (f *fakeMail) Regenerate(service, by string) error { f.note("regenerate " + service); return nil }

type fakeExposure struct {
	mu   sync.Mutex
	log  []string
	fail error
}

func (f *fakeExposure) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}
func (f *fakeExposure) Set(_ context.Context, service string, funnel bool, paths []string, by string) error {
	if f.fail != nil {
		return f.fail
	}
	f.note("set " + service + " " + strings.Join(paths, ",") + map[bool]string{true: " funnel", false: ""}[funnel] + " by " + by)
	return nil
}
func (f *fakeExposure) Reset(_ context.Context, service, by string) error {
	f.note("reset " + service)
	return nil
}
func (f *fakeExposure) SetMCPFunnel(_ context.Context, service string, on bool, by string) error {
	f.note("mcp " + service + map[bool]string{true: " on", false: " off"}[on])
	return nil
}
func (f *fakeExposure) ResetMCPFunnel(_ context.Context, service, by string) error {
	f.note("mcp-reset " + service)
	return nil
}

// extrasDaemon is a daemon declaring sous, with mail and exposure as given.
func extrasDaemon(t *testing.T, m gateway.Mail, e gateway.Exposure) *Client {
	t.Helper()
	home := t.TempDir()
	svcDir := filepath.Join(home, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name = \"sous\"\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nport = 19310\nstorage = " +
		`"` + filepath.Join(home, "data", "sous") + `"` + "\n"
	if err := os.WriteFile(filepath.Join(svcDir, "sous.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir: svcDir, Store: st,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sock, sup, (&logBuffer{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		srv.UseMail(m)
	}
	if e != nil {
		srv.UseExposure(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sock)
}

// activity is the gateway feed's messages, newest first.
func activity(t *testing.T, c *Client) string {
	t.Helper()
	events, err := c.Events(gateway.ActivityService, 20)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Message)
	}
	return strings.Join(out, "\n")
}

func TestMail_Commands(t *testing.T) {
	f := &fakeMail{send: map[string]bool{}}
	c := extrasDaemon(t, f, nil)

	o, err := c.MailStatus()
	if err != nil || o.Account.Ready() || o.Secret != "outpost-mail-password" {
		t.Fatalf("MailStatus = %+v, %v", o, err)
	}
	if len(o.Apps) != 1 || o.Apps[0].Name != "sous" || o.Apps[0].Held != 2 || o.Apps[0].Routes[0].Route != "sous" {
		t.Errorf("apps = %+v", o.Apps)
	}

	if _, err := c.MailAccount("me@gmail.com", "app pass"); err != nil {
		t.Fatal(err)
	}
	if o, _ := c.MailStatus(); o.Account.Address != "me@gmail.com" {
		t.Errorf("account not set: %+v", o.Account)
	}
	msg, err := c.MailAliasAdd("sous", "receipts")
	if err != nil || !strings.Contains(msg, "me+k3x9@gmail.com") {
		t.Errorf("MailAliasAdd = %q, %v", msg, err)
	}
	if _, err := c.MailAliasRemove("sous", "+k3x9"); err != nil {
		t.Error(err)
	}
	if _, err := c.MailSend("sous", true); err != nil || !f.send["sous"] {
		t.Errorf("MailSend: %v, send=%v", err, f.send)
	}
	if _, err := c.MailRegenerate("sous"); err != nil {
		t.Error(err)
	}
	if msg, err := c.MailCheck(); err != nil || msg != "checked the inbox" {
		t.Errorf("MailCheck = %q, %v", msg, err)
	}

	want := []string{"account me@gmail.com app pass by the CLI", "alias sous receipts by the CLI", "unalias sous k3x9", "regenerate sous", "check"}
	if got := strings.Join(f.log, "|"); got != strings.Join(want, "|") {
		t.Errorf("calls = %q", got)
	}
	feed := activity(t, c)
	for _, w := range []string{"set the mail account from the CLI", "gave sous the mail address +k3x9", "let sous send mail", "regenerated sous's mail token"} {
		if !strings.Contains(feed, w) {
			t.Errorf("activity lacks %q:\n%s", w, feed)
		}
	}
}

// A login Gmail refuses is said straight away and not saved.
func TestMail_AccountIsTriedBeforeItsSaved(t *testing.T) {
	f := &fakeMail{send: map[string]bool{}, testFail: errors.New("Gmail refused the login")}
	c := extrasDaemon(t, f, nil)
	if _, err := c.MailAccount("me@gmail.com", "wrong"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v", err)
	}
	if len(f.log) != 0 {
		t.Errorf("saved anyway: %v", f.log)
	}
}

func TestMail_UnknownServiceAndNoMail(t *testing.T) {
	c := extrasDaemon(t, &fakeMail{send: map[string]bool{}}, nil)
	if _, err := c.MailAliasAdd("nope", ""); err == nil {
		t.Error("an address for a service that isn't declared")
	}
	bare := extrasDaemon(t, nil, nil)
	if _, err := bare.MailStatus(); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("no mail: %v", err)
	}
}

func TestExposure_Commands(t *testing.T) {
	f := &fakeExposure{}
	c := extrasDaemon(t, nil, f)
	msg, err := c.ExposureSet("sous", true, []string{"/public/*", "/feed"})
	if err != nil || msg != "set public paths /public/*, /feed, with Funnel on for sous" {
		t.Errorf("ExposureSet = %q, %v", msg, err)
	}
	for _, call := range []func() (string, error){
		func() (string, error) { return c.ExposureReset("sous") },
		func() (string, error) { return c.ExposureMCP("sous", true) },
		func() (string, error) { return c.ExposureMCPReset("sous") },
	} {
		if _, err := call(); err != nil {
			t.Error(err)
		}
	}
	want := "set sous /public/*,/feed funnel by the CLI|reset sous|mcp sous on|mcp-reset sous"
	if got := strings.Join(f.log, "|"); got != want {
		t.Errorf("calls = %q", got)
	}
	if feed := activity(t, c); !strings.Contains(feed, "put sous's MCP endpoint on the internet from the CLI") {
		t.Errorf("activity:\n%s", feed)
	}
	// The manifest's rules are the setter's to apply; its refusal comes back.
	f.fail = errors.New(`public path "admin" must start with /`)
	if _, err := c.ExposureSet("sous", false, []string{"admin"}); err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Errorf("refusal = %v", err)
	}
}
