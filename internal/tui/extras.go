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
	viewAI
	viewOverview
	viewSecrets
	viewTokens
	viewLogs
	viewMail
)

var viewNames = []string{"Services", "Users", "Gateway", "AI", "Overview", "Secrets", "Tokens", "Logs", "Mail"}

// views are the views this client can show, in tab order. Each beyond the
// first three needs the part of the API it draws on, so a client without it
// is never shown a page that can only fail: the AI view needs AIExtras, the
// overview usage stats, and so on. The logs view needs a LogReader.
func (m Model) views() []view {
	var v []view
	if _, ok := m.statsClient(); ok {
		v = append(v, viewOverview)
	}
	v = append(v, viewServices, viewUsers, viewGateway)
	if _, ok := m.aiClient(); ok {
		v = append(v, viewAI)
	}
	if _, ok := m.secretOps(); ok {
		v = append(v, viewSecrets)
	}
	if _, ok := m.tokenOps(); ok {
		v = append(v, viewTokens)
	}
	if m.logs != nil {
		v = append(v, viewLogs)
	}
	if _, ok := m.mailOps(); ok {
		v = append(v, viewMail)
	}
	return v
}

// enterView switches to v and fetches what it shows.
func (m Model) enterView(v view) (Model, tea.Cmd) {
	m.view = v
	m.confirm, m.notice = "", ""
	switch v {
	case viewAI:
		m.ai.mode = aiBrowse
		return m, tea.Batch(m.refreshExtras(), m.fetchAI())
	case viewLogs:
		if m.lv.service == "" && !m.lv.all {
			m.lv.service = m.selectedName()
		}
		var cmd tea.Cmd
		m, cmd = m.startLogTicks()
		return m, tea.Batch(m.refreshExtras(), m.fetchLogView(), cmd)
	}
	var usage tea.Cmd
	if m.usageShown() {
		usage = m.fetchUsage()
	}
	return m, tea.Batch(m.refreshExtras(), m.enterCmd(), usage)
}

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
	err     error
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
	// Only the Gateway view shows the feed; the others need the addresses.
	withEvents := m.view == viewGateway
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
			if withEvents {
				msg.events, _ = client.Events(gateway.ActivityService, eventLimit)
			}
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
		return signInsMsg{user: user, signIns: list, err: err}
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
		// The sign-ins follow once the list arrives (updateExtras), for
		// whoever is selected then.
		cmds = append(cmds, m.fetchUsers())
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
			m.signIns, m.signInsUser, m.signInsErr = msg.signIns, msg.user, msg.err
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
		vs := m.views()
		at := 0
		for i, v := range vs {
			if v == m.view {
				at = i
			}
		}
		step := 1
		if msg.String() == "shift+tab" {
			step = len(vs) - 1
		}
		next, cmd := m.enterView(vs[(at+step)%len(vs)])
		return next, cmd, true
	}
	if m.view == viewServices {
		return m, nil, false
	}
	// Anything but a second L calls off a pending sign-out.
	pending := m.confirm
	m.confirm = ""
	if pending != "" && msg.String() != "L" {
		// Don't leave "press L again" up once it no longer holds.
		m.notice = ""
	}
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
		return m, tea.Batch(m.fetchServices(), m.refreshExtras(), m.refreshMore(), m.fetchUsage()), true
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

// tabs is the line naming the views, the current one marked. With more
// views than fit, it tightens up before it gives anything away, and as a
// last resort numbers the others, which the digit keys jump to.
func (m Model) tabs() string {
	vs := m.views()
	build := func(sep, hint string, numbered bool) string {
		parts := make([]string, 0, len(vs))
		for i, v := range vs {
			name := viewNames[v]
			switch {
			case v == m.view:
				parts = append(parts, selectedRow.Render("▸ "+name))
			case numbered:
				parts = append(parts, labelStyle.Render(fmt.Sprintf("%d", i+1)))
			default:
				parts = append(parts, labelStyle.Render("  "+name))
			}
		}
		return "  " + strings.Join(parts, sep) + hint
	}
	for _, line := range []string{
		build("  ", "  "+labelStyle.Render("(tab)"), false),
		build(" ", "", false),
	} {
		if lipgloss.Width(line) <= m.width {
			return line
		}
	}
	return build(" ", "  "+labelStyle.Render("(tab, 1-9)"), true)
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
	kv("internet", reachWords(s))
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
		b.WriteString("  " + style.Render(truncate(marker+joinCells(row, widths), m.width-4)) + "\n")
	}

	return b.String()
}

// signInsPane is where the selected user is signed in.
func (m Model) signInsPane() string {
	var b strings.Builder
	user := m.selectedUser()
	if user == "" {
		return ""
	}
	switch {
	case m.signInsUser != user:
		b.WriteString("  " + labelStyle.Render("loading…") + "\n")
		return b.String()
	case m.signInsErr != nil:
		// Saying "not signed in" when the daemon couldn't be asked would
		// hide sessions that are live.
		b.WriteString("  " + errorStyle.Render(m.signInsErr.Error()) + "\n")
		return b.String()
	}
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

	return b.String()
}

