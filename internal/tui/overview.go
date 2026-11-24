package tui

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/supervisor"
)

// overviewState is the overview's.
type overviewState struct {
	cursor int // which tile Enter opens
	// backups is each service's newest backup; a service with none is
	// absent. Read on opening the overview and now and then after, since
	// it lists a directory per service.
	backups  map[string]time.Time
	backupAt time.Time
	// backupNames is the services the last complete read covered; a list
	// that has changed since (the first read can run before the list has
	// loaded) is read again without waiting out backupsEvery.
	backupNames string
	// scanning is set while a read is out, so ticks don't pile more on; a
	// pointer, so every copy of the model sees the one flag.
	scanning *atomic.Bool
	activity []supervisor.EventRecord
	signIns  []supervisor.EventRecord
}

// backupsEvery is how often the overview re-reads the backups.
const backupsEvery = 5 * time.Minute

type overviewMsg struct {
	activity []supervisor.EventRecord
	signIns  []supervisor.EventRecord
	backups  map[string]time.Time // nil: not read this time
	names    string               // the services read, joined
	partial  bool                 // some reads failed; backups has the rest
}

// fetchOverview reads the activity feeds, and the backups if it's time.
func (m Model) fetchOverview() tea.Cmd {
	client := m.client
	ops, hasOps := m.serviceOps()
	_, hasGateway := m.extras()
	names := make([]string, 0, len(m.services))
	for _, s := range m.services {
		if s.Status != supervisor.StatusRemoved && s.Storage != "" {
			names = append(names, s.Name)
		}
	}
	joined := strings.Join(names, "\x00")
	scanning := m.ov.scanning
	readBackups := hasOps && m.services != nil && scanning != nil &&
		(joined != m.ov.backupNames || time.Since(m.ov.backupAt) > backupsEvery)
	return func() tea.Msg {
		var msg overviewMsg
		// Decoration, as the detail pane's feed is: a failure leaves the
		// last one up.
		msg.activity, _ = client.Events("", 8)
		if hasGateway {
			msg.signIns, _ = client.Events(gateway.ActivityService, 5)
		}
		// Claimed here, when the read runs, so a command never run can't
		// hold it.
		if readBackups && scanning.CompareAndSwap(false, true) {
			defer scanning.Store(false)
			msg.backups, msg.names = map[string]time.Time{}, joined
			for _, name := range names {
				list, err := ops.Backups(name)
				if err != nil {
					msg.partial = true
					continue
				}
				if len(list) == 0 {
					continue
				}
				newest := list[0].CreatedAt
				for _, a := range list {
					if a.CreatedAt.After(newest) {
						newest = a.CreatedAt
					}
				}
				msg.backups[name] = newest
			}
		}
		return msg
	}
}

func (m Model) updateOverview(msg overviewMsg) Model {
	if msg.activity != nil {
		m.ov.activity = msg.activity
	}
	if msg.signIns != nil {
		m.ov.signIns = msg.signIns
	}
	switch {
	case msg.backups == nil:
	case msg.partial:
		// Keep what the failed reads last found rather than show "never"
		// for a service whose backups just couldn't be listed; with the
		// time left alone, the next tick reads again.
		merged := make(map[string]time.Time, len(m.ov.backups)+len(msg.backups))
		for k, v := range m.ov.backups {
			merged[k] = v
		}
		for k, v := range msg.backups {
			merged[k] = v
		}
		m.ov.backups = merged
	default:
		m.ov.backups, m.ov.backupAt, m.ov.backupNames = msg.backups, time.Now(), msg.names
	}
	return m
}

// tile is one box of the overview.
type tile struct {
	title string
	lines []string
	to    view // where Enter goes
}

