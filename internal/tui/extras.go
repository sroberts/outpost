package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/supervisor"
)

// Extras is the rest of the daemon API the dashboard can show: users and
// their sign-ins, and the gateway. *ipc.Client satisfies it. A Client
// without it gets the services view alone, as before.
type Extras interface {
	GatewayStatus() (gateway.Status, error)
	UserList() ([]ipc.UserInfo, error)
	UserSignIns(username string) ([]ipc.SignInInfo, error)
	UserLogout(username string) (string, error)
}

// view is which page the dashboard shows.
type view int

const (
	viewServices view = iota
	viewUsers
	viewGateway
)

var viewNames = []string{"Services", "Users", "Gateway"}

// WithConfigCheck has the header say when config.toml no longer matches
// what the daemon started with. changed is asked on every refresh; the CLI
// owns the comparison, since it reads the file.
func (m Model) WithConfigCheck(changed func() bool) Model {
	m.configChanged = changed
	return m
}

// extras is the client's Extras, if it has them.
func (m Model) extras() (Extras, bool) {
	x, ok := m.client.(Extras)
	return x, ok
}

// --- messages ---

type gatewayMsg struct {
	status *gateway.Status // nil: the gateway is off, or couldn't be asked
	err    error
	events []supervisor.EventRecord
	config bool // config.toml has changed since the daemon started
}

type usersMsg struct {
	users []ipc.UserInfo
	err   error
}

type signInsMsg struct {
	user    string
	signIns []ipc.SignInInfo
}

// fetchGateway reads the gateway's state, its recent activity, and whether
// the daemon's settings are stale. The services view uses it too, for each
// app's address.
func (m Model) fetchGateway() tea.Cmd {
	x, ok := m.extras()
	check := m.configChanged
	if !ok && check == nil {
		return nil
	}
	client := m.client
	return func() tea.Msg {
		var msg gatewayMsg
		if check != nil {
			msg.config = check()
		}
		if !ok || !client.Available() {
			return msg
		}
		st, err := x.GatewayStatus()
		if err == nil {
			msg.status = &st
			// The feed is decoration here, as on the services view.
			msg.events, _ = client.Events(gateway.ActivityService, eventLimit)
		} else {
			msg.err = err
		}
		return msg
	}
}

func (m Model) fetchUsers() tea.Cmd {
	x, ok := m.extras()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		users, err := x.UserList()
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		return usersMsg{users: users, err: err}
	}
}

func (m Model) fetchSignIns(user string) tea.Cmd {
	x, ok := m.extras()
	if !ok || user == "" {
		return nil
	}
	return func() tea.Msg {
		list, err := x.UserSignIns(user)
		if err != nil {
			return nil
		}
		return signInsMsg{user: user, signIns: list}
	}
}

// signOut ends every session a user has, as `outpost user logout` does.
func (m Model) signOut(user string) tea.Cmd {
	x, ok := m.extras()
	if !ok || user == "" {
		return nil
	}
	return func() tea.Msg {
		msg, err := x.UserLogout(user)
		return actionMsg{verb: "sign out", service: user, message: msg, err: err}
	}
}

// refreshExtras is what a tick fetches beyond the services.
func (m Model) refreshExtras() tea.Cmd {
	cmds := []tea.Cmd{m.fetchGateway()}
	if m.view == viewUsers {
		cmds = append(cmds, m.fetchUsers(), m.fetchSignIns(m.selectedUser()))
	}
	return tea.Batch(cmds...)
}

// updateExtras handles the messages this file adds. ok is false for any
// other message.
func (m Model) updateExtras(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case gatewayMsg:
		m.gateway, m.gatewayErr, m.gatewayEvents = msg.status, msg.err, msg.events
		m.configStale = msg.config
		return m, nil, true
	case usersMsg:
		if msg.err != nil {
			m.usersErr = msg.err
			return m, nil, true
		}
		was := m.selectedUser()
		m.users, m.usersErr = msg.users, nil
		m.userCursor = 0
		for i, u := range m.users {
			if u.Username == was {
				m.userCursor = i
			}
		}
		return m, m.fetchSignIns(m.selectedUser()), true
	case signInsMsg:
		if msg.user == m.selectedUser() {
			m.signIns = msg.signIns
		}
		return m, nil, true
	}
	return m, nil, false
}

