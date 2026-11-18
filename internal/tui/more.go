package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ipc"
)

type daemonVersionMsg string

// fetchDaemonVersion asks once which build the daemon runs, for the header.
func (m Model) fetchDaemonVersion() tea.Cmd {
	x, ok := m.client.(DaemonInfo)
	if !ok {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		if !client.Available() {
			return nil
		}
		v, err := x.DaemonVersion()
		if err != nil {
			return nil
		}
		return daemonVersionMsg(v)
	}
}

// enterCmd fetches what the current view shows beyond what every tick
// fetches; it's what opening the view does.
func (m Model) enterCmd() tea.Cmd {
	switch m.view {
	case viewOverview:
		return m.fetchOverview()
	case viewSecrets:
		return m.fetchSecrets()
	case viewTokens:
		return m.fetchTokens()
	}
	return nil
}

// refreshMore is what a tick fetches for the views this file adds. The
// logs view keeps its own, faster, clock.
func (m Model) refreshMore() tea.Cmd {
	switch m.view {
	case viewOverview, viewSecrets, viewTokens:
		return m.enterCmd()
	}
	return nil
}

// updateMore handles the messages of the views and overlays added beside
// the original three; ok is false for any other.
func (m Model) updateMore(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case daemonVersionMsg:
		m.daemonVersion = string(msg)
		return m, nil, true
	case overviewMsg:
		return m.updateOverview(msg), nil, true
	case secretsMsg:
		m.mg.secrets, m.mg.secretsErr = msg.list, msg.err
		if m.mg.secrets == nil && msg.err == nil {
			m.mg.secrets = []ipc.SecretInfo{}
		}
		m.mg.secretCursor = max(min(m.mg.secretCursor, len(m.mg.secrets)-1), 0)
		return m, nil, true
	case tokensMsg:
		m.mg.tokensErr = msg.err
		if msg.err == nil {
			m.mg.tokens, m.mg.clients = msg.tokens, msg.clients
			if m.mg.tokens == nil {
				m.mg.tokens = []ipc.TokenInfo{}
			}
		}
		n := len(m.mg.tokens) + len(m.mg.clients)
		m.mg.tokenCursor = max(min(m.mg.tokenCursor, n-1), 0)
		return m, nil, true
	case logViewMsg:
		return m.updateLogView(msg), nil, true
	case logTickMsg:
		if m.view != viewLogs {
			m.lv.ticking = false
			return m, nil, true
		}
		return m, tea.Batch(m.fetchLogView(), logTick()), true
	case outputMsg:
		o := &outputPane{title: msg.title, text: msg.text, then: msg.then, thenKey: msg.thenKey, thenLabel: msg.thenLabel}
		if msg.err != nil {
			o.isErr = true
			if o.text != "" {
				o.text += "\n"
			}
			o.text += msg.err.Error()
		}
		// The text above was the busy notice; the pane says it now.
		m.out, m.notice = o, ""
		return m, tea.Batch(m.fetchServices(), m.refreshExtras(), m.refreshMore()), true
	}
	return m, nil, false
}

// handleMoreKey is the keys the added views and the services view's new
// shortcuts take; ok is false for the rest.
func (m Model) handleMoreKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	k := msg.String()
	// The digits jump to a view by its place on the tab line, unless a field
	// is taking them, as the log search is.
	if _, ok := m.extras(); ok && !m.typing() && len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		vs := m.views()
		if i := int(k[0] - '1'); i < len(vs) {
			next, cmd := m.enterView(vs[i])
			return next, cmd, true
		}
	}
	switch m.view {
	case viewOverview:
		return m.handleOverviewKey(msg)
	case viewLogs:
		return m.handleLogKey(msg)
	case viewSecrets, viewTokens:
		return m.handleManageKey(msg)
	case viewUsers:
		id := map[string]string{"a": "user-add", "p": "user-passwd", "g": "user-groups", "A": "user-admin",
			"t": "user-tailnet", "X": "user-rm", "n": "token-create"}[k]
		if k == "d" {
			id = "user-disable"
			if u, ok := m.selectedUserInfo(); ok && u.Disabled {
				id = "user-enable"
			}
		}
		if id != "" {
			next, cmd := m.openAction(id)
			return next, cmd, true
		}
	case viewServices:
		switch k {
		case "g":
			m.graphLong = !m.graphLong
			return m, nil, true
		case "z":
			m.zoom = !m.zoom
			return m, nil, true
		case "L":
			if m.logs == nil || m.selectedName() == "" {
				return m, nil, true
			}
			m.lv.service, m.lv.all, m.lv.events = m.selectedName(), false, false
			m.lv.reset()
			next, cmd := m.enterView(viewLogs)
			return next, cmd, true
		case "b", "u", "S":
			id := map[string]string{"b": "backup", "u": "update", "S": "status"}[k]
			next, cmd := m.openAction(id)
			return next, cmd, true
		}
	}
	return m, nil, false
}

// moreHelp is the key help for the added views; "" for the others.
func (m Model) moreHelp(key func(k, what string) string, sep string) string {
	join := func(parts ...string) string {
		out := ""
		for i, p := range parts {
			if i > 0 {
				out += sep
			}
			out += p
		}
		return out
	}
	switch m.view {
	case viewOverview:
		return join(key("↑/↓", "tile"), key("enter", "open"), key("tab", "views"), key(":", "commands"), key("q", "quit"))
	case viewLogs:
		return m.logHelp(key, sep)
	case viewSecrets:
		return join(key("n", "set"), key("b", "give"), key("u", "take"), key("d", "delete"), key("tab", "views"), key(":", "commands"), key("q", "quit"))
	case viewTokens:
		return join(key("n", "create"), key("d", "revoke"), key("tab", "views"), key(":", "commands"), key("q", "quit"))
	}
	return ""
}
