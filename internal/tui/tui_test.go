package tui

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeClient stands in for the daemon.
type fakeClient struct {
	mu        sync.Mutex
	available bool
	services  []supervisor.ServiceStatus
	events    []supervisor.EventRecord
	listErr   error
	actionErr error
	calls     []string
}

func newFakeClient(services ...supervisor.ServiceStatus) *fakeClient {
	return &fakeClient{available: true, services: services}
}

func (c *fakeClient) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.available
}

func (c *fakeClient) List() ([]supervisor.ServiceStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.services, c.listErr
}

func (c *fakeClient) Events(service string, limit int) ([]supervisor.EventRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events, nil
}

func (c *fakeClient) record(verb, name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, verb+":"+name)
	if c.actionErr != nil {
		return "", c.actionErr
	}
	return verb + "ed " + name, nil
}

func (c *fakeClient) Start(name string) (string, error)   { return c.record("start", name) }
func (c *fakeClient) Stop(name string) (string, error)    { return c.record("stop", name) }
func (c *fakeClient) Restart(name string) (string, error) { return c.record("restart", name) }

func (c *fakeClient) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.calls...)
}

func svc(name, status string) supervisor.ServiceStatus {
	return supervisor.ServiceStatus{
		Name: name, Runtime: "process", Status: status, Desired: "running",
		Port: 8080, Healthcheck: "/up", TailscaleName: name,
		RestartPolicy: "on-failure", Backoff: 5,
		LastStartedAt: time.Now().Add(-2 * time.Hour),
	}
}

// run applies a message and returns the concrete model back.
func run(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	out, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, not Model", next)
	}
	return out, cmd
}

// exec runs a command and returns its message.
func exec(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return cmd()
}

// loaded builds a model already populated with services.
func loaded(t *testing.T, c *fakeClient) Model {
	t.Helper()
	m := New(c, nil)
	m.width = 100
	m, _ = run(t, m, servicesMsg{services: c.services})
	return m
}

func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestView_ShowsServicesAndStatus(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"), svc("kosync", "failed"))
	view := loaded(t, c).View()

	for _, want := range []string{"Outpost", "2 services", "plumbline", "kosync", "running", "failed"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

// The dashboard must explain a missing daemon rather than showing an empty table.
func TestView_NoDaemonIsExplained(t *testing.T) {
	c := newFakeClient()
	c.available = false

	m := New(c, nil)
	msg := exec(t, m.fetchServices())
	m, _ = run(t, m, msg)

	view := m.View()
	if !strings.Contains(view, "no outpost daemon is running") {
		t.Errorf("view does not name the problem:\n%s", view)
	}
	if !strings.Contains(view, "outpost serve") {
		t.Errorf("view does not say how to fix it:\n%s", view)
	}
	if !strings.Contains(view, "reconnects") {
		t.Errorf("view should say it recovers on its own:\n%s", view)
	}
}

func TestView_EmptyServiceListIsExplained(t *testing.T) {
	m := loaded(t, newFakeClient())
	if view := m.View(); !strings.Contains(view, "No services declared") {
		t.Errorf("unhelpful empty state:\n%s", view)
	}
}

func TestNavigation_MovesCursorWithinBounds(t *testing.T) {
	c := newFakeClient(svc("a", "running"), svc("b", "running"), svc("c", "running"))
	m := loaded(t, c)

	if got := m.selectedName(); got != "a" {
		t.Fatalf("initial selection = %q; want a", got)
	}
	m, _ = run(t, m, keyMsg("j"))
	m, _ = run(t, m, keyMsg("j"))
	if got := m.selectedName(); got != "c" {
		t.Errorf("after two downs = %q; want c", got)
	}
	// Must not run off the end.
	m, _ = run(t, m, keyMsg("j"))
	if got := m.selectedName(); got != "c" {
		t.Errorf("cursor ran past the last service: %q", got)
	}

	m, _ = run(t, m, keyMsg("k"))
	m, _ = run(t, m, keyMsg("k"))
	m, _ = run(t, m, keyMsg("k"))
	if got := m.selectedName(); got != "a" {
		t.Errorf("cursor ran past the first service: %q", got)
	}
}

func TestActions_SendToTheDaemon(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"s", "start:plumbline"},
		{"x", "stop:plumbline"},
		{"r", "restart:plumbline"},
	} {
		c := newFakeClient(svc("plumbline", "running"))
		m := loaded(t, c)

		m, cmd := run(t, m, keyMsg(tc.key))
		if cmd == nil {
			t.Fatalf("%s: no command issued", tc.key)
		}
		exec(t, cmd)

		if got := c.recorded(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s produced %v; want [%s]", tc.key, got, tc.want)
		}
	}
}

func TestActions_ResultIsShown(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"))
	m := loaded(t, c)

	m, _ = run(t, m, actionMsg{verb: "stop", service: "plumbline", message: "stopped plumbline"})
	if view := m.View(); !strings.Contains(view, "stopped plumbline") {
		t.Errorf("action result not shown:\n%s", view)
	}
}

