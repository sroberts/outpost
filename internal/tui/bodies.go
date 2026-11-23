package tui

import (
	"fmt"
	"strings"
)

// The views beyond the services list, each as panels filling the body.

// overviewBody is the overview: usage across every service along the top
// when there's room, then the tiles in a grid that takes the rest, one to
// three across as the width allows and every row an equal share of the
// height. On a terminal too short for every row, the rows around the
// focused tile are shown.
func (m Model) overviewBody(w, h int) []string {
	var out []string
	if len(m.usage) > 0 && h >= 30 && w >= 80 {
		sysH := max(min(h/4, 12), 7)
		out = append(out, m.systemRow(w, sysH)...)
		h -= sysH
	}
	ts := m.tiles()
	cursor := min(m.ov.cursor, len(ts)-1)
	cols := 1
	switch {
	case w >= 170:
		cols = 3
	case w >= 100:
		cols = 2
	}
	nrows := (len(ts) + cols - 1) / cols
	const minRow = 5
	shown := max(min(nrows, h/minRow), 1)
	first := 0
	if r := cursor / cols; r >= shown {
		first = r - shown + 1
	}
	// Each row asks for its fullest tile; a long service list gets the room
	// an empty activity feed doesn't need.
	needs := make([]int, shown)
	for i := range needs {
		r := first + i
		for _, t := range ts[r*cols : min(r*cols+cols, len(ts))] {
			needs[i] = max(needs[i], len(t.lines)+2)
		}
	}
	heights := allot(h, minRow, needs...)
	for i, rh := range heights {
		r := first + i
		row := ts[r*cols : min(r*cols+cols, len(ts))]
		widths := split(w, len(row))
		var panels [][]string
		for j, t := range row {
			idx := r*cols + j
			panels = append(panels, tileBox(t, widths[j], rh, idx == cursor))
		}
		out = append(out, beside(panels...)...)
	}
	return out
}

// tileBox is one overview tile as a panel, saying how much it had to leave
// out when its lines don't all fit.
func tileBox(t tile, w, h int, focused bool) []string {
	rows := max(h-2, 1)
	lines := t.lines
	note := ""
	if len(lines) > rows {
		note = fmt.Sprintf("%d more", len(lines)-rows+1)
		lines = append(append([]string(nil), lines[:rows-1]...), countStyle.Render("…"))
	}
	if focused {
		note = strings.TrimSpace(note + "  enter opens")
	}
	return box(t.title, lines, w, h, focused, note)
}

// usersBody is the users and, below or beside them, where the selected one
// is signed in.
func (m Model) usersBody(w, h int) []string {
	list := func(bw, bh int) []string {
		return box("users", paneLines(m.narrowed(bw).usersPane()), bw, bh, true, countNote(len(m.users), "user"))
	}
	if m.usersErr != nil || len(m.users) == 0 {
		return list(w, h)
	}
	signed := func(bw, bh int) []string {
		return box("signed in: "+m.selectedUser(), paneLines(m.narrowed(bw).signInsPane()), bw, bh, false, countNote(len(m.signIns), "session"))
	}
	if w >= 160 {
		ws := split(w, 2)
		return beside(list(ws[0], h), signed(ws[1], h))
	}
	listH := min(len(m.users)+3, max(h/2, 5))
	if h-listH < 4 {
		return list(w, h)
	}
	return append(list(w, listH), signed(w, h-listH)...)
}

// tokensBody is the access tokens above the connected OAuth clients.
func (m Model) tokensBody(w, h int) []string {
	onTokens := m.mg.tokenCursor < len(m.mg.tokens)
	tokens := func(bh int) []string {
		return box("access tokens", paneLines(m.narrowed(w).tokensPane()), w, bh, onTokens, countNote(len(m.mg.tokens), "token"))
	}
	if m.mg.tokensErr != nil || (m.mg.tokens == nil && m.mg.clients == nil) {
		return tokens(h)
	}
	tokH := min(max(len(m.mg.tokens), 1)+3, max(h/2, 5))
	if h-tokH < 4 {
		return tokens(h)
	}
	clients := box("connected clients", paneLines(m.narrowed(w).clientsPane()), w, h-tokH, !onTokens, countNote(len(m.mg.clients), "client"))
	return append(tokens(tokH), clients...)
}

// gatewayBody is the gateway's addresses, then the apps it routes and what
// it has decided lately, side by side when there's room.
func (m Model) gatewayBody(w, h int) []string {
	info := paneLines(m.gatewayPane())
	if m.gateway == nil {
		return box("gateway", info, w, h, false, "")
	}
	infoH := min(len(info)+2, h)
	out := box("gateway", info, w, infoH, false, "")
	rest := h - infoH
	if rest < 4 {
		return out
	}
	routes := func(bw, bh int) []string {
		return box("routes", paneLines(m.narrowed(bw).routesPane()), bw, bh, false, countNote(len(m.gateway.Routes), "app"))
	}
	activity := func(bw, bh int) []string {
		return box("recent activity", paneLines(m.narrowed(bw).gatewayActivityPane()), bw, bh, false, "")
	}
	if w >= 140 {
		ws := split(w, 2)
		return append(out, beside(routes(ws[0], rest), activity(ws[1], rest))...)
	}
	routesH := min(len(m.gateway.Routes)+3, max(rest/2, 4))
	if rest-routesH < 4 {
		return append(out, routes(w, rest)...)
	}
	return append(append(out, routes(w, routesH)...), activity(w, rest-routesH)...)
}

// overlayBody is the palette, a form or a command's output, as a panel
// across the body, no wider than reads well.
func (m Model) overlayBody(w, h int) []string {
	bw := min(w, 120)
	inner := m.narrowed(bw)
	var title, note string
	var lines []string
	focused := true
	switch {
	case m.out != nil:
		title = m.out.title
		lines, note = inner.outputLines(h - 2)
	case m.pal.form != nil:
		title = m.pal.form.act.title
		lines = inner.formLines()
	default:
		title = "commands"
		lines, note = inner.paletteLines(h - 2)
	}
	panel := box(title, lines, bw, h, focused, note)
	if left := (w - bw) / 2; left > 0 {
		gap := strings.Repeat(" ", left)
		for i := range panel {
			panel[i] = gap + panel[i]
		}
	}
	return panel
}
