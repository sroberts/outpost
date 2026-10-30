package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/supervisor"
)

// extrasClient is a daemon with users and a gateway as well as services.
type extrasClient struct {
	*fakeClient
	gw        *gateway.Status
	users     []ipc.UserInfo
	signIns   map[string][]ipc.SignInInfo
	loggedOut []string
}

func (c *extrasClient) GatewayStatus() (gateway.Status, error) {
	if c.gw == nil {
		return gateway.Status{}, errGatewayOff
	}
	return *c.gw, nil
}

var errGatewayOff = &gatewayOffErr{}

type gatewayOffErr struct{}

func (*gatewayOffErr) Error() string { return "the gateway is off; turn it on" }

func (c *extrasClient) UserList() ([]ipc.UserInfo, error) { return c.users, nil }

func (c *extrasClient) UserSignIns(u string) ([]ipc.SignInInfo, error) { return c.signIns[u], nil }

func (c *extrasClient) UserLogout(u string) (string, error) {
	c.loggedOut = append(c.loggedOut, u)
	return "ended 2 session(s) for " + u, nil
}

func newExtras() *extrasClient {
	notes := svc("notes", "running")
	notes.Title, notes.Description = "Notes", "Plain-text notes"
	notes.Allow, notes.Hidden = []string{"group:family"}, true
	notes.PublicPaths = []string{"/public/*"}
	return &extrasClient{
		fakeClient: newFakeClient(notes, svc("wiki", "stopped")),
		gw: &gateway.Status{
			PublicURL: "https://outpost.example", Listen: "127.0.0.1:8443", Domain: "outpost.example",
			Routes: []gateway.RouteStatus{{Name: "notes", URL: "https://notes.outpost.example", Status: "running"}},
		},
		users: []ipc.UserInfo{
			{Username: "bob", HasPassword: true},
			{Username: "alice", Admin: true, HasPassword: true, Groups: []string{"family"}, Sessions: 2,
				LastLoginAt: time.Date(2026, 10, 30, 9, 5, 0, 0, time.Local)},
		},
		signIns: map[string][]ipc.SignInInfo{
			"alice": {{UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) Version/17.5 Mobile/15E148 Safari/604.1",
				IP: "192.0.2.7", CreatedAt: time.Date(2026, 10, 30, 9, 5, 0, 0, time.Local), Apps: []string{"notes"}}},
		},
	}
}

// drain runs a command and every command its messages produce, the way the
// program loop would, applying each message to the model.
func drain(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if msg == nil {
			continue
		}
		var next tea.Cmd
		m, next = run(t, m, msg)
		queue = append(queue, next)
	}
	return m
}

func loadedExtras(t *testing.T, c *extrasClient) Model {
	t.Helper()
	m := New(c, nil)
	m.width, m.height = 100, 40
	// Init less its tick, which would wait out the refresh interval.
	return drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway()))
}