func TestActions_FailureIsShownAsAnError(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"))
	m := loaded(t, c)

	m, _ = run(t, m, actionMsg{verb: "start", service: "plumbline", err: errors.New("no such service")})
	view := m.View()
	if !strings.Contains(view, "no such service") {
		t.Errorf("error not surfaced:\n%s", view)
	}
	if !m.noticeIsErr {
		t.Error("failure was not marked as an error")
	}
}

// An action must refresh immediately rather than waiting for the next tick.
func TestActions_TriggerARefresh(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"))
	m := loaded(t, c)

	_, cmd := run(t, m, actionMsg{verb: "stop", service: "plumbline", message: "stopped"})
	if cmd == nil {
		t.Fatal("no refresh issued after an action")
	}
	if _, ok := exec(t, cmd).(servicesMsg); !ok {
		t.Error("the command after an action does not refresh the service list")
	}
}

// Pressing a key on an empty list must not panic or send a nameless request.
func TestActions_NoSelectionIsSafe(t *testing.T) {
	c := newFakeClient()
	m := loaded(t, c)

	for _, k := range []string{"s", "x", "r", "l"} {
		m, cmd := run(t, m, keyMsg(k))
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if am, ok := msg.(actionMsg); ok && am.service == "" {
					t.Errorf("%s sent an action with no service", k)
				}
			}
		}
		_ = m
	}
	if len(c.recorded()) != 0 {
		t.Errorf("actions were sent with nothing selected: %v", c.recorded())
	}
}

// Services disappearing must not leave the cursor out of range.
func TestCursor_SurvivesServicesGoingAway(t *testing.T) {
	c := newFakeClient(svc("a", "running"), svc("b", "running"), svc("c", "running"))
	m := loaded(t, c)
	m, _ = run(t, m, keyMsg("j"))
	m, _ = run(t, m, keyMsg("j"))

	// Two services removed from the manifest.
	m, _ = run(t, m, servicesMsg{services: []supervisor.ServiceStatus{svc("a", "running")}})
	if m.cursor != 0 {
		t.Errorf("cursor = %d after the list shrank; want 0", m.cursor)
	}
	if view := m.View(); view == "" {
		t.Error("view is empty after the list shrank")
	}
}

func TestEvents_ShownForSelectedServiceOnly(t *testing.T) {
	c := newFakeClient(svc("a", "running"), svc("b", "running"))
	m := loaded(t, c)

	m, _ = run(t, m, eventsMsg{service: "a", events: []supervisor.EventRecord{
		{Service: "a", Kind: "started", Message: "started (id 42)", At: time.Now()},
	}})
	if !strings.Contains(m.View(), "started (id 42)") {
		t.Error("event for the selected service not shown")
	}

	// A feed arriving for a service that is no longer selected must be ignored.
	m, _ = run(t, m, eventsMsg{service: "b", events: []supervisor.EventRecord{
		{Service: "b", Kind: "started", Message: "wrong service", At: time.Now()},
	}})
	if strings.Contains(m.View(), "wrong service") {
		t.Error("events for an unselected service were rendered")
	}
}

func TestLogs_ToggleFetchesAndShows(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"))
	m := New(c, func(service string, lines int) (string, error) {
		return "line one\nline two", nil
	})
	m.width = 100
	m, _ = run(t, m, servicesMsg{services: c.services})

	if strings.Contains(m.View(), "line one") {
		t.Fatal("logs shown before being asked for")
	}
	m, cmd := run(t, m, keyMsg("l"))
	if !m.showLogs {
		t.Fatal("log pane not toggled on")
	}
	msg := exec(t, cmd)
	m, _ = run(t, m, msg)

	if !strings.Contains(m.View(), "line one") {
		t.Errorf("log text not rendered:\n%s", m.View())
	}

	m, _ = run(t, m, keyMsg("l"))
	if m.showLogs {
		t.Error("log pane not toggled off")
	}
}

func TestLogs_MissingFileIsHandled(t *testing.T) {
	c := newFakeClient(svc("plumbline", "running"))
	m := New(c, func(string, int) (string, error) { return "", errors.New("no such file") })
	m.width = 100
	m, _ = run(t, m, servicesMsg{services: c.services})

	m, cmd := run(t, m, keyMsg("l"))
	m, _ = run(t, m, exec(t, cmd))
	if !strings.Contains(m.View(), "no logs yet") {
		t.Errorf("missing log file not handled gracefully:\n%s", m.View())
	}
}

func TestHalted_IsCalledOutInTheDetail(t *testing.T) {
	s := svc("broken", "failed")
	s.Halted = true
	m := loaded(t, newFakeClient(s))

	view := m.View()
	if !strings.Contains(view, "halted") {
		t.Errorf("halted state not surfaced:\n%s", view)
	}
	if !strings.Contains(view, "press s to start") {
		t.Errorf("halted detail does not say how to recover:\n%s", view)
	}
}

func TestQuit_StopsTheProgram(t *testing.T) {
	m := loaded(t, newFakeClient(svc("a", "running")))
	m, cmd := run(t, m, keyMsg("q"))
	if !m.quitting {
		t.Error("model not marked as quitting")
	}
	if cmd == nil {
		t.Fatal("no quit command issued")
	}
	if view := m.View(); view != "" {
		t.Errorf("view should be empty while quitting, got %q", view)
	}
}

