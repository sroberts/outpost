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

// View renders the dashboard, bounded by the terminal height.
//
// The table is rendered twice: once with no service rows to measure how much
// room everything else needs, then again with whatever is left. Measuring
// rather than hand-counting the chrome means the table stays correct when the
// detail pane, the log pane or a notice changes shape.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	if m.err != nil || !m.loaded || len(m.services) == 0 || m.view != viewServices || m.overlayUp() {
		return m.render(0, 0, false)
	}
	if m.zoom {
		return m.renderZoom()
	}
	if m.wide() {
		return m.renderWide()
	}

	compact := false
	chrome := lipgloss.Height(m.render(0, 0, compact))
	avail := m.height - chrome
	if avail < 1 {
		// The detail and log panes alone are taller than the terminal. Drop
		// them: a list you can move around in is worth more than a detail pane
		// on a screen with no room for either.
		compact = true
		chrome = lipgloss.Height(m.render(0, 0, compact))
		avail = m.height - chrome
	}
	if avail < 1 {
		avail = 1
	}
	start, end := m.window(avail)
	// Rows the list doesn't need go to the log, so a short list on a tall
	// terminal leaves no screen empty below it.
	if spare := avail - (end - start); m.showLogs && !compact && spare > 0 {
		m.logRows = logLines + spare
	}
	return m.render(start, end, compact)
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

// render draws the dashboard with services[start:end] in the table. compact
// drops the panes below the list, for terminals too short to hold them.
func (m Model) render(start, end int, compact bool) string {
	var b strings.Builder
	b.WriteString(m.top())

	switch {
	case m.err != nil:
		b.WriteString(m.errorPane())
	case m.overlayUp():
		b.WriteString(m.overlay(m.bodyHeight()))
	case m.view == viewOverview:
		b.WriteString(m.overviewPane(m.bodyHeight()))
	case m.view == viewSecrets:
		b.WriteString(m.secretsPane())
	case m.view == viewTokens:
		b.WriteString(m.tokensPane())
	case m.view == viewLogs:
		b.WriteString(m.logViewPane(m.bodyHeight()))
	case m.view == viewUsers:
		b.WriteString(m.usersPane())
	case m.view == viewGateway:
		b.WriteString(m.gatewayPane())
	case m.view == viewAI:
		b.WriteString(m.aiPane())
	case !m.loaded:
		b.WriteString(labelStyle.Render("  loading…"))
	case len(m.services) == 0:
		b.WriteString(labelStyle.Render("  No services declared. Add a manifest under services/, then run `outpost apply`."))
	default:
		b.WriteString(m.tableRange(start, end))
		if !compact {
			b.WriteString("\n")
			b.WriteString(m.detail())
			if g := m.graphLines(m.selectedName(), m.width-4, 1); len(g) > 0 {
				b.WriteString("\n")
				for _, line := range g {
					b.WriteString("  " + line + "\n")
				}
			}
			if m.showLogs {
				b.WriteString("\n")
				b.WriteString(m.logPane())
			}
		}
	}

	b.WriteString(m.footer())
	return b.String()
}

// top is everything above the body: the title, the tabs and any banner.
func (m Model) top() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	if _, ok := m.extras(); ok && m.err == nil {
		b.WriteString(m.tabs() + "\n")
	}
	if banner := m.configBanner(); banner != "" {
		b.WriteString(banner + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// footer is everything below the body: the notice and the key help.
func (m Model) footer() string {
	var b strings.Builder
	if m.notice != "" {
		style := noticeStyle
		if m.noticeIsErr {
			style = errorStyle
		}
		b.WriteString("\n\n  " + style.Render(truncate(m.notice, m.width-4)))
	}
	b.WriteString("\n\n  " + m.helpLine())
	return b.String()
}

// bodyHeight is the rows left for the body between top and footer. A body
// ends in a newline, which the footer's leading blank line absorbs.
func (m Model) bodyHeight() int {
	return max(m.height-lipgloss.Height(m.top())-lipgloss.Height(m.footer())+1, 3)
}

// wide is a terminal with room for the list and the detail side by side.
func (m Model) wide() bool { return m.width >= 120 && m.height >= 24 }

// fit makes s exactly width cells wide and height rows tall: lines cut or
// padded, rows dropped or added.
func fit(s string, width, height int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	cut := lipgloss.NewStyle().MaxWidth(width)
	for i, l := range lines {
		lines[i] = pad(cut.Render(l), width)
	}
	for len(lines) < height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return lines
}

// renderWide is the services view on a wide terminal: the list on the left,
// the selected service's detail and graphs on the right, and the log, when
// it's on, across the bottom taking whatever height is left.
func (m Model) renderWide() string {
	var b strings.Builder
	b.WriteString(m.top())
	bodyH := m.bodyHeight()
	logsH := 0
	if m.showLogs {
		logsH = max(bodyH*2/5, 6)
	}
	paneH := bodyH - logsH
	if logsH > 0 {
		paneH-- // the gap between the panes and the log
	}

	// The list gets what it needs, up to half the screen; the detail the
	// rest, since graphs use every column they're given.
	natural := 0
	for _, line := range strings.Split(m.tableRange(0, len(m.services)), "\n") {
		natural = max(natural, lipgloss.Width(line))
	}
	leftW := min(natural+2, m.width/2)
	rightW := m.width - leftW - 3

	left := m
	left.width = leftW
	start, end := m.window(max(paneH-1, 1))
	leftLines := fit(left.tableRange(start, end), leftW, paneH)

	right := m
	right.width = rightW + 2
	detail := right.detail()
	used := lipgloss.Height(detail) + 1
	tall := max(min((paneH-used-5)/2, 6), 1)
	var rb strings.Builder
	rb.WriteString(detail)
	if g := m.graphLines(m.selectedName(), rightW-2, tall); len(g) > 0 {
		rb.WriteString("\n")
		for _, line := range g {
			rb.WriteString("  " + line + "\n")
		}
	}
	rightLines := fit(rb.String(), rightW, paneH)

	sep := ruleStyle.Render(" │ ")
	for i := range leftLines {
		b.WriteString(leftLines[i] + sep + rightLines[i] + "\n")
	}
	if logsH > 0 {
		lg := m
		lg.logRows = logsH - 1
		b.WriteString("\n" + lg.logPane())
	}
	b.WriteString(m.footer())
	return b.String()
}

// renderZoom gives the selected service's detail and graphs the whole
// body, with the charts as tall as it allows.
func (m Model) renderZoom() string {
	var b strings.Builder
	b.WriteString(m.top())
	bodyH := m.bodyHeight()
	detail := m.detail()
	tall := max(min((bodyH-lipgloss.Height(detail)-6)/2, 12), 1)
	var body strings.Builder
	body.WriteString(detail)
	if g := m.graphLines(m.selectedName(), m.width-4, tall); len(g) > 0 {
		body.WriteString("\n")
		for _, line := range g {
			body.WriteString("  " + line + "\n")
		}
	}
	for _, line := range fit(body.String(), m.width, bodyH) {
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
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
	if gap := m.width - lipgloss.Width(head) - lipgloss.Width(note) - 4; note != "" && gap >= 2 {
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

	var b strings.Builder
	b.WriteString("  " + m.rule(s.Name) + "\n")

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
	b.WriteString("  " + m.rule("logs") + "\n")
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
