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
	if m.err != nil || !m.loaded || len(m.services) == 0 {
		return m.render(0, 0, false)
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
	b.WriteString(m.header())
	b.WriteString("\n\n")

	switch {
	case m.err != nil:
		b.WriteString(m.errorPane())
	case !m.loaded:
		b.WriteString(labelStyle.Render("  loading…"))
	case len(m.services) == 0:
		b.WriteString(labelStyle.Render("  No services declared. Add a manifest under services/, then run `outpost apply`."))
	default:
		b.WriteString(m.tableRange(start, end))
		if !compact {
			b.WriteString("\n")
			b.WriteString(m.detail())
			if m.showLogs {
				b.WriteString("\n")
				b.WriteString(m.logPane())
			}
		}
	}

	if m.notice != "" {
		style := noticeStyle
		if m.noticeIsErr {
			style = errorStyle
		}
		b.WriteString("\n\n  " + style.Render(m.notice))
	}

	b.WriteString("\n\n  " + m.help.View(m.keys))
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

// colWidths sizes the table columns to their contents.
type colWidths struct{ name, rt, status, uptime, restarts int }

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

	var b strings.Builder
	b.WriteString("  " + headerStyle.Render(head))
	// The range indicator rides on the header row rather than taking a line of
	// its own, so the chrome is the same height whether or not the list is
	// windowed — which is what lets View measure it in one pass.
	if note := m.rangeNote(start, end); note != "" {
		gap := m.width - lipgloss.Width(head) - lipgloss.Width(note) - 4
		if gap < 2 {
			gap = 2
		}
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

		b.WriteString("  " + style.Render(marker) + style.Render(row) + "\n")
	}
	return b.String()
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
		b.WriteString("  " + labelStyle.Render(pad(k, 10)) + valueStyle.Render(v) + "\n")
	}
	kv("tailscale", s.TailscaleName)
	kv("storage", s.Storage)
	kv("instance", s.InstanceID)
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
	for _, line := range strings.Split(strings.TrimRight(m.logText, "\n"), "\n") {
		b.WriteString("  " + logStyle.Render(truncate(line, m.width-4)) + "\n")
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
