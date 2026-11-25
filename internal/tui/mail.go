package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/supervisor"
)

// MailOps is the mail apps get through the Gmail account. *ipc.Client
// satisfies it; a client without it has no Mail view.
type MailOps interface {
	MailStatus() (ipc.MailOverview, error)
	MailAccount(address, password string) (string, error)
	MailCheck() (string, error)
	MailAliasAdd(service, note string) (string, error)
	MailAliasRemove(service, route string) (string, error)
	MailSend(service string, allowed bool) (string, error)
	MailRegenerate(service string) (string, error)
}

// ExposureOps sets what of a service skips the login and what the internet
// reaches, over its manifest. What it is now comes with the service list.
type ExposureOps interface {
	ExposureSet(service string, funnel bool, paths []string) (string, error)
	ExposureReset(service string) (string, error)
	ExposureMCP(service string, on bool) (string, error)
	ExposureMCPReset(service string) (string, error)
}

func (m Model) mailOps() (MailOps, bool)         { x, ok := m.client.(MailOps); return x, ok }
func (m Model) exposureOps() (ExposureOps, bool) { x, ok := m.client.(ExposureOps); return x, ok }

func hasMailOps(m Model) bool     { _, ok := m.mailOps(); return ok }
func hasExposureOps(m Model) bool { _, ok := m.exposureOps(); return ok }

// mailState is the Mail view's.
type mailState struct {
	overview *ipc.MailOverview
	err      error
	cursor   int // over the apps
}

type mailMsg struct {
	overview ipc.MailOverview
	err      error
}

func (m Model) fetchMail() tea.Cmd {
	x, ok := m.mailOps()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		o, err := x.MailStatus()
		return mailMsg{overview: o, err: err}
	}
}

func (m Model) updateMail(msg mailMsg) Model {
	m.ml.err = msg.err
	if msg.err == nil {
		o := msg.overview
		m.ml.overview = &o
	}
	if m.ml.overview != nil {
		m.ml.cursor = max(min(m.ml.cursor, len(m.ml.overview.Apps)-1), 0)
	}
	return m
}

// selectedMailApp is the app the Mail view's cursor is on.
func (m Model) selectedMailApp() (ipc.MailApp, bool) {
	if m.view != viewMail || m.ml.overview == nil || m.ml.cursor >= len(m.ml.overview.Apps) {
		return ipc.MailApp{}, false
	}
	return m.ml.overview.Apps[m.ml.cursor], true
}

// mailService is the app a mail action starts on: the one picked in the Mail
// view, else the service selected in the list.
func mailService(m Model) vals {
	if a, ok := m.selectedMailApp(); ok {
		return vals{"service": a.Name}
	}
	return selectedService(m)
}

func (m Model) handleMailKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	n := 0
	if m.ml.overview != nil {
		n = len(m.ml.overview.Apps)
	}
	id := ""
	switch msg.String() {
	case "up", "k":
		m.ml.cursor = max(m.ml.cursor-1, 0)
		return m, nil, true
	case "down", "j":
		m.ml.cursor = max(min(m.ml.cursor+1, n-1), 0)
		return m, nil, true
	case "a":
		id = "mail-account"
	case "c":
		id = "mail-check"
	case "n":
		id = "mail-alias-add"
	case "d":
		id = "mail-alias-rm"
	case "s":
		id = "mail-send"
	case "t":
		id = "mail-token"
	default:
		return m, nil, false
	}
	next, cmd := m.openAction(id)
	return next, cmd, true
}

// mailAccountPane is the account: where mail comes from and how reading it
// is going.
func (m Model) mailAccountPane() []string {
	switch {
	case m.ml.err != nil:
		return []string{errorStyle.Render(m.ml.err.Error())}
	case m.ml.overview == nil:
		return []string{labelStyle.Render("loading…")}
	}
	st := m.ml.overview.Account
	kv := func(k, v string) string { return labelStyle.Render(pad(k, 10)) + v }
	var out []string
	if !st.Ready() {
		return append(out, kv("account", labelStyle.Render("not set up; press a to set it")))
	}
	out = append(out, kv("account", valueStyle.Render(st.Address)))
	inbox := ""
	switch {
	case st.Error != "":
		inbox = errorStyle.Render("can't be read: " + st.Error)
	case st.Checked.IsZero():
		inbox = noticeStyle.Render("checking")
	default:
		inbox = statusStyle("running").Render("checked "+shortDuration(time.Since(st.Checked))+" ago") +
			labelStyle.Render(fmt.Sprintf(" · %d routed since the daemon started", st.Routed))
	}
	out = append(out, kv("inbox", inbox))
	if s := m.ml.overview.Secret; s != "" {
		out = append(out, kv("password", labelStyle.Render("kept as the secret "+s+"; never shown")))
	}
	return out
}

