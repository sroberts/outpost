package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// minViewHeight is the shortest the dashboard can be: the title, a blank line,
// the column header, one service row, a blank line and the help bar. Below
// this there is nothing left to cut that would leave anything worth looking
// at, so a shorter terminal gets clipped by the renderer rather than served
// something unusable.
const minViewHeight = 7

// View renders the dashboard to fill the terminal exactly: the header, the
// body's panels, and the notice and key help, every row accounted for.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	top, foot := m.top(), m.footer()
	rows := max(m.height-lipgloss.Height(top)-lipgloss.Height(foot), 3)
	body := m.body(m.width, rows)
	return top + "\n" + strings.Join(body, "\n") + "\n" + foot
}

// bodyHeight is the rows inside a panel that takes the whole body: the
// terminal less the header, the footer and the panel's own border.
func (m Model) bodyHeight() int {
	return max(m.height-lipgloss.Height(m.top())-lipgloss.Height(m.footer()), 3) - 2
}

// body is every view's panels, exactly w by h.
func (m Model) body(w, h int) []string {
	full := func(title string, lines []string, note string) []string {
		return box(title, lines, w, h, false, note)
	}
	switch {
	case m.err != nil:
		return full("daemon", paneLines(m.errorPane()), "")
	case m.overlayUp():
		return m.overlayBody(w, h)
	case m.view == viewOverview:
		return m.overviewBody(w, h)
	case m.view == viewSecrets:
		return full("secrets", paneLines(m.secretsPane()), countNote(len(m.mg.secrets), "secret"))
	case m.view == viewTokens:
		return m.tokensBody(w, h)
	case m.view == viewLogs:
		return full(m.logViewTitle(), m.logViewLines(h-2), "")
	case m.view == viewUsers:
		return m.usersBody(w, h)
	case m.view == viewGateway:
		return m.gatewayBody(w, h)
	case m.view == viewAI:
		return full("ai", paneLines(m.aiPane()), "")
	case !m.loaded:
		return full("services", []string{labelStyle.Render("loading…")}, "")
	case len(m.services) == 0:
		return full("services", []string{labelStyle.Render("No services declared. Add a manifest under services/, then run `outpost apply`.")}, "")
	}
	return m.servicesBody(w, h)
}

// countNote is a panel's bottom note counting what it lists.
func countNote(n int, what string) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// servicesBody is the services view. From the top: usage across every
// service, when there's room and anything measured; the list and the
// selected service's detail, side by side on a wide terminal and stacked on
// a narrow one; and the log, when it's on, taking a share of what's left.
func (m Model) servicesBody(w, h int) []string {
	var out []string
	sysH := 0
	if len(m.usage) > 0 && h >= 30 && w >= 80 && !m.zoom {
		sysH = max(min(h/4, 12), 7)
		out = append(out, m.systemRow(w, sysH)...)
	}
	rest := h - sysH
	logsH := 0
	if m.showLogs && !m.zoom {
		logsH = max(rest*2/5, 6)
		if rest-logsH < 6 {
			logsH = 0 // no room for both; the list wins
		}
	}
	mainH := rest - logsH

	switch {
	case m.zoom:
		out = append(out, m.detailBox(w, mainH, true)...)
	case m.wide(w):
		natural := 0
		for _, line := range strings.Split(m.narrowed(w).tableRange(0, len(m.services)), "\n") {
			natural = max(natural, lipgloss.Width(line))
		}
		leftW := min(natural+2, w*3/5)
		out = append(out, beside(m.listBox(leftW, mainH), m.detailBox(w-leftW, mainH, false))...)
	default:
		// The list and the detail share the height by what each needs. On a
		// terminal too short for a useful detail, the list takes it all.
		detailNeed := len(paneLines(m.narrowed(w).detail())) + 2
		if g := m.graphLines(m.selectedName(), boxInner(w), 1); len(g) > 0 {
			detailNeed += len(g) + 1
		}
		listH := allot(mainH, 5, len(m.services)+3, detailNeed)[0]
		if mainH-listH < 6 {
			listH = mainH
		}
		out = append(out, m.listBox(w, listH)...)
		if mainH > listH {
			out = append(out, m.detailBox(w, mainH-listH, false)...)
		}
	}
	if logsH > 0 {
		out = append(out, m.logBox(w, logsH)...)
	}
	return out
}

