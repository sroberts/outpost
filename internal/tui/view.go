package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/supervisor"
)

// View renders the dashboard.
func (m Model) View() string {
	if m.quitting {
		return ""
	}

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
		b.WriteString(m.table())
		b.WriteString("\n")
		b.WriteString(m.detail())
		if m.showLogs {
			b.WriteString("\n")
			b.WriteString(m.logPane())
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
	for _, s := range m.services {
		if n := len(s.Name); n > w.name {
			w.name = n
		}
		if n := len(s.Runtime); n > w.rt {
			w.rt = n
		}
		// +2 for the status glyph and its space.
		if n := len(statusText(s)) + 2; n > w.status {
			w.status = n
		}
		if n := len(uptime(s)); n > w.uptime {
			w.uptime = n
		}
		if n := len(fmt.Sprintf("%d", s.RestartCount)); n > w.restarts {
			w.restarts = n
		}
	}
	return w
}

// table is the service list, one row per service.
func (m Model) table() string {
	w := m.columnWidths()

	var b strings.Builder
	b.WriteString("  " + headerStyle.Render(fmt.Sprintf("  %s  %s  %s  %s  %s",
		pad("NAME", w.name), pad("RUNTIME", w.rt), pad("STATUS", w.status),
		pad("UPTIME", w.uptime), pad("RESTARTS", w.restarts))))
	b.WriteString("\n")

	for i, s := range m.services {
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
			b.WriteString(fmt.Sprintf("  %s  %s  %s\n",
				labelStyle.Render(e.At.Format("15:04:05")),
				labelStyle.Render(pad(e.Kind, 11)),
				valueStyle.Render(e.Message)))
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

	var b strings.Builder
	for _, r := range s {
		if lipgloss.Width(b.String()+string(r)) > width-1 {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}