func (m Model) tiles() []tile {
	var ts []tile

	// Health: the count of each state, then a row per service.
	counts := map[string]int{}
	for _, s := range m.services {
		counts[s.Status]++
	}
	var summary []string
	for _, st := range []string{string(runtime.StatusRunning), string(runtime.StatusStarting), string(runtime.StatusUnhealthy),
		string(runtime.StatusFailed), string(runtime.StatusStopped), supervisor.StatusRemoved} {
		if n := counts[st]; n > 0 {
			summary = append(summary, statusStyle(st).Render(fmt.Sprintf("%s %d %s", statusMark(st), n, st)))
		}
	}
	health := []string{strings.Join(summary, "  ")}
	if len(m.services) == 0 {
		health = []string{labelStyle.Render("No services declared.")}
	}
	nameW := 0
	for _, s := range m.services {
		nameW = max(nameW, lipgloss.Width(s.Name))
	}
	nameW = min(nameW, 20)
	for _, s := range m.services {
		cpu, mem := m.usageCells(s.Name, 12)
		line := statusStyle(s.Status).Render(statusMark(s.Status)) + " " + pad(truncate(s.Name, nameW), nameW) + "  " +
			labelStyle.Render(pad(uptime(s), 4)) + "  " + pad(cpu, 18) + "  " + labelStyle.Render(mem)
		health = append(health, line)
	}
	ts = append(ts, tile{title: "services", lines: health, to: viewServices})

	// Attention: what wants doing.
	var attention []string
	for _, s := range m.services {
		switch {
		case s.Halted:
			attention = append(attention, errorStyle.Render("✖ "+s.Name+" is halted; start it once it's fixed"))
		case s.Tailnet != nil && s.Tailnet.LoginURL != "":
			attention = append(attention, noticeStyle.Render("▲ "+s.Name+" needs a tailnet login"))
		case s.Status == string(runtime.StatusUnhealthy):
			attention = append(attention, noticeStyle.Render("▲ "+s.Name+" is failing its healthcheck"))
		}
	}
	if m.configStale {
		attention = append(attention, errorStyle.Render("config.toml changed; restart the daemon to apply it"))
	}
	var updates []string
	for _, s := range m.services {
		if s.UpdateAvailable {
			updates = append(updates, s.Name+" "+s.LatestRelease)
		}
	}
	if len(updates) > 0 {
		attention = append(attention, valueStyle.Render("↑ updates: "+strings.Join(updates, ", ")))
	}
	if len(attention) == 0 {
		attention = []string{statusStyle(string(runtime.StatusRunning)).Render("● nothing needs you")}
	}
	ts = append(ts, tile{title: "attention", lines: attention, to: viewServices})

	// Reach: the tailnet and the internet.
	var reach []string
	on, starting, login := 0, 0, 0
	var internet []string
	for _, s := range m.services {
		if t := s.Tailnet; t != nil {
			switch t.State {
			case supervisor.TailnetOn:
				on++
			case supervisor.TailnetStarting:
				starting++
			case supervisor.TailnetNeedsLogin:
				login++
			}
		}
		if s.Funnel && len(s.PublicPaths) > 0 {
			internet = append(internet, s.Name+" "+strings.Join(s.PublicPaths, " "))
		}
		if s.MCPFunnel && s.MCPPath != "" {
			internet = append(internet, s.Name+" "+s.MCPPath+" (mcp)")
		}
	}
	if on+starting+login > 0 {
		reach = append(reach, fmt.Sprintf("tailnet  %d on · %d starting · %d need login", on, starting, login))
	}
	if g := m.gateway; g != nil {
		reach = append(reach, "portal   "+g.PublicURL)
		reach = append(reach, fmt.Sprintf("routes   %d app(s)", len(g.Routes)))
	} else {
		reach = append(reach, labelStyle.Render("gateway off"))
	}
	if len(internet) == 0 {
		reach = append(reach, labelStyle.Render("nothing answers from the internet"))
	} else {
		for _, s := range internet {
			reach = append(reach, "internet "+s)
		}
	}
	ts = append(ts, tile{title: "reach", lines: reach, to: viewGateway})

	// Backups: how old each service's newest is.
	if _, ok := m.serviceOps(); ok {
		var lines []string
		var names []string
		for _, s := range m.services {
			if s.Status != supervisor.StatusRemoved && s.Storage != "" {
				names = append(names, s.Name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			at, ok := m.ov.backups[name]
			age := errorStyle.Render("never")
			if ok {
				age = shortDuration(time.Since(at)) + " ago"
				if time.Since(at) > 7*24*time.Hour {
					age = noticeStyle.Render(age)
				}
			}
			lines = append(lines, pad(truncate(name, nameW), nameW)+"  "+age)
		}
		if len(lines) == 0 {
			lines = []string{labelStyle.Render("No service has storage to back up.")}
		} else if m.ov.backupAt.IsZero() && len(m.ov.backups) == 0 {
			lines = []string{labelStyle.Render("reading…")}
		}
		ts = append(ts, tile{title: "backups", lines: lines, to: viewServices})
	}

	// Activity, everywhere, and the gateway's sign-ins.
	feed := func(events []supervisor.EventRecord, withService bool) []string {
		var out []string
		for _, e := range events {
			who := ""
			if withService {
				who = pad(truncate(e.Service, 12), 12) + " "
			}
			out = append(out, labelStyle.Render(e.At.Local().Format("15:04"))+" "+who+e.Message)
		}
		return out
	}
	act := feed(m.ov.activity, true)
	if len(act) == 0 {
		act = []string{labelStyle.Render("nothing yet")}
	}
	activity := tile{title: "activity", lines: act, to: viewServices}
	if m.logs != nil {
		activity.to = viewLogs
	}
	ts = append(ts, activity)
	if _, ok := m.extras(); ok && m.gateway != nil {
		si := feed(m.ov.signIns, false)
		if len(si) == 0 {
			si = []string{labelStyle.Render("no sign-ins yet")}
		}
		ts = append(ts, tile{title: "sign-ins", lines: si, to: viewUsers})
	}
	return ts
}

func (m Model) handleOverviewKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	n := len(m.tiles())
	switch msg.String() {
	case "up", "k", "left", "h":
		m.ov.cursor = max(m.ov.cursor-1, 0)
	case "down", "j", "right", "l":
		m.ov.cursor = min(m.ov.cursor+1, n-1)
	case "enter":
		ts := m.tiles()
		if m.ov.cursor < len(ts) {
			to := ts[m.ov.cursor].to
			if to == viewLogs {
				m.lv.all, m.lv.events = true, true
				m.lv.reset()
			}
			next, cmd := m.enterView(to)
			return next, cmd, true
		}
	case "s", "x", "r", "L":
		// Nothing is selected here to act on.
	default:
		return m, nil, false
	}
	return m, nil, true
}