// wide is a body with room for the list and the detail side by side.
func (m Model) wide(w int) bool { return w >= 110 }

// listBox is the service list, windowed on the selection.
func (m Model) listBox(w, h int) []string {
	n := m.narrowed(w)
	n.noteInBorder = true
	start, end := n.window(max(h-3, 1))
	note := n.rangeNote(start, end)
	if note == "" {
		note = countNote(len(m.services), "service")
	}
	title := "services"
	if m.watching {
		title += " · live"
	}
	return box(title, paneLines(n.tableRange(start, end)), w, h, !m.zoom, note)
}

// detailBox is the selected service: its facts, its recent activity, and
// its usage drawn as tall as the panel leaves room for.
func (m Model) detailBox(w, h int, zoomed bool) []string {
	s, ok := m.selected()
	if !ok {
		return box("", nil, w, h, false, "")
	}
	n := m.narrowed(w)
	inner := boxInner(w)
	lines := paneLines(n.detail())
	spare := h - 2 - len(lines) - 1
	limit := 6
	if zoomed {
		limit = 16
	}
	extra := 0
	if u, ok := m.usageOf(s.Name); ok {
		extra = 2 // the heading and the gap
		if ceiling(m.series(u.RecentIn, u.InSeries)) > 0 || u.In24 > 0 || u.Out24 > 0 {
			extra += 2
		}
		if !u.StorageAt.IsZero() {
			extra++
		}
	}
	tall := max(min((spare-extra)/2, limit), 1)
	if g := m.graphLines(s.Name, inner, tall); len(g) > 0 {
		lines = append(lines, "")
		lines = append(lines, g...)
	}
	title := statusMark(s.Status) + " " + s.Name
	note := statusText(s)
	if zoomed {
		note = "z to go back"
	}
	return box(title, lines, w, h, zoomed, note)
}

// logBox is the selected service's log, its newest lines filling the panel.
func (m Model) logBox(w, h int) []string {
	lg := m.narrowed(w)
	lg.logRows = max(h-2, 1)
	return box("log · "+m.selectedName(), paneLines(lg.logPane()), w, h, false, "L full screen")
}

// systemRow is usage summed across every service, drawn as charts the way
// btop draws its CPU and memory: CPU, memory, and the gateway's traffic
// when it has carried any and the terminal is wide enough for three.
func (m Model) systemRow(w, h int) []string {
	var cpu, mem, in, out [][]float64
	var cpuNow, memNow float64
	for _, u := range m.usage {
		cpu = append(cpu, m.series(u.RecentCPU, u.CPUSeries))
		mem = append(mem, m.series(u.RecentRSS, u.RSSSeries))
		in = append(in, m.series(u.RecentIn, u.InSeries))
		out = append(out, m.series(u.RecentOut, u.OutSeries))
		if u.Measured {
			cpuNow += u.CPU
			memNow += float64(u.RSS)
		}
	}
	cpuSum, memSum := sumSeries(cpu), sumSeries(mem)
	inSum, outSum := sumSeries(in), sumSeries(out)
	traffic := (ceiling(inSum) > 0 || ceiling(outSum) > 0) && w >= 150
	n := 2
	if traffic {
		n = 3
	}
	widths := split(w, n)
	chartH := max(h-2, 1)
	panel := func(title string, cw int, series []chartSeries, label func(float64) string, note string) []string {
		return box(title, lineChart(series, boxInner(cw), chartH, label), cw, h, false, note)
	}
	cols := [][]string{
		panel("cpu · "+fmtCPU(cpuNow), widths[0],
			[]chartSeries{{name: "cpu", vals: cpuSum, style: graphStyle}},
			fmtCPU, m.rangeLabel()+" · every service"),
		panel("memory · "+fmtBytes(memNow), widths[1],
			[]chartSeries{{name: "mem", vals: memSum, style: lipgloss.NewStyle().Foreground(colOK)}},
			func(v float64) string { return fmtBytes(v) }, "peak "+fmtBytes(ceiling(memSum))),
	}
	if traffic {
		cols = append(cols, panel("traffic · in/out", widths[2],
			[]chartSeries{
				{name: "in", vals: inSum, style: lipgloss.NewStyle().Foreground(colOK)},
				{name: "out", vals: outSum, style: lipgloss.NewStyle().Foreground(colWarn)},
			},
			func(v float64) string { return fmtBytes(v) }, "through the gateway"))
	}
	return beside(cols...)
}