// handleExtraKey handles the keys the extra views add. ok is false when the
// key is the services view's to handle.
func (m Model) handleExtraKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if _, ok := m.extras(); !ok {
		return m, nil, false
	}
	switch msg.String() {
	case "tab", "shift+tab":
		step := 1
		if msg.String() == "shift+tab" {
			step = len(viewNames) - 1
		}
		m.view = (m.view + view(step)) % view(len(viewNames))
		m.confirm, m.notice = "", ""
		return m, m.refreshExtras(), true
	}
	if m.view == viewServices {
		return m, nil, false
	}
	// Anything but a second L calls off a pending sign-out.
	pending := m.confirm
	m.confirm = ""
	switch {
	case msg.String() == "up" || msg.String() == "k":
		if m.view == viewUsers && m.userCursor > 0 {
			m.userCursor--
			m.signIns = nil
			return m, m.fetchSignIns(m.selectedUser()), true
		}
		return m, nil, true
	case msg.String() == "down" || msg.String() == "j":
		if m.view == viewUsers && m.userCursor < len(m.users)-1 {
			m.userCursor++
			m.signIns = nil
			return m, m.fetchSignIns(m.selectedUser()), true
		}
		return m, nil, true
	case msg.String() == "L" && m.view == viewUsers:
		user := m.selectedUser()
		if user == "" {
			return m, nil, true
		}
		if pending != user {
			// Ending every session someone has is worth a second key.
			m.confirm = user
			m.notice = "press L again to sign " + user + " out everywhere, on every device"
			m.noticeIsErr = false
			return m, nil, true
		}
		m.notice = "signing out " + user
		m.noticeIsErr = false
		return m, m.signOut(user), true
	case msg.String() == "R":
		m.notice = ""
		return m, tea.Batch(m.fetchServices(), m.refreshExtras()), true
	case msg.String() == "s" || msg.String() == "x" || msg.String() == "r" || msg.String() == "l":
		// The services view's actions mean nothing here; they mustn't act
		// on a service the operator can't see.
		return m, nil, true
	}
	return m, nil, false
}

func (m Model) selectedUser() string {
	if m.userCursor < 0 || m.userCursor >= len(m.users) {
		return ""
	}
	return m.users[m.userCursor].Username
}

// tabs is the line naming the views, the current one marked.
func (m Model) tabs() string {
	parts := make([]string, 0, len(viewNames))
	for i, name := range viewNames {
		if view(i) == m.view {
			parts = append(parts, selectedRow.Render("▸ "+name))
		} else {
			parts = append(parts, labelStyle.Render("  "+name))
		}
	}
	return "  " + strings.Join(parts, "  ") + "  " + labelStyle.Render("(tab)")
}

// configBanner says the daemon is running settings the file no longer has.
func (m Model) configBanner() string {
	if !m.configStale {
		return ""
	}
	return "  " + errorStyle.Render("config.toml has changed since the daemon started; restart the daemon to apply it")
}

// serviceGateway is the selected service's lines about the gateway and the
// catalog, for the detail pane: what it's called, where it is, who may open
// it.
func (m Model) serviceGateway(s supervisor.ServiceStatus, kv func(k, v string)) {
	kv("title", s.Title)
	kv("about", s.Description)
	if m.gateway != nil {
		for _, r := range m.gateway.Routes {
			if r.Name == s.Name {
				kv("address", r.URL)
			}
		}
	}
	var access []string
	if s.Hidden {
		access = append(access, "hidden from the catalog")
	}
	if len(s.Allow) > 0 {
		access = append(access, "only "+strings.Join(s.Allow, ", "))
	}
	if len(s.PublicPaths) > 0 {
		access = append(access, "public: "+strings.Join(s.PublicPaths, " "))
	}
	kv("access", strings.Join(access, "; "))
}

// usersPane is the users view: everyone, then the selected user's sign-ins.
func (m Model) usersPane() string {
	var b strings.Builder
	if m.usersErr != nil {
		b.WriteString("  " + errorStyle.Render(m.usersErr.Error()) + "\n")
		return b.String()
	}
	if m.users == nil {
		b.WriteString("  " + labelStyle.Render("loading…") + "\n")
		return b.String()
	}
	if len(m.users) == 0 {
		b.WriteString("  " + labelStyle.Render("No users yet. Create the first admin with `outpost user add <name> --admin`.") + "\n")
		return b.String()
	}

	rows := [][]string{{"NAME", "ROLE", "GROUPS", "STATUS", "LAST SIGN-IN", "SESSIONS"}}
	for _, u := range m.users {
		role := "user"
		if u.Admin {
			role = "admin"
		}
		status := "active"
		switch {
		case u.Disabled:
			status = "disabled"
		case !u.HasPassword:
			status = "no password"
		}
		last := "never"
		if !u.LastLoginAt.IsZero() {
			last = u.LastLoginAt.Local().Format("2006-01-02 15:04")
		}
		groups := strings.Join(u.Groups, ",")
		if groups == "" {
			groups = "-"
		}
		rows = append(rows, []string{u.Username, role, groups, status, last, fmt.Sprintf("%d", u.Sessions)})
	}
	widths := cellWidths(rows)
	b.WriteString("    " + headerStyle.Render(joinCells(rows[0], widths)) + "\n")
	for i, row := range rows[1:] {
		marker, style := "  ", rowStyle
		if i == m.userCursor {
			marker, style = "▸ ", selectedRow
		}
		b.WriteString("  " + style.Render(marker+joinCells(row, widths)) + "\n")
	}

	user := m.selectedUser()
	b.WriteString("\n  " + m.rule("signed in: "+user) + "\n")
	if len(m.signIns) == 0 {
		b.WriteString("  " + labelStyle.Render("not signed in anywhere") + "\n")
		return b.String()
	}
	for _, si := range m.signIns {
		from := si.IP
		if from == "" {
			from = "-"
		}
		apps := strings.Join(si.Apps, ", ")
		if apps == "" {
			apps = "no apps opened"
		}
		line := fmt.Sprintf("%s  %s  %s  %s",
			pad(gateway.DescribeAgent(si.UserAgent), 22), pad(from, 15),
			si.CreatedAt.Local().Format("2006-01-02 15:04"), apps)
		b.WriteString("  " + valueStyle.Render(truncate(line, m.width-4)) + "\n")
	}
	return b.String()
}