// mailAppsPane is every app's addresses, whether it sends, and what waits.
func (m Model) mailAppsPane() string {
	if m.ml.overview == nil || len(m.ml.overview.Apps) == 0 {
		return "  " + labelStyle.Render("No apps.") + "\n"
	}
	rows := [][]string{{"APP", "ADDRESSES", "SENDS", "WAITING"}}
	for _, a := range m.ml.overview.Apps {
		var addrs []string
		for _, r := range a.Routes {
			s := r.Address
			if s == "" {
				s = "+" + r.Route
			}
			if r.Note != "" {
				s += " (" + r.Note + ")"
			}
			addrs = append(addrs, s)
		}
		sends := "no"
		if a.Send {
			sends = "yes"
		}
		waiting := "-"
		if a.Held > 0 {
			waiting = fmt.Sprintf("%d, newest %s ago", a.Held, shortDuration(time.Since(a.HeldNewest)))
		}
		rows = append(rows, []string{a.Name, orDashTUI(strings.Join(addrs, ", ")), sends, waiting})
	}
	return m.grid(rows, m.ml.cursor)
}

func orDashTUI(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// mailBody is the account above the apps.
func (m Model) mailBody(w, h int) []string {
	acct := m.mailAccountPane()
	acctH := min(len(acct)+2, h)
	out := box("mail account", acct, w, acctH, false, "")
	if h-acctH < 4 {
		return out
	}
	senders := 0
	if m.ml.overview != nil {
		for _, a := range m.ml.overview.Apps {
			if a.Send {
				senders++
			}
		}
	}
	note := ""
	if senders > 0 {
		note = countNote(senders, "app") + " may send"
	}
	return append(out, box("apps", paneLines(m.narrowed(w).mailAppsPane()), w, h-acctH, true, note)...)
}

func (m Model) mailHelp(key func(k, what string) string, sep string) string {
	return strings.Join([]string{key("↑/↓", "app"), key("a", "account"), key("c", "check"), key("n", "new address"),
		key("d", "remove address"), key("s", "sending"), key("t", "token"), key(":", "commands"), key("q", "quit")}, sep)
}

func mailActions() []action {
	svcField := text("service", "service", "name")
	return []action{
		{id: "mail", group: "Mail", title: "mail", about: "the account, and each app's addresses and sending",
			needs: hasMailOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				return m.enterView(viewMail)
			}, covers: []string{"ipc:MailStatus", "cli:mail"}},
		{id: "mail-account", group: "Mail", title: "set mail account", about: "the Gmail address and an app password, tried before it's saved",
			fields: []formField{text("address", "address", "you@gmail.com"), secretField("password", "app password", false)},
			prefill: func(m Model) vals {
				if m.ml.overview != nil {
					return vals{"address": m.ml.overview.Account.Address}
				}
				return nil
			}, needs: hasMailOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return busy(m, "asking Gmail", do("set mail account", v["address"], func() (string, error) {
					return x.MailAccount(v["address"], v["password"])
				}))
			}, covers: []string{"ipc:MailAccount", "cli:mail account"}},
		{id: "mail-check", group: "Mail", title: "check mail", about: "read the inbox now",
			needs: hasMailOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return busy(m, "checking the inbox", do("check mail", "", x.MailCheck))
			}, covers: []string{"ipc:MailCheck", "cli:mail check"}},
		{id: "mail-alias-add", group: "Mail", title: "new mail address", about: "give an app another address, with a note on what it's for",
			fields:  []formField{svcField, optional("note", "note", "what it's for")},
			prefill: mailService, needs: hasMailOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return m, do("new address", v["service"], func() (string, error) { return x.MailAliasAdd(v["service"], v["note"]) })
			}, covers: []string{"ipc:MailAliasAdd", "cli:mail alias"}},
		{id: "mail-alias-rm", group: "Mail", title: "remove mail address", about: "mail to it stays in the inbox from then on",
			fields: []formField{svcField, text("route", "address", "+route")},
			prefill: func(m Model) vals {
				v := mailService(m)
				if a, ok := m.selectedMailApp(); ok {
					// The newest alias: the app's own name can't be removed.
					for _, r := range a.Routes {
						if r.Alias {
							v["route"] = "+" + r.Route
						}
					}
				}
				return v
			}, needs: hasMailOps,
			confirm: func(v vals) string {
				return "Remove " + v["route"] + " from " + v["service"] + "? Mail to it stays in the inbox from then on."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return m, do("remove address", v["service"], func() (string, error) { return x.MailAliasRemove(v["service"], v["route"]) })
			}, covers: []string{"ipc:MailAliasRemove"}},
		{id: "mail-send", group: "Mail", title: "mail sending on/off", about: "whether an app may send as the account, up to 100 a day",
			fields: []formField{svcField, choice("send", "may send", "on", "off")},
			prefill: func(m Model) vals {
				v := mailService(m)
				v["send"] = "on"
				if a, ok := m.selectedMailApp(); ok && a.Send {
					v["send"] = "off" // the likely change is the other way
				}
				return v
			}, needs: hasMailOps,
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return m, do("mail sending", v["service"], func() (string, error) { return x.MailSend(v["service"], v["send"] == "on") })
			}, covers: []string{"ipc:MailSend", "cli:mail send"}},
		{id: "mail-token", group: "Mail", title: "new mail token", about: "give an app a new mail token; it restarts",
			fields: []formField{svcField}, prefill: mailService, needs: hasMailOps,
			confirm: func(v vals) string {
				return "Give " + v["service"] + " a new mail token? It restarts, and the old one stops working."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.mailOps()
				return m, do("mail token", v["service"], func() (string, error) { return x.MailRegenerate(v["service"]) })
			}, covers: []string{"ipc:MailRegenerate", "cli:mail token"}},
	}
}