// window is the slice of services to show. It is derived from the cursor
// rather than stored, so the highlight can never drift away from the service
// the operator chose: the selection is the input and the viewport follows it.
// The cursor is kept near the middle so moving off either end scrolls.
func (m Model) window(avail int) (int, int) {
	n := len(m.services)
	if avail >= n {
		return 0, n
	}
	start := m.cursor - avail/2
	if start > n-avail {
		start = n - avail
	}
	if start < 0 {
		start = 0
	}
	return start, start + avail
}

func (m Model) header() string {
	title := titleStyle.Render("Outpost")

	var count string
	switch {
	case m.err != nil:
		count = countStyle.Render("disconnected")
	case len(m.services) == 1:
		count = countStyle.Render("1 service")
	default:
		count = countStyle.Render(fmt.Sprintf("%d services", len(m.services)))
	}

	if m.watching && m.err == nil {
		count = countStyle.Render("live · ") + count
	}
	if m.daemonVersion != "" && m.err == nil {
		title += countStyle.Render("  " + m.daemonVersion)
	}
	gap := m.width - lipgloss.Width(title) - lipgloss.Width(count) - 4
	if gap < 1 {
		gap = 1
	}
	return "  " + title + strings.Repeat(" ", gap) + count
}

// top is everything above the body: the title, the tabs and any banner.
func (m Model) top() string {
	lines := []string{m.header()}
	if _, ok := m.extras(); ok && m.err == nil {
		lines = append(lines, m.tabs())
	}
	if banner := m.configBanner(); banner != "" {
		lines = append(lines, banner)
	}
	return strings.Join(lines, "\n")
}

// footer is everything below the body: the notice, when there is one, and
// the key help.
func (m Model) footer() string {
	help := "  " + m.helpLine()
	if m.notice == "" {
		return help
	}
	style := noticeStyle
	if m.noticeIsErr {
		style = errorStyle
	}
	return "  " + style.Render(truncate(m.notice, m.width-4)) + "\n" + help
}

// errorPane explains a missing daemon instead of showing an empty table.
func (m Model) errorPane() string {
	var b strings.Builder
	b.WriteString("  " + errorStyle.Render(m.err.Error()) + "\n\n")
	b.WriteString("  " + labelStyle.Render("Start one with `outpost serve`, or have it run automatically:") + "\n")
	b.WriteString("  " + labelStyle.Render("outpost install --enable") + "\n\n")
	b.WriteString("  " + labelStyle.Render("This view reconnects on its own once a daemon is up."))
	return b.String()
}

// statusText is the status word plus any qualifier worth seeing at a glance.
func statusText(s supervisor.ServiceStatus) string {
	if s.Halted {
		return s.Status + " (halted)"
	}
	return s.Status
}

// updateText is the table's word on a newer release, if there is one to install.
func updateText(s supervisor.ServiceStatus) string {
	if !s.UpdateAvailable {
		return ""
	}
	return s.LatestRelease + " available"
}

// colWidths sizes the table columns to their contents.
//
// update stays 0 unless a service has one waiting, and then the column isn't
// drawn: most of the time it would say nothing, on a table already tight at 80
// columns.
type colWidths struct{ name, rt, status, uptime, restarts, update int }