// gatewayPane is the gateway view: where it is, what it routes, and what it
// has decided lately.
func (m Model) gatewayPane() string {
	var b strings.Builder
	if m.gateway == nil {
		msg := "The gateway is off. Turn it on with [gateway] enabled = true in config.toml, then restart the daemon."
		if m.gatewayErr != nil && !strings.Contains(m.gatewayErr.Error(), "gateway is off") {
			msg = m.gatewayErr.Error()
		}
		b.WriteString("  " + labelStyle.Render(msg) + "\n")
		return b.String()
	}
	st := m.gateway
	tls := "terminated in front"
	if st.TLS {
		tls = "served directly"
	}
	kv := func(k, v string) {
		b.WriteString("  " + labelStyle.Render(pad(k, 10)) + valueStyle.Render(v) + "\n")
	}
	kv("portal", st.PublicURL)
	kv("listen", st.Listen+" (TLS "+tls+")")
	if st.Tailnet {
		kv("apps", "their own tailnet address, else <name>."+st.Domain)
	} else {
		kv("apps", "<name>."+st.Domain)
	}

	b.WriteString("\n")
	if len(st.Routes) == 0 {
		b.WriteString("  " + labelStyle.Render("No services routed yet.") + "\n")
	} else {
		rows := [][]string{{"NAME", "ADDRESS", "STATUS"}}
		for _, r := range st.Routes {
			rows = append(rows, []string{r.Name, r.URL, r.Status})
		}
		widths := cellWidths(rows)
		b.WriteString("    " + headerStyle.Render(joinCells(rows[0], widths)) + "\n")
		for _, row := range rows[1:] {
			b.WriteString("    " + rowStyle.Render(joinCells(row, widths)) + "\n")
		}
	}

	if len(m.gatewayEvents) > 0 {
		b.WriteString("\n  " + sectionStyle.Render("recent activity") + "\n")
		for _, e := range m.gatewayEvents {
			line := fmt.Sprintf("%s  %s  %s", e.At.Format("15:04:05"), pad(e.Kind, 12), e.Message)
			b.WriteString("  " + valueStyle.Render(truncate(line, m.width-4)) + "\n")
		}
	}
	return b.String()
}

// cellWidths sizes columns to their contents in display cells.
func cellWidths(rows [][]string) []int {
	var w []int
	for _, row := range rows {
		for i, c := range row {
			if i >= len(w) {
				w = append(w, 0)
			}
			w[i] = max(w[i], lipgloss.Width(c))
		}
	}
	return w
}

func joinCells(row []string, widths []int) string {
	cells := make([]string, len(row))
	for i, c := range row {
		cells[i] = pad(c, widths[i])
	}
	return strings.TrimRight(strings.Join(cells, "  "), " ")
}

// helpLine is the key help for the view on screen. The services view keeps
// its generated bar, adding the key that leaves it; the others say only
// what works there, since a start or stop would mean nothing.
func (m Model) helpLine() string {
	if _, ok := m.extras(); !ok {
		return m.help.View(m.keys)
	}
	key := func(k, what string) string {
		return m.help.Styles.ShortKey.Render(k) + " " + m.help.Styles.ShortDesc.Render(what)
	}
	sep := m.help.Styles.ShortSeparator.Render(" • ")
	switch m.view {
	case viewUsers:
		return strings.Join([]string{key("↑/k", "up"), key("↓/j", "down"),
			key("L", "sign out everywhere"), key("tab", "next view"), key("R", "refresh"), key("q", "quit")}, sep)
	case viewGateway:
		return strings.Join([]string{key("tab", "next view"), key("R", "refresh"), key("q", "quit")}, sep)
	}
	// The generated bar gives up items to leave room for this one, rather
	// than the line running past the terminal.
	more := sep + key("tab", "views")
	h := m.help
	h.Width = m.width - 4 - lipgloss.Width(more)
	return h.View(m.keys) + more
}