func exposureActions() []action {
	svcField := text("service", "service", "name")
	selectedExposure := func(m Model) vals {
		v := selectedService(m)
		v["funnel"] = "off"
		v["mcp"] = "on"
		if s, ok := m.selected(); ok {
			v["paths"] = strings.Join(s.PublicPaths, " ")
			if s.Funnel {
				v["funnel"] = "on"
			}
			if s.MCPFunnel {
				v["mcp"] = "off"
			}
		}
		return v
	}
	return []action{
		{id: "expose", group: "Internet", title: "public paths and Funnel", about: "the paths that skip the login, and whether the internet reaches them",
			fields:  []formField{svcField, optional("paths", "public paths", "none; space separated, such as /public/*"), choice("funnel", "funnel", "off", "on")},
			prefill: selectedExposure, needs: hasExposureOps,
			confirm: func(v vals) string {
				if v["funnel"] == "on" && v["paths"] != "" {
					return "Put " + v["paths"] + " of " + v["service"] + " on the internet, with no login in front?"
				}
				paths := v["paths"]
				if paths == "" {
					paths = "nothing"
				}
				return "Let " + paths + " of " + v["service"] + " skip the login, on the tailnet only?"
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.exposureOps()
				return busy(m, "applying", do("expose", v["service"], func() (string, error) {
					return x.ExposureSet(v["service"], v["funnel"] == "on", list(v["paths"]))
				}))
			}, covers: []string{"ipc:ExposureSet", "cli:expose"}},
		{id: "expose-reset", group: "Internet", title: "public paths as the manifest says", about: "drop what was set over the manifest's public paths and Funnel",
			fields: []formField{svcField}, prefill: selectedService, needs: hasExposureOps,
			confirm: func(v vals) string {
				return "Go back to " + v["service"] + "'s manifest for its public paths and Funnel?"
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.exposureOps()
				return busy(m, "applying", do("expose", v["service"], func() (string, error) { return x.ExposureReset(v["service"]) }))
			}, covers: []string{"ipc:ExposureReset"}},
		{id: "expose-mcp", group: "Internet", title: "MCP on the internet on/off", about: "whether the internet reaches an app's MCP endpoint, for internet access tokens",
			fields: []formField{svcField, choice("mcp", "on the internet", "on", "off")}, prefill: selectedExposure, needs: hasExposureOps,
			confirm: func(v vals) string {
				if v["mcp"] == "on" {
					return "Put " + v["service"] + "'s MCP endpoint on the internet? Only access tokens made for the internet and this app get in."
				}
				return "Take " + v["service"] + "'s MCP endpoint off the internet? Clients using it from outside the tailnet stop working."
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.exposureOps()
				return busy(m, "applying", do("mcp", v["service"], func() (string, error) { return x.ExposureMCP(v["service"], v["mcp"] == "on") }))
			}, covers: []string{"ipc:ExposureMCP"}},
		{id: "expose-mcp-reset", group: "Internet", title: "MCP as the manifest says", about: "drop what was set over the manifest's mcp_funnel",
			fields: []formField{svcField}, prefill: selectedService, needs: hasExposureOps,
			confirm: func(v vals) string {
				return "Go back to " + v["service"] + "'s manifest for its MCP endpoint on the internet?"
			},
			run: func(m Model, v vals) (Model, tea.Cmd) {
				x, _ := m.exposureOps()
				return busy(m, "applying", do("mcp", v["service"], func() (string, error) { return x.ExposureMCPReset(v["service"]) }))
			}, covers: []string{"ipc:ExposureMCPReset"}},
	}
}

// reachWords is what of s the internet reaches, for the detail pane: ""
// when it's only the tailnet.
func reachWords(s supervisor.ServiceStatus) string {
	var parts []string
	if s.Funnel {
		state := "starting"
		if t := s.Tailnet; t != nil && t.Funnel != "" {
			state = t.Funnel
			if t.FunnelError != "" {
				state += ": " + t.FunnelError
			}
		}
		parts = append(parts, "funnel "+state)
	}
	if s.MCPFunnel && s.MCPPath != "" {
		parts = append(parts, "mcp "+s.MCPPath)
	}
	if len(parts) > 0 && (s.FromPortal || s.MCPFromPortal) {
		parts = append(parts, "set over the manifest")
	}
	return strings.Join(parts, " · ")
}