func (m Model) columnWidths() colWidths {
	w := colWidths{name: len("NAME"), rt: len("RUNTIME"), status: len("STATUS"),
		uptime: len("UPTIME"), restarts: len("RESTARTS")}
	// Widths are display cells, matching pad: a byte count over-pads any column
	// holding a multi-byte name.
	for _, s := range m.services {
		if n := lipgloss.Width(s.Name); n > w.name {
			w.name = n
		}
		if n := lipgloss.Width(s.Runtime); n > w.rt {
			w.rt = n
		}
		// +2 for the status glyph and its space.
		if n := lipgloss.Width(statusText(s)) + 2; n > w.status {
			w.status = n
		}
		if n := lipgloss.Width(uptime(s)); n > w.uptime {
			w.uptime = n
		}
		if n := len(fmt.Sprintf("%d", s.RestartCount)); n > w.restarts {
			w.restarts = n
		}
		if u := updateText(s); u != "" {
			w.update = max(w.update, lipgloss.Width("UPDATE"), lipgloss.Width(u))
		}
	}
	return w
}

// table renders every service, unbounded by terminal height. View uses
// tableRange instead; this is the whole-list form.
func (m Model) table() string { return m.tableRange(0, len(m.services)) }

// tableRange is the service list, one row per service, covering
// services[start:end].
func (m Model) tableRange(start, end int) string {
	w := m.columnWidths()

	head := fmt.Sprintf("  %s  %s  %s  %s  %s",
		pad("NAME", w.name), pad("RUNTIME", w.rt), pad("STATUS", w.status),
		pad("UPTIME", w.uptime), pad("RESTARTS", w.restarts))
	if w.update > 0 {
		head += "  " + pad("UPDATE", w.update)
	}
	note := m.rangeNote(start, end)
	reserve := 0
	if note != "" {
		reserve = lipgloss.Width(note) + 2
	}
	spark, cpuW, memW := m.usageColumns(lipgloss.Width(head) + reserve)
	if cpuW > 0 {
		head += "  " + pad("CPU", cpuW) + "  " + pad("MEM", memW)
	}

	var b strings.Builder
	b.WriteString("  " + headerStyle.Render(head))
	// The range indicator rides on the header row rather than taking a line of
	// its own, so the chrome is the same height whether or not the list is
	// windowed — which is what lets View measure it in one pass.
	// Left off when it won't fit beside the header: the cursor's row is on
	// screen either way, and a header that runs past the edge wraps.
	if gap := m.width - lipgloss.Width(head) - lipgloss.Width(note) - 4; note != "" && gap >= 2 && !m.noteInBorder {
		b.WriteString(strings.Repeat(" ", gap) + countStyle.Render(note))
	}
	b.WriteString("\n")

	for i, s := range m.services[start:end] {
		i += start
		marker := "  "
		style := rowStyle
		if i == m.cursor {
			marker = "▸ "
			style = selectedRow
		}

		status := statusStyle(s.Status).Render(
			pad(statusMark(s.Status)+" "+statusText(s), w.status))

		row := fmt.Sprintf("%s  %s  %s  %s  %s",
			pad(s.Name, w.name), pad(s.Runtime, w.rt), status,
			pad(uptime(s), w.uptime), pad(fmt.Sprintf("%d", s.RestartCount), w.restarts))
		if w.update > 0 {
			row += "  " + pad(updateText(s), w.update)
		}
		if cpuW > 0 {
			cpu, mem := m.usageCells(s.Name, spark)
			row += "  " + pad(cpu, cpuW) + "  " + pad(mem, memW)
		}

		b.WriteString("  " + style.Render(marker) + style.Render(row) + "\n")
	}
	return b.String()
}

// usageColumns sizes the table's CPU and MEM columns to the room left of
// width: with a sparkline of recent CPU, without one, or not at all. They
// appear only once there is usage to show.
func (m Model) usageColumns(used int) (spark, cpuW, memW int) {
	if len(m.usage) == 0 {
		return 0, 0, 0
	}
	const memCol = 9 // "1023 MiB"
	room := m.width - 4 - used
	switch {
	case room >= 2+5+1+10+2+memCol:
		return 10, 5 + 1 + 10, memCol
	case room >= 2+5+2+memCol:
		return 0, 5, memCol
	}
	return 0, 0, 0
}

