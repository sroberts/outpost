package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ipc"
)

// manageState is the secrets and tokens views'.
type manageState struct {
	secrets      []ipc.SecretInfo
	secretsErr   error
	secretCursor int

	tokens      []ipc.TokenInfo
	clients     []ipc.ClientInfo
	tokensErr   error
	tokenCursor int // over the tokens, then the clients
}

type secretsMsg struct {
	list []ipc.SecretInfo
	err  error
}

type tokensMsg struct {
	tokens  []ipc.TokenInfo
	clients []ipc.ClientInfo
	err     error
}

func (m Model) fetchSecrets() tea.Cmd {
	x, ok := m.secretOps()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		list, err := x.SecretList()
		return secretsMsg{list: list, err: err}
	}
}

func (m Model) fetchTokens() tea.Cmd {
	x, ok := m.tokenOps()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		tokens, err := x.TokenList("")
		if err != nil {
			return tokensMsg{err: err}
		}
		clients, err := x.TokenClients("")
		return tokensMsg{tokens: tokens, clients: clients, err: err}
	}
}

func (m Model) selectedSecret() (ipc.SecretInfo, bool) {
	if m.view != viewSecrets || m.mg.secretCursor < 0 || m.mg.secretCursor >= len(m.mg.secrets) {
		return ipc.SecretInfo{}, false
	}
	return m.mg.secrets[m.mg.secretCursor], true
}

func (m Model) selectedSecretName() string {
	s, _ := m.selectedSecret()
	return s.Name
}

func (m Model) selectedTokenID() string {
	if m.mg.tokenCursor < len(m.mg.tokens) {
		return fmt.Sprintf("%d", m.mg.tokens[m.mg.tokenCursor].ID)
	}
	return ""
}

func (m Model) selectedClientID() string {
	i := m.mg.tokenCursor - len(m.mg.tokens)
	if i >= 0 && i < len(m.mg.clients) {
		return fmt.Sprintf("%d", m.mg.clients[i].ID)
	}
	return ""
}

func (m Model) selectedUserInfo() (ipc.UserInfo, bool) {
	if m.userCursor < 0 || m.userCursor >= len(m.users) {
		return ipc.UserInfo{}, false
	}
	return m.users[m.userCursor], true
}

// handleManageKey is the secrets and tokens views' keys.
func (m Model) handleManageKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch m.view {
	case viewSecrets:
		switch msg.String() {
		case "up", "k":
			m.mg.secretCursor = max(m.mg.secretCursor-1, 0)
			return m, nil, true
		case "down", "j":
			m.mg.secretCursor = max(min(m.mg.secretCursor+1, len(m.mg.secrets)-1), 0)
			return m, nil, true
		case "n", "enter":
			next, cmd := m.openAction("secret-set")
			return next, cmd, true
		case "b":
			next, cmd := m.openAction("secret-bind")
			return next, cmd, true
		case "u":
			next, cmd := m.openAction("secret-unbind")
			return next, cmd, true
		case "d":
			next, cmd := m.openAction("secret-rm")
			return next, cmd, true
		}
	case viewTokens:
		n := len(m.mg.tokens) + len(m.mg.clients)
		switch msg.String() {
		case "up", "k":
			m.mg.tokenCursor = max(m.mg.tokenCursor-1, 0)
			return m, nil, true
		case "down", "j":
			m.mg.tokenCursor = max(min(m.mg.tokenCursor+1, n-1), 0)
			return m, nil, true
		case "n":
			next, cmd := m.openAction("token-create")
			return next, cmd, true
		case "d":
			id := "token-revoke"
			if m.mg.tokenCursor >= len(m.mg.tokens) {
				id = "client-revoke"
			}
			next, cmd := m.openAction(id)
			return next, cmd, true
		}
	}
	return m, nil, false
}

func when(t time.Time, never string) string {
	if t.IsZero() {
		return never
	}
	return t.Local().Format("2006-01-02 15:04")
}