func TestHelp_Toggles(t *testing.T) {
	m := loaded(t, newFakeClient(svc("a", "running")))
	before := m.View()
	m, _ = run(t, m, keyMsg("?"))
	if m.View() == before {
		t.Error("help did not expand")
	}
}

func TestTick_SchedulesAnotherRefresh(t *testing.T) {
	m := loaded(t, newFakeClient(svc("a", "running")))
	_, cmd := run(t, m, tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tick produced no follow-up")
	}
}

// The view must render at any terminal size without panicking.
func TestView_HandlesNarrowTerminals(t *testing.T) {
	c := newFakeClient(svc("a-service-with-a-long-name", "running"))
	m := loaded(t, c)

	for _, w := range []int{0, 1, 10, 40, 200} {
		m, _ = run(t, m, tea.WindowSizeMsg{Width: w, Height: 24})
		if got := m.View(); got == "" {
			t.Errorf("width %d produced an empty view", w)
		}
	}
}

func TestWindowSize_IsRecorded(t *testing.T) {
	m := loaded(t, newFakeClient(svc("a", "running")))
	m, _ = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.width != 120 || m.height != 40 {
		t.Errorf("size = %dx%d; want 120x40", m.width, m.height)
	}
}

// Some terminals and pty wrappers report a zero size; taking it literally
// collapses the layout.
func TestWindowSize_ZeroIsIgnored(t *testing.T) {
	m := loaded(t, newFakeClient(svc("a", "running")))
	m, _ = run(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = run(t, m, tea.WindowSizeMsg{Width: 0, Height: 0})

	if m.width != 100 || m.height != 30 {
		t.Errorf("size = %dx%d after a zero-size message; want the last good 100x30", m.width, m.height)
	}
	if !strings.Contains(m.View(), "────") {
		t.Errorf("layout collapsed after a zero-size message:\n%s", m.View())
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30s",
		5 * time.Minute:  "5m",
		3 * time.Hour:    "3h",
		50 * time.Hour:   "2d",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q; want %q", d, got, want)
		}
	}
}

// truncate must respect display width, not byte length: the ellipsis is three
// bytes and log lines may hold arbitrary UTF-8.
func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
	}{
		{"hello world", 8},
		{"hello world", 1},
		{"ünïcödé is wíder than bytes", 10},
		{"日本語のログ行です", 6},
	} {
		got := truncate(tc.in, tc.width)
		if w := lipgloss.Width(got); w > tc.width {
			t.Errorf("truncate(%q, %d) = %q, %d cells wide", tc.in, tc.width, got, w)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q, which is not valid UTF-8", tc.in, tc.width, got)
		}
	}

	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate shortened a string that fits: %q", got)
	}
	if got := truncate("anything", 0); got != "" {
		t.Errorf("truncate to zero width = %q; want empty", got)
	}
}

func TestListError_IsSurfaced(t *testing.T) {
	c := newFakeClient()
	c.listErr = fmt.Errorf("socket closed")
	m := New(c, nil)

	m, _ = run(t, m, exec(t, m.fetchServices()))
	if !strings.Contains(m.View(), "socket closed") {
		t.Errorf("list error not surfaced:\n%s", m.View())
	}
}

// Columns must line up in display cells. The status glyphs are three bytes and
// one cell each, so byte-based padding silently misaligns every row.
func TestTable_ColumnsAlign(t *testing.T) {
	c := newFakeClient(
		svc("plumbline", "running"),
		svc("kosync", "unhealthy"),
		svc("a", "failed"),
	)
	c.services[2].Halted = true      // the widest status cell
	c.services[1].RestartCount = 100 // the widest restart count
	m := loaded(t, c)

	var widths []int
	var lines []string
	for _, line := range strings.Split(m.table(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		plain := stripANSI(line)
		widths = append(widths, lipgloss.Width(plain))
		lines = append(lines, plain)
	}
	if len(widths) < 4 {
		t.Fatalf("expected a header and three rows, got %d", len(widths))
	}

	// Every column is padded, so header and rows must be identically wide.
	for i, w := range widths {
		if w != widths[0] {
			t.Errorf("line %d is %d cells wide, header is %d:\n%s",
				i, w, widths[0], strings.Join(lines, "\n"))
		}
	}
}

// stripANSI removes styling so widths can be measured on the text itself.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pad must count display cells, not bytes.
func TestPad_CountsDisplayCells(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
	}{
		{"● running", 20},
		{"plain", 20},
		{"✖ failed (halted)", 20},
		{"日本語", 20},
	} {
		if got := lipgloss.Width(pad(tc.in, tc.width)); got != tc.width {
			t.Errorf("pad(%q, %d) is %d cells wide; want %d", tc.in, tc.width, got, tc.width)
		}
	}
	// Over-long input is returned untouched rather than truncated.
	if got := pad("something long", 4); got != "something long" {
		t.Errorf("pad shortened an over-long value: %q", got)
	}
}
