package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The dashboard fills the terminal the way btop does: a header, the body as
// bordered panels that between them cover every cell, and the key help.
// Panels are drawn by hand rather than with a lipgloss border because the
// title sits in the top edge, and a lipgloss border has nowhere to put it.

var (
	borderStyle = lipgloss.NewStyle().Foreground(colSubtle)
	focusBorder = lipgloss.NewStyle().Foreground(colAccent)
	boxTitle    = lipgloss.NewStyle().Bold(true).Foreground(colText)
)

// boxInner is the room inside a panel w cells wide: the border and a space
// of margin on each side.
func boxInner(w int) int { return max(w-4, 1) }

// box draws a panel exactly w cells wide and h rows tall: a rounded border
// with title set into the top edge, note into the bottom one at the right,
// and body cut or padded to the w-4 by h-2 inside. focused draws it in the
// accent colour, for the panel the keys act on.
func box(title string, body []string, w, h int, focused bool, note string) []string {
	if w < 4 || h < 2 {
		return fitLines(body, max(w, 0), max(h, 0))
	}
	bs, ts := borderStyle, boxTitle
	if focused {
		bs, ts = focusBorder, selectedRow
	}
	out := make([]string, 0, h)
	out = append(out, edge("╭", "╮", title, ts, w, bs, false))
	inner := boxInner(w)
	side := bs.Render("│")
	for _, line := range fitLines(body, inner, h-2) {
		out = append(out, side+" "+line+" "+side)
	}
	return append(out, edge("╰", "╯", note, countStyle, w, bs, true))
}

// edge is a panel's top or bottom border with label set into it: at the
// left on the top edge, at the right on the bottom one. A label that won't
// fit is cut rather than pushing the corner past the panel.
func edge(left, right, label string, ls lipgloss.Style, w int, bs lipgloss.Style, alignRight bool) string {
	run := w - 2
	label = truncate(label, run-4)
	if label == "" {
		return bs.Render(left + strings.Repeat("─", run) + right)
	}
	fill := run - lipgloss.Width(label) - 3
	if alignRight {
		return bs.Render(left+strings.Repeat("─", fill)+" ") + ls.Render(label) + bs.Render(" ─"+right)
	}
	return bs.Render(left+"─ ") + ls.Render(label) + bs.Render(" "+strings.Repeat("─", fill)+right)
}

// fitLines makes lines exactly width cells wide and height rows tall: each
// cut or padded, rows dropped or blank ones added.
func fitLines(lines []string, width, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	cut := lipgloss.NewStyle().MaxWidth(max(width, 0))
	out := make([]string, 0, height)
	for _, l := range lines {
		// MaxWidth cuts at a cell boundary without breaking an escape.
		out = append(out, pad(cut.Render(l), width))
	}
	for len(out) < height {
		out = append(out, strings.Repeat(" ", max(width, 0)))
	}
	return out
}

// beside joins panels left to right, row by row. Each is already its own
// width and the same height, as box makes them.
func beside(cols ...[]string) []string {
	h := 0
	for _, c := range cols {
		h = max(h, len(c))
	}
	out := make([]string, h)
	for i := range out {
		var b strings.Builder
		for _, c := range cols {
			if i < len(c) {
				b.WriteString(c[i])
			}
		}
		out[i] = b.String()
	}
	return out
}

// split divides total into n shares as evenly as cells allow, the first
// ones taking the remainder.
func split(total, n int) []int {
	if n <= 0 {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		out[i] = total / n
		if i < total%n {
			out[i]++
		}
	}
	return out
}

// paneLines turns a pane written for the old single-column layout, every
// line indented two spaces, into lines for the inside of a panel.
func paneLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, "  ")
	}
	return lines
}

// narrowed is the model as a pane w cells wide sees it: the panes truncate
// to width-4, which is a panel's inside.
func (m Model) narrowed(w int) Model {
	m.width = w
	return m
}

// allot shares total rows among panels that each need some: every panel
// gets what it needs when that fits, the rows to spare going evenly to
// them all; when it doesn't, the largest give way first, none below least.
func allot(total, least int, needs ...int) []int {
	out := make([]int, len(needs))
	sum := 0
	for i, n := range needs {
		out[i] = max(n, least)
		sum += out[i]
	}
	if len(out) == 0 {
		return out
	}
	if sum <= total {
		for i, extra := range split(total-sum, len(out)) {
			out[i] += extra
		}
		return out
	}
	for sum > total {
		big := 0
		for i := range out {
			if out[i] > out[big] {
				big = i
			}
		}
		if out[big] <= least {
			break // the floor is taller than the terminal; it gets clipped
		}
		out[big]--
		sum--
	}
	return out
}