// routesPane is the apps the gateway routes and where.
func (m Model) routesPane() string {
	st := m.gateway
	if st == nil {
		return ""
	}
	var b strings.Builder
	if len(st.Routes) == 0 {
		b.WriteString("  " + labelStyle.Render("No services routed yet.") + "\n")
		return b.String()
	}
	rows := [][]string{{"NAME", "ADDRESS", "STATUS"}}
	for _, r := range st.Routes {
		rows = append(rows, []string{r.Name, r.URL, r.Status})
	}
	widths := cellWidths(rows)
	b.WriteString("  " + headerStyle.Render(truncate(joinCells(rows[0], widths), m.width-4)) + "\n")
	for _, row := range rows[1:] {
		b.WriteString("  " + rowStyle.Render(truncate(joinCells(row, widths), m.width-4)) + "\n")
	}
	return b.String()
}

// gatewayActivityPane is what the gateway has decided lately.
func (m Model) gatewayActivityPane() string {
	var b strings.Builder
	if len(m.gatewayEvents) == 0 {
		b.WriteString("  " + labelStyle.Render("nothing yet") + "\n")
		return b.String()
	}
	for _, e := range m.gatewayEvents {
		line := fmt.Sprintf("%s  %s  %s", e.At.Format("15:04:05"), pad(e.Kind, 12), e.Message)
		b.WriteString("  " + valueStyle.Render(truncate(line, m.width-4)) + "\n")
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

// fitHelp cuts a hand-written help line to the terminal, at a separator
// where it can, rather than letting it wrap.
func (m Model) fitHelp(h string) string {
	limit := m.width - 4
	if lipgloss.Width(h) <= limit {
		return h
	}
	sep := m.help.Styles.ShortSeparator.Render(" • ")
	parts := strings.Split(h, sep)
	for len(parts) > 1 && lipgloss.Width(strings.Join(parts, sep)+" …") > limit {
		parts = parts[:len(parts)-1]
	}
	out := strings.Join(parts, sep)
	if lipgloss.Width(out) > limit {
		return lipgloss.NewStyle().MaxWidth(max(limit, 1)).Render(out)
	}
	return out + " …"
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
	if h := m.overlayHelp(key, sep); h != "" {
		return h
	}
	if h := m.moreHelp(key, sep); h != "" {
		return m.fitHelp(h)
	}
	switch m.view {
	case viewUsers:
		if _, ok := m.userOps(); ok {
			return m.fitHelp(strings.Join([]string{key("↑/↓", "move"), key("a", "add"), key("p", "password"), key("g", "groups"),
				key("A", "admin"), key("d", "disable"), key("L", "sign out"), key(":", "commands"), key("q", "quit")}, sep))
		}
		return strings.Join([]string{key("↑/k", "up"), key("↓/j", "down"),
			key("L", "sign out everywhere"), key("tab", "next view"), key("R", "refresh"), key("q", "quit")}, sep)
	case viewGateway:
		return strings.Join([]string{key("tab", "next view"), key("R", "refresh"), key("q", "quit")}, sep)
	case viewAI:
		return m.aiHelp(key, sep)
	}
	// The generated bar gives up items to leave room for this one, rather
	// than the line running past the terminal.
	more := sep + key("tab", "views")
	if len(m.views()) > 3 {
		// The full dashboard: its extra keys go ahead of the generated ones,
		// which give way first when the line is short.
		var extra []string
		if m.logs != nil {
			extra = append(extra, key("L", "full log"))
		}
		if len(m.usage) > 0 {
			extra = append(extra, key("g", "24h/30m"))
		}
		extra = append(extra, key("z", "zoom"), key(":", "commands"))
		more = sep + strings.Join(extra, sep) + more
		if m.width-4-lipgloss.Width(more) < 30 {
			more = sep + key(":", "commands") + sep + key("tab", "views")
		}
	}
	h := m.help
	h.Width = m.width - 4 - lipgloss.Width(more)
	if len(m.views()) <= 3 {
		return h.View(m.keys) + more
	}
	// Trimmed here rather than by help's own Width: when the ellipsis just
	// misses fitting, that stops trimming and keeps every item after it.
	h.Width = 0
	limit := m.width - 4 - lipgloss.Width(more)
	parts := strings.Split(h.View(m.keys), sep)
	cut := false
	for len(parts) > 1 && lipgloss.Width(strings.Join(parts, sep))+2 > limit {
		parts, cut = parts[:len(parts)-1], true
	}
	out := strings.Join(parts, sep)
	if cut {
		out += " …"
	}
	return out + more
}
