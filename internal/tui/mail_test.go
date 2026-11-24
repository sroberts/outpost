package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/mail"
)

// mailClient is a full client with mail and internet access too.
type mailClient struct {
	*fullClient
	calls []string
}

func (c *mailClient) MailStatus() (ipc.MailOverview, error) {
	return ipc.MailOverview{
		Account: mail.Status{Address: "me@gmail.com", HasPassword: true, Checked: time.Now().Add(-time.Minute), Routed: 3},
		Secret:  "outpost-mail-password",
		Apps: []ipc.MailApp{
			{Name: "notes", Send: true, Routes: []mail.Route{{Route: "notes", Address: "me+notes@gmail.com"},
				{Route: "k3x9", Alias: true, Note: "receipts", Address: "me+k3x9@gmail.com"}}},
			{Name: "wiki", Held: 4, HeldNewest: time.Now()},
		},
	}, nil
}
func (c *mailClient) note(f string, a ...any) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf(f, a...))
	return "done", nil
}
func (c *mailClient) MailAccount(address, password string) (string, error) {
	return c.note("account %s %s", address, password)
}
func (c *mailClient) MailCheck() (string, error) { return c.note("check") }
func (c *mailClient) MailAliasAdd(service, n string) (string, error) {
	return c.note("alias %s %s", service, n)
}
func (c *mailClient) MailAliasRemove(service, route string) (string, error) {
	return c.note("unalias %s %s", service, route)
}
func (c *mailClient) MailSend(service string, on bool) (string, error) {
	return c.note("send %s %v", service, on)
}
func (c *mailClient) MailRegenerate(service string) (string, error) {
	return c.note("token %s", service)
}
func (c *mailClient) ExposureSet(service string, funnel bool, paths []string) (string, error) {
	return c.note("expose %s %v %s", service, funnel, strings.Join(paths, ","))
}
func (c *mailClient) ExposureReset(service string) (string, error) {
	return c.note("expose-reset %s", service)
}
func (c *mailClient) ExposureMCP(service string, on bool) (string, error) {
	return c.note("mcp %s %v", service, on)
}
func (c *mailClient) ExposureMCPReset(service string) (string, error) {
	return c.note("mcp-reset %s", service)
}

func mailDashboard(t *testing.T) (Model, *mailClient) {
	t.Helper()
	c := &mailClient{fullClient: newSecretsClient()}
	m := New(c, nil)
	m.width, m.height = 120, 40
	m = drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway()))
	return m, c
}

func TestMail_ViewShowsTheAccountAndEachApp(t *testing.T) {
	m, _ := mailDashboard(t)
	var cmd tea.Cmd
	m, cmd = m.enterView(viewMail)
	m = drain(t, m, cmd)
	v := stripANSI(m.View())
	for _, want := range []string{"Mail", "me@gmail.com", "checked 1m ago", "outpost-mail-password",
		"me+k3x9@gmail.com (receipts)", "4, newest", "1 app may send"} {
		if !strings.Contains(v, want) {
			t.Errorf("mail view lacks %q:\n%s", want, v)
		}
	}
}

// The view's keys open their actions on the app under the cursor.
func TestMail_KeysActOnTheSelectedApp(t *testing.T) {
	m, c := mailDashboard(t)
	var cmd tea.Cmd
	m, cmd = m.enterView(viewMail)
	m = drain(t, m, cmd)

	// notes may send, so s offers to turn it off.
	m = press(t, m, "s")
	if m.pal.form == nil || *m.pal.form.values["service"] != "notes" || *m.pal.form.values["send"] != "off" {
		t.Fatalf("s didn't open sending for notes, off: %+v", m.pal.form)
	}
	m = press(t, m, "enter", "enter")
	// d starts on the app's newest alias, and asks before removing it.
	m = press(t, m, "d", "enter", "enter")
	if !m.pal.form.confirming {
		t.Fatal("removing an address wasn't asked about")
	}
	m = press(t, m, "y")
	// Down to wiki, and a new address for it.
	m = press(t, m, "down", "n", "enter")
	for _, r := range "invoices" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")

	want := "send notes false|unalias notes +k3x9|alias wiki invoices"
	if got := strings.Join(c.calls, "|"); got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// The app password is typed hidden and never drawn.
func TestMail_AccountPasswordIsNeverShown(t *testing.T) {
	m, c := mailDashboard(t)
	m, _ = m.openAction("mail-account")
	for _, r := range "me@gmail.com" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	m = press(t, m, "abcd", " ", "efgh")
	if v := m.View(); strings.Contains(v, "efgh") {
		t.Fatalf("the app password is on screen:\n%s", v)
	}
	_ = press(t, m, "enter")
	if len(c.calls) != 1 || c.calls[0] != "account me@gmail.com abcd efgh" {
		t.Errorf("calls = %v", c.calls)
	}
}

// Public paths and Funnel start from what the service has now, and putting
// anything on the internet is asked about first.
func TestExposure_StartsFromTheServiceAndAsks(t *testing.T) {
	m, c := mailDashboard(t)
	sel, _ := m.selected()
	m, _ = m.openAction("expose")
	if got := *m.pal.form.values["paths"]; got != strings.Join(sel.PublicPaths, " ") {
		t.Fatalf("paths = %q, want the service's %v", got, sel.PublicPaths)
	}
	var cmd tea.Cmd
	m = press(t, m, "enter", "enter")
	m, cmd = run(t, m, keyMsgType("right")) // funnel on
	m = drain(t, m, cmd)
	m = press(t, m, "enter")
	if !m.pal.form.confirming || !strings.Contains(m.View(), "on the internet, with no login") {
		t.Fatalf("not asked:\n%s", m.View())
	}
	m = press(t, m, "y")
	want := fmt.Sprintf("expose %s true %s", sel.Name, strings.Join(sel.PublicPaths, ","))
	if len(c.calls) != 1 || c.calls[0] != want {
		t.Errorf("calls = %v, want %q", c.calls, want)
	}
}
