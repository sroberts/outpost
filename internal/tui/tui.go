// Package tui is Outpost's Bubble Tea dashboard (spec §15 step 10).
//
// It talks to the daemon over the same control socket the CLI uses, through
// internal/supervisor's public API, rather than reading the state store or the
// runtime directly (§12). The only exception is log tailing, which reads the
// log file for the same reason `outpost logs` does.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// refreshInterval is how often the dashboard re-reads the daemon.
const refreshInterval = 2 * time.Second

// eventLimit is how many activity entries the detail pane shows.
const eventLimit = 8

// logLines is how many trailing log lines the log pane shows.
const logLines = 12

// Client is the part of the daemon API the dashboard needs. *ipc.Client
// satisfies it; tests substitute a fake.
type Client interface {
	Available() bool
	List() ([]supervisor.ServiceStatus, error)
	Events(service string, limit int) ([]supervisor.EventRecord, error)
	Start(name string) (string, error)
	Stop(name string) (string, error)
	Restart(name string) (string, error)
}

// LogReader returns the trailing lines of a service's log. Separate from
// Client because logs come from the filesystem, not the socket.
type LogReader func(service string, lines int) (string, error)

// Model is the dashboard state.
type Model struct {
	client Client
	logs   LogReader
	keys   keyMap
	help   help.Model

	services []supervisor.ServiceStatus
	events   []supervisor.EventRecord
	logText  string

	cursor   int
	showLogs bool

	// err is a hard failure (the daemon is unreachable); notice is a transient
	// message from an action.
	err         error
	notice      string
	noticeIsErr bool

	width, height int
	loaded        bool
	quitting      bool
}

// New builds a dashboard model.
func New(client Client, logs LogReader) Model {
	return Model{
		client: client,
		logs:   logs,
		keys:   defaultKeys(),
		help:   help.New(),
		width:  80,
		height: 24,
	}
}

// --- messages ---

type servicesMsg struct {
	services []supervisor.ServiceStatus
	err      error
}

type eventsMsg struct {
	service string
	events  []supervisor.EventRecord
}

type logsMsg struct {
	service string
	text    string
	err     error
}

type actionMsg struct {
	verb    string
	service string
	message string
	err     error
}

type tickMsg time.Time

// Init starts the first load and the refresh ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchServices(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// fetchServices reads the service table from the daemon.
func (m Model) fetchServices() tea.Cmd {
	return func() tea.Msg {
		if !m.client.Available() {
			return servicesMsg{err: errNoDaemon}
		}
		services, err := m.client.List()
		if err != nil {
			return servicesMsg{err: err}
		}
		sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
		return servicesMsg{services: services}
	}
}

// fetchEvents reads the activity feed for the selected service.
func (m Model) fetchEvents(service string) tea.Cmd {
	if service == "" {
		return nil
	}
	return func() tea.Msg {
		events, err := m.client.Events(service, eventLimit)
		if err != nil {
			// The feed is decoration; a failure here must not replace the view.
			return eventsMsg{service: service}
		}
		return eventsMsg{service: service, events: events}
	}
}

// fetchLogs reads the tail of a service's log file.
func (m Model) fetchLogs(service string) tea.Cmd {
	if service == "" || m.logs == nil {
		return nil
	}
	return func() tea.Msg {
		text, err := m.logs(service, logLines)
		return logsMsg{service: service, text: text, err: err}
	}
}