// table draws rows (the first is the header) with the row at cursor marked;
// cursor -1 marks none.
func (m Model) grid(rows [][]string, cursor int) string {
	var b strings.Builder
	widths := cellWidths(rows)
	b.WriteString("    " + headerStyle.Render(truncate(joinCells(rows[0], widths), m.width-6)) + "\n")
	for i, row := range rows[1:] {
		marker, style := "  ", rowStyle
		if i == cursor {
			marker, style = "▸ ", selectedRow
		}
		b.WriteString("  " + style.Render(truncate(marker+joinCells(row, widths), m.width-4)) + "\n")
	}
	return b.String()
}

func (m Model) secretsPane() string {
	var b strings.Builder
	switch {
	case m.mg.secretsErr != nil:
		return "  " + errorStyle.Render(m.mg.secretsErr.Error()) + "\n"
	case m.mg.secrets == nil:
		return "  " + labelStyle.Render("loading…") + "\n"
	case len(m.mg.secrets) == 0:
		return "  " + labelStyle.Render("No secrets yet. Press n to add one.") + "\n"
	}
	rows := [][]string{{"NAME", "VARIABLE", "GIVEN TO", "REACHES", "REV", "UPDATED"}}
	for _, s := range m.mg.secrets {
		to := strings.Join(s.Services, ",")
		if s.Global {
			to = "every service"
		}
		if to == "" {
			to = "-"
		}
		reaches := strings.Join(s.Reaches, ",")
		if reaches == "" {
			reaches = "-"
		}
		rows = append(rows, []string{s.Name, s.Env, to, reaches, fmt.Sprintf("%d", s.Revision), when(s.UpdatedAt, "-")})
	}
	b.WriteString(m.grid(rows, m.mg.secretCursor))
	b.WriteString("\n  " + labelStyle.Render(truncate("Values are never shown; setting one again rotates it, and the services that get it restart.", m.width-4)) + "\n")
	return b.String()
}

func (m Model) tokensPane() string {
	var b strings.Builder
	switch {
	case m.mg.tokensErr != nil:
		return "  " + errorStyle.Render(m.mg.tokensErr.Error()) + "\n"
	case m.mg.tokens == nil && m.mg.clients == nil:
		return "  " + labelStyle.Render("loading…") + "\n"
	}
	b.WriteString("  " + m.rule("access tokens") + "\n")
	if len(m.mg.tokens) == 0 {
		b.WriteString("  " + labelStyle.Render("None. Press n to make one.") + "\n")
	} else {
		rows := [][]string{{"ID", "USER", "NAME", "APPS", "EXPIRES", "LAST USED"}}
		for _, t := range m.mg.tokens {
			apps := strings.Join(t.Apps, ",")
			if apps == "" {
				apps = "any"
			}
			if t.Internet {
				apps += " (internet)"
			}
			exp := when(t.ExpiresAt, "never")
			if t.Expired {
				exp = "expired"
			}
			name := t.Name
			if name == "" {
				name = "-"
			}
			rows = append(rows, []string{fmt.Sprintf("%d", t.ID), t.Username, name, apps, exp, when(t.LastUsedAt, "never")})
		}
		b.WriteString(m.grid(rows, m.mg.tokenCursor))
	}
	b.WriteString("\n  " + m.rule("connected clients") + "\n")
	if len(m.mg.clients) == 0 {
		b.WriteString("  " + labelStyle.Render("No OAuth clients connected.") + "\n")
		return b.String()
	}
	rows := [][]string{{"ID", "USER", "APP", "CLIENT", "REFRESHED", "EXPIRES"}}
	for _, c := range m.mg.clients {
		name := c.ClientName
		if name == "" {
			name = c.ClientID
		}
		exp := when(c.ExpiresAt, "never")
		if c.Lapsed {
			exp = "lapsed"
		}
		rows = append(rows, []string{fmt.Sprintf("%d", c.ID), c.Username, c.App, name, when(c.RefreshedAt, "-"), exp})
	}
	b.WriteString(m.grid(rows, m.mg.tokenCursor-len(m.mg.tokens)))
	return b.String()
}