// rangeNote says which slice of a too-long list is on screen. Empty when all
// of it fits, so the usual case stays uncluttered.
func (m Model) rangeNote(start, end int) string {
	n := len(m.services)
	if n == 0 || (start == 0 && end >= n) {
		return ""
	}
	return fmt.Sprintf("showing %d–%d of %d", start+1, end, n)
}

// detail is the pane for the selected service.
func (m Model) detail() string {
	s, ok := m.selected()
	if !ok {
		return ""
	}

	// The panel's title names the service.
	var b strings.Builder

	// One line of the things you check first.
	facts := []string{}
	if s.Port > 0 {
		facts = append(facts, fmt.Sprintf("port %d", s.Port))
	}
	if s.Healthcheck != "" {
		facts = append(facts, s.Healthcheck)
	}
	if s.RestartPolicy != "" {
		facts = append(facts, fmt.Sprintf("%s, backoff %ds", s.RestartPolicy, s.Backoff))
	}
	if len(facts) > 0 {
		b.WriteString("  " + valueStyle.Render(strings.Join(facts, " · ")) + "\n")
	}

	kv := func(k, v string) {
		if v == "" {
			return
		}
		// Cut to the line, as log lines are: a value that wraps, such as a
		// tailnet node's last error, pushes everything below it down.
		b.WriteString("  " + labelStyle.Render(pad(k, 10)) + valueStyle.Render(truncate(v, m.width-12)) + "\n")
	}
	m.serviceGateway(s, kv)
	kv("tailscale", s.TailnetDetail())
	if t := s.Tailnet; t != nil && t.LoginURL != "" {
		// Never cut: a login URL with an ellipsis in it can't be opened, and
		// wrapping is the lesser harm.
		b.WriteString("  " + labelStyle.Render(pad("login", 10)) + valueStyle.Render(t.LoginURL) + "\n")
	}
	kv("storage", s.Storage)
	kv("instance", s.InstanceID)
	if s.Source != "" {
		kv("installed", s.Release)
		if s.UpdateAvailable {
			kv("available", s.LatestRelease)
		}
	}
	if s.Desired != "" && s.Desired != "running" {
		kv("desired", s.Desired)
	}
	if s.Halted {
		b.WriteString("  " + errorStyle.Render("halted — not being restarted; press s to start it") + "\n")
	}
	if !s.NextAttempt.IsZero() {
		if wait := timeUntil(s.NextAttempt); wait != "" {
			kv("retry in", wait)
		}
	}

	if len(m.events) > 0 {
		b.WriteString("\n  " + sectionStyle.Render("recent activity") + "\n")
		for _, e := range m.events {
			fmt.Fprintf(&b, "  %s  %s  %s\n",
				labelStyle.Render(e.At.Format("15:04:05")),
				labelStyle.Render(pad(e.Kind, 11)),
				valueStyle.Render(e.Message))
		}
	}
	return b.String()
}

// logPane shows the tail of the selected service's log.
func (m Model) logPane() string {
	var b strings.Builder
	if m.logText == "" {
		b.WriteString("  " + labelStyle.Render("no output yet") + "\n")
		return b.String()
	}
	lines := strings.Split(strings.TrimRight(plainText(m.logText), "\n"), "\n")
	rows := m.logRows
	if rows <= 0 {
		rows = logLines
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	for _, line := range lines {
		b.WriteString("  " + lineStyle(line).Render(truncate(line, m.width-4)) + "\n")
	}
	return b.String()
}

// rule is a labelled horizontal divider.
func (m Model) rule(label string) string {
	text := sectionStyle.Render(label)
	fill := m.width - lipgloss.Width(text) - 6
	if fill < 1 {
		fill = 1
	}
	return text + " " + ruleStyle.Render(strings.Repeat("─", fill))
}

// truncate shortens s to fit width display cells, appending an ellipsis.
//
// Width is measured in cells, not bytes: log lines carry arbitrary UTF-8, and
// slicing by byte both overshoots the terminal width and can cut a rune in half.
func truncate(s string, width int) string {
	if width < 1 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}

	// Accumulate the width as we go: re-measuring the whole prefix per rune is
	// quadratic, and log lines are neither short nor rare.
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > width-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + "…"
}