// act runs a start/stop/restart against the daemon.
func (m Model) act(verb, service string) tea.Cmd {
	if service == "" {
		return nil
	}
	return func() tea.Msg {
		var (
			msg string
			err error
		)
		switch verb {
		case "start":
			msg, err = m.client.Start(service)
		case "stop":
			msg, err = m.client.Stop(service)
		case "restart":
			msg, err = m.client.Restart(service)
		}
		return actionMsg{verb: verb, service: service, message: msg, err: err}
	}
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Some terminals — and pty wrappers like script(1) — report a zero
		// size. Keep the last known good width rather than collapsing the
		// layout to nothing.
		if msg.Width > 0 {
			m.width = msg.Width
			m.help.Width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		// Keep the view current without the user asking.
		return m, tea.Batch(m.fetchServices(), tick())

	case servicesMsg:
		m.loaded = true
		if msg.err != nil {
			m.err = msg.err
			m.services = nil
			return m, nil
		}
		m.err = nil
		m.services = msg.services
		m.clampCursor()

		cmds := []tea.Cmd{m.fetchEvents(m.selectedName())}
		if m.showLogs {
			cmds = append(cmds, m.fetchLogs(m.selectedName()))
		}
		return m, tea.Batch(cmds...)

	case eventsMsg:
		// Ignore a feed that arrived for a service no longer selected.
		if msg.service == m.selectedName() {
			m.events = msg.events
		}
		return m, nil

	case logsMsg:
		if msg.service == m.selectedName() {
			if msg.err != nil {
				m.logText = "no logs yet"
			} else {
				m.logText = msg.text
			}
		}
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.notice = fmt.Sprintf("%s %s: %v", msg.verb, msg.service, msg.err)
			m.noticeIsErr = true
		} else {
			m.notice = msg.message
			m.noticeIsErr = false
		}
		// Reflect the result straight away rather than waiting for the tick.
		return m, m.fetchServices()
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		return m, nil

	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
			m.events, m.logText = nil, ""
			return m, tea.Batch(m.fetchEvents(m.selectedName()), m.maybeFetchLogs())
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.services)-1 {
			m.cursor++
			m.events, m.logText = nil, ""
			return m, tea.Batch(m.fetchEvents(m.selectedName()), m.maybeFetchLogs())
		}
		return m, nil

	case key.Matches(msg, m.keys.Logs):
		m.showLogs = !m.showLogs
		if m.showLogs {
			return m, m.fetchLogs(m.selectedName())
		}
		m.logText = ""
		return m, nil

	case key.Matches(msg, m.keys.Refresh):
		m.notice = ""
		return m, m.fetchServices()

	case key.Matches(msg, m.keys.Start):
		m.notice = "starting " + m.selectedName()
		m.noticeIsErr = false
		return m, m.act("start", m.selectedName())

	case key.Matches(msg, m.keys.Stop):
		m.notice = "stopping " + m.selectedName()
		m.noticeIsErr = false
		return m, m.act("stop", m.selectedName())

	case key.Matches(msg, m.keys.Restart):
		m.notice = "restarting " + m.selectedName()
		m.noticeIsErr = false
		return m, m.act("restart", m.selectedName())
	}
	return m, nil
}

func (m Model) maybeFetchLogs() tea.Cmd {
	if !m.showLogs {
		return nil
	}
	return m.fetchLogs(m.selectedName())
}

// selectedName is the highlighted service, or "" when there are none.
func (m Model) selectedName() string {
	if m.cursor < 0 || m.cursor >= len(m.services) {
		return ""
	}
	return m.services[m.cursor].Name
}

// selected is the highlighted service record.
func (m Model) selected() (supervisor.ServiceStatus, bool) {
	if m.cursor < 0 || m.cursor >= len(m.services) {
		return supervisor.ServiceStatus{}, false
	}
	return m.services[m.cursor], true
}

// clampCursor keeps the selection valid as services come and go.
func (m *Model) clampCursor() {
	if len(m.services) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= len(m.services) {
		m.cursor = len(m.services) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// errNoDaemon is the hard failure the dashboard explains rather than crashes on.
var errNoDaemon = fmt.Errorf("no outpost daemon is running")

// uptime renders how long a service has been up.
func uptime(s supervisor.ServiceStatus) string {
	if s.Status != "running" || s.LastStartedAt.IsZero() {
		return "-"
	}
	return shortDuration(time.Since(s.LastStartedAt))
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// timeUntil renders how long until t, or "" if it has passed.
func timeUntil(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return ""
	}
	return shortDuration(d)
}

// pad right-pads s to width display cells, for column alignment.
//
// Width is measured in cells, not bytes: the status glyphs are three bytes and
// one cell each, so byte-based padding leaves every column short.
func pad(s string, width int) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}