func TestExtras_ServiceDetailShowsTheGatewaySide(t *testing.T) {
	view := loadedExtras(t, newExtras()).View()
	for _, want := range []string{
		"▸ Services", "Users", "Gateway",
		"Notes", "Plain-text notes", "https://notes.outpost.example",
		"hidden from the catalog; only group:family; public: /public/*",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestExtras_UsersViewAndSigningOut(t *testing.T) {
	c := newExtras()
	m := loadedExtras(t, c)
	m, cmd := run(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = drain(t, m, cmd)
	view := m.View()
	for _, want := range []string{"▸ Users", "alice", "admin", "family", "2026-10-30 09:05", "bob", "never",
		"signed in: alice", "Safari on iPhone", "192.0.2.7", "notes"} {
		if !strings.Contains(view, want) {
			t.Errorf("users view missing %q:\n%s", want, view)
		}
	}
	// The services view's keys do nothing here.
	m, cmd = run(t, m, keyMsg("x"))
	if cmd != nil || len(c.recorded()) != 0 {
		t.Errorf("x acted on a service from the users view: %v", c.recorded())
	}

	// L asks first; a second L signs out; anything between calls it off.
	m, cmd = run(t, m, keyMsg("L"))
	if cmd != nil || !strings.Contains(m.View(), "press L again to sign alice out everywhere") {
		t.Fatalf("first L didn't ask:\n%s", m.View())
	}
	m, _ = run(t, m, keyMsg("j"))
	m, cmd = run(t, m, keyMsg("L"))
	if cmd != nil || len(c.loggedOut) != 0 {
		t.Fatalf("an L after moving signed someone out: %v", c.loggedOut)
	}
	m, cmd = run(t, m, keyMsg("L"))
	m = drain(t, m, cmd)
	if len(c.loggedOut) != 1 || c.loggedOut[0] != "bob" {
		t.Errorf("signed out %v; want bob", c.loggedOut)
	}
	if !strings.Contains(m.View(), "ended 2 session(s) for bob") {
		t.Errorf("no confirmation:\n%s", m.View())
	}
}

func TestExtras_GatewayView(t *testing.T) {
	c := newExtras()
	c.events = []supervisor.EventRecord{{Service: gateway.ActivityService, Kind: "sign-in", Message: "alice signed in from 192.0.2.7", At: time.Now()}}
	m := loadedExtras(t, c)
	for range 2 {
		var cmd tea.Cmd
		m, cmd = run(t, m, tea.KeyMsg{Type: tea.KeyTab})
		m = drain(t, m, cmd)
	}
	view := m.View()
	for _, want := range []string{"▸ Gateway", "https://outpost.example", "127.0.0.1:8443 (TLS terminated in front)",
		"<name>.outpost.example", "https://notes.outpost.example", "alice signed in from 192.0.2.7"} {
		if !strings.Contains(view, want) {
			t.Errorf("gateway view missing %q:\n%s", want, view)
		}
	}

	c.gw = nil
	m = drain(t, m, m.fetchGateway())
	if !strings.Contains(m.View(), "The gateway is off.") {
		t.Errorf("gateway off isn't explained:\n%s", m.View())
	}
}

func TestExtras_ConfigBanner(t *testing.T) {
	stale := false
	m := New(newExtras(), nil).WithConfigCheck(func() bool { return stale })
	m.width, m.height = 100, 40
	m = drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway()))
	if strings.Contains(m.View(), "config.toml has changed") {
		t.Fatal("banner with nothing changed")
	}
	stale = true
	m = drain(t, m, m.fetchGateway())
	if !strings.Contains(m.View(), "config.toml has changed since the daemon started") {
		t.Errorf("no banner:\n%s", m.View())
	}
}

// Free-text titles and names are measured in display cells.
func TestExtras_WideTextLinesUp(t *testing.T) {
	c := newExtras()
	c.users = append(c.users, ipc.UserInfo{Username: "日本語", HasPassword: true})
	m := loadedExtras(t, c)
	m, cmd := run(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = drain(t, m, cmd)
	var cols []int
	for _, line := range strings.Split(m.View(), "\n") {
		if i := strings.Index(line, "active"); i >= 0 {
			cols = append(cols, lipgloss.Width(line[:i]))
		}
	}
	if len(cols) != 3 || cols[0] != cols[1] || cols[1] != cols[2] {
		t.Errorf("STATUS column at cells %v; want three equal", cols)
	}
}

// Every view's lines fit an 80-column terminal, key help included.
func TestExtras_FitEightyColumns(t *testing.T) {
	m := loadedExtras(t, newExtras())
	m.width = 80
	m.help.Width = 80
	for i := range viewNames {
		for _, line := range strings.Split(m.View(), "\n") {
			if w := lipgloss.Width(line); w > 80 {
				t.Errorf("%s view: %d cells: %q", viewNames[i], w, line)
			}
		}
		var cmd tea.Cmd
		m, cmd = run(t, m, tea.KeyMsg{Type: tea.KeyTab})
		m = drain(t, m, cmd)
	}
}
