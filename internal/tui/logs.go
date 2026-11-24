package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/supervisor"
)

const (
	// logViewLines is how much of one service's log the logs view reads.
	logViewLines = 2000
	// logAllLines is how much of each log the every-service view reads per
	// poll, to find what's new.
	logAllLines = 200
	// logKeep bounds what the every-service view holds.
	logKeep = 5000
	// logEvery is how often the logs view re-reads, faster than the
	// dashboard's tick, so following a log feels live.
	logEvery = time.Second
)

// logLine is one line of the logs view; svc is set in the every-service
// view, where lines from all of them interleave.
type logLine struct {
	svc, text string
}

// logView is the logs view's state.
type logView struct {
	service string // whose log; "" with all
	all     bool   // every service's, interleaved as it arrives
	events  bool   // the activity feed rather than the log file
	own     string // one of Outpost's own logs (ownSources) rather than either

	lines  []logLine
	err    string
	offset int // lines scrolled up from the end
	follow bool
	wrap   bool

	// minLevel hides lines below it: "", warn or error, cycled with v.
	minLevel string

	searching bool
	query     field
	pattern   string
	match     int // index into lines of the current match, -1 for none

	// last is the tail each service had at the last poll, so the
	// every-service view can tell new lines from old.
	last map[string][]string

	ticking bool
}

// reset clears what the view holds, for a new service or mode.
func (lv *logView) reset() {
	lv.lines, lv.err, lv.offset, lv.follow = nil, "", 0, true
	lv.last, lv.match = nil, -1
}

type logViewMsg struct {
	service string
	all     bool
	events  bool
	own     string
	lines   []logLine
	names   []string            // the every-service view's services, in order
	tails   map[string][]string // the every-service view's new tails
	err     error
}

type logTickMsg time.Time

func logTick() tea.Cmd {
	return tea.Tick(logEvery, func(t time.Time) tea.Msg { return logTickMsg(t) })
}

// startLogTicks starts the logs view's own refresh, unless it's running.
func (m Model) startLogTicks() (Model, tea.Cmd) {
	if m.lv.ticking {
		return m, nil
	}
	m.lv.ticking = true
	return m, logTick()
}

// ownSources are Outpost's own logs, in the order o steps through them, and
// what the view's title calls each.
var ownSources = []string{applog.SourceDaemon, applog.SourceAccess}

var ownTitles = map[string]string{applog.SourceDaemon: "Outpost's own log", applog.SourceAccess: "requests to the gateway"}

// fetchLogView reads what the logs view shows.
func (m Model) fetchLogView() tea.Cmd {
	lv := m.lv
	if lv.own != "" && m.ownLogs != nil {
		read := m.ownLogs
		return func() tea.Msg {
			text, err := read(lv.own, logViewLines)
			var lines []logLine
			for _, l := range splitLines(text) {
				lines = append(lines, logLine{text: l})
			}
			return logViewMsg{service: lv.service, all: lv.all, events: lv.events, own: lv.own, lines: lines, err: err}
		}
	}
	if lv.events {
		client, svc := m.client, lv.service
		if lv.all {
			svc = ""
		}
		return func() tea.Msg {
			events, err := client.Events(svc, 500)
			lines := make([]logLine, 0, len(events))
			// Newest first from the daemon; a log reads oldest first.
			for i := len(events) - 1; i >= 0; i-- {
				e := events[i]
				lines = append(lines, logLine{svc: e.Service, text: eventLine(e)})
			}
			return logViewMsg{service: lv.service, all: lv.all, events: true, lines: lines, err: err}
		}
	}
	if m.logs == nil {
		return nil
	}
	read := m.logs
	if !lv.all {
		svc := lv.service
		if svc == "" {
			return nil
		}
		return func() tea.Msg {
			text, err := read(svc, logViewLines)
			var lines []logLine
			for _, l := range splitLines(text) {
				lines = append(lines, logLine{text: l})
			}
			return logViewMsg{service: svc, lines: lines, err: err}
		}
	}
	names := make([]string, 0, len(m.services))
	for _, s := range m.services {
		if s.Status != supervisor.StatusRemoved {
			names = append(names, s.Name)
		}
	}
	// Only the tails are read here; what is new in them is worked out when
	// they're applied, against the tails applied last. Reads overlap (the
	// view ticks every second whether or not the last one answered), and two
	// diffed against the same earlier tails would both add the same lines.
	return func() tea.Msg {
		tails := map[string][]string{}
		for _, name := range names {
			text, err := read(name, logAllLines)
			if err != nil {
				continue // no log yet
			}
			tails[name] = splitLines(text)
		}
		return logViewMsg{all: true, names: names, tails: tails}
	}
}

// freshLines is what tails add to the every-service view since last, in
// names' order.
func freshLines(names []string, last, tails map[string][]string) []logLine {
	var lines []logLine
	for _, name := range names {
		cur, ok := tails[name]
		if !ok {
			continue
		}
		fresh := cur
		if last != nil {
			fresh = newSince(last[name], cur)
		} else if len(fresh) > 20 {
			// Opening on every service: enough of each to see where it
			// is, not two hundred lines of each.
			fresh = fresh[len(fresh)-20:]
		}
		for _, l := range fresh {
			lines = append(lines, logLine{svc: name, text: l})
		}
	}
	return lines
}

func eventLine(e supervisor.EventRecord) string {
	return fmt.Sprintf("%s  %-12s %s", e.At.Local().Format("2006-01-02 15:04:05"), e.Kind, e.Message)
}

func splitLines(text string) []string {
	text = strings.TrimRight(plainText(text), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// newSince is the lines cur has after the end of prev: the tail of a log
// read twice, the second time with more written. A tail is a window that
// slides forward, so cur is prev with its first d lines gone and the new ones
// after; the smallest such d is the answer. Matching the whole overlap, not
// a few anchor lines, keeps a log that repeats itself ("GET /up 200" every
// second) from reading as if nothing were written. With no overlap (the log
// rotated, or more was written than a tail holds) all of cur is new. A full
// window of one repeated line can't show whether more was written; that
// reads as nothing new.
func newSince(prev, cur []string) []string {
	for d := range len(prev) {
		n := len(prev) - d
		if n <= len(cur) && equalLines(cur[:n], prev[d:]) {
			return cur[n:]
		}
	}
	return cur
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// updateLogView applies a read to the view.
func (m Model) updateLogView(msg logViewMsg) Model {
	lv := &m.lv
	if msg.own != lv.own || msg.events != lv.events || msg.all != lv.all || (!msg.all && msg.service != lv.service) {
		return m // for a view the operator has since left
	}
	switch {
	case msg.err != nil && msg.own != "":
		lv.err = msg.err.Error()
	case msg.err != nil && !msg.events:
		lv.lines, lv.err = nil, "no log yet"
	case msg.err != nil:
		lv.err = msg.err.Error()
	case msg.all && !msg.events && msg.own == "":
		fresh := atLevel(freshLines(msg.names, lv.last, msg.tails), lv.minLevel)
		had := len(lv.lines)
		lv.lines = append(lv.lines, fresh...)
		dropped := 0
		if over := len(lv.lines) - logKeep; over > 0 {
			lv.lines, dropped = lv.lines[over:], over
		}
		lv.last, lv.err = msg.tails, ""
		lv.hold(had, len(fresh), dropped)
	default:
		if !msg.events {
			msg.lines = atLevel(msg.lines, lv.minLevel)
		}
		// A log longer than the read is a window that slides, so what's new
		// is found by matching, not by the count growing.
		grown := len(msg.lines)
		if len(lv.lines) > 0 {
			grown = len(newSince(logTexts(lv.lines), logTexts(msg.lines)))
		}
		lv.hold(len(lv.lines), grown, len(lv.lines)+grown-len(msg.lines))
		lv.lines, lv.err = msg.lines, ""
	}
	if lv.pattern != "" && lv.match >= len(lv.lines) {
		lv.match = -1
	}
	return m
}

// hold keeps a scrolled-back view on the same lines, and the search on the
// same match, when grown lines arrive at the end of the had there were and
// dropped go from the front.
func (lv *logView) hold(had, grown, dropped int) {
	if !lv.follow {
		lv.offset += grown
	}
	// A match at or past had is the search's "none found yet" mark, not a
	// line, and moving it would land it on one.
	if lv.match >= 0 && lv.match < had {
		lv.match -= max(dropped, 0)
		if lv.match < 0 {
			lv.match = -1
		}
	}
}

// atLevel keeps the lines at min or above, read from the level tag each
// line carries (applog.Text); "" keeps them all.
func atLevel(lines []logLine, min string) []logLine {
	if min == "" {
		return lines
	}
	var out []logLine
	for _, l := range lines {
		if applog.AtLeast(applog.TextLevel(l.text), min) && applog.TextLevel(l.text) != applog.LevelNone {
			out = append(out, l)
		}
	}
	return out
}

// lineStyle colours a line by the level it's tagged with.
func lineStyle(text string) lipgloss.Style {
	switch applog.TextLevel(text) {
	case applog.LevelError:
		return errorStyle
	case applog.LevelWarn:
		return noticeStyle
	}
	return logStyle
}

func logTexts(lines []logLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.text
	}
	return out
}

// handleLogKey is the logs view's keys.
func (m Model) handleLogKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	lv := &m.lv
	if lv.searching {
		switch msg.String() {
		case "esc":
			lv.searching = false
		case "enter":
			lv.searching, lv.pattern = false, lv.query.Value()
			lv.match = len(lv.lines)
			m.findMatch(-1)
		default:
			lv.query = lv.query.Update(msg)
		}
		return m, nil, true
	}
	page := max(m.height-8, 1)
	top := m.maxLogOffset()
	scroll := func(by int) {
		lv.offset = max(min(lv.offset+by, top), 0)
		lv.follow = lv.offset == 0
	}
	switch msg.String() {
	case "up", "k":
		scroll(1)
	case "down", "j":
		scroll(-1)
	case "pgup", "b", "ctrl+u":
		scroll(page)
	case "pgdown", " ", "ctrl+d":
		scroll(-page)
	case "g", "home":
		scroll(top)
	case "G", "end":
		lv.offset, lv.follow = 0, true
	case "f":
		lv.follow = !lv.follow
		if lv.follow {
			lv.offset = 0
		}
	case "w":
		lv.wrap = !lv.wrap
	case "/":
		lv.searching, lv.query = true, newInput("search", false)
		lv.query.SetValue(lv.pattern)
	case "n":
		m.findMatch(-1)
	case "N":
		m.findMatch(1)
	case "esc":
		if lv.pattern == "" {
			return m, nil, false
		}
		lv.pattern, lv.match = "", -1
	case "o":
		if m.ownLogs == nil {
			return m, nil, true
		}
		// Steps through Outpost's own logs, then back to the services'.
		next := ownSources[0]
		for i, s := range ownSources {
			if s == lv.own {
				next = ""
				if i+1 < len(ownSources) {
					next = ownSources[i+1]
				}
			}
		}
		lv.own = next
		lv.reset()
		return m, m.fetchLogView(), true
	case "e":
		lv.events, lv.own = !lv.events, ""
		lv.reset()
		return m, m.fetchLogView(), true
	case "a":
		lv.all, lv.own = !lv.all, ""
		if !lv.all && lv.service == "" {
			lv.service = m.selectedName()
		}
		lv.reset()
		return m, m.fetchLogView(), true
	case "v":
		if lv.events {
			return m, nil, true // the activity feed has no levels
		}
		lv.minLevel = map[string]string{"": applog.LevelWarn, applog.LevelWarn: applog.LevelError}[lv.minLevel]
		lv.reset()
		return m, m.fetchLogView(), true
	case "[", "]":
		if len(m.services) == 0 {
			return m, nil, true
		}
		at := 0
		for i, s := range m.services {
			if s.Name == lv.service {
				at = i
			}
		}
		if lv.all || lv.own != "" {
			lv.all, lv.own = false, ""
		} else if msg.String() == "]" {
			at = (at + 1) % len(m.services)
		} else {
			at = (at + len(m.services) - 1) % len(m.services)
		}
		lv.service = m.services[at].Name
		lv.reset()
		return m, m.fetchLogView(), true
	case "s", "x", "r", "l", "L":
		// The services view's actions; they mustn't act on a service the
		// operator can't see the row for.
	default:
		return m, nil, false
	}
	return m, nil, true
}

// findMatch moves to the next match of the pattern in dir: -1 is back,
// toward older lines, as n is in less.
func (m *Model) findMatch(dir int) {
	lv := &m.lv
	if lv.pattern == "" || len(lv.lines) == 0 {
		return
	}
	needle := strings.ToLower(lv.pattern)
	i := lv.match
	if i < 0 || i > len(lv.lines) {
		i = len(lv.lines)
	}
	for range lv.lines {
		i += dir
		if i < 0 || i >= len(lv.lines) {
			m.notice, m.noticeIsErr = "no more matches for "+lv.pattern, false
			return
		}
		if strings.Contains(strings.ToLower(lv.lines[i].text), needle) {
			lv.match = i
			// Put the match a third of the way up the screen.
			lv.offset = min(max(len(lv.lines)-1-i-max((m.height-8)/3, 0), 0), m.maxLogOffset())
			lv.follow = false
			m.notice = ""
			return
		}
	}
}

// logViewTitle names what the logs view shows and how: the panel's title.
func (m Model) logViewTitle() string {
	lv := m.lv
	what := lv.service
	switch {
	case lv.own != "":
		what = ownTitles[lv.own]
	case lv.all && lv.events:
		what = "every service's activity"
	case lv.all:
		what = "every service"
	case lv.events:
		what += " activity"
	}
	var flags []string
	if lv.follow {
		flags = append(flags, "following")
	} else {
		flags = append(flags, fmt.Sprintf("%d lines up", lv.offset))
	}
	if lv.wrap {
		flags = append(flags, "wrapped")
	}
	if lv.minLevel != "" {
		flags = append(flags, lv.minLevel+" and up")
	}
	if lv.pattern != "" {
		flags = append(flags, "/"+lv.pattern)
	}
	return what + " · " + strings.Join(flags, " · ")
}

// logViewLines is the inside of the logs view's panel, height rows: the
// log, and the search field while it's open.
func (m Model) logViewLines(height int) []string {
	lv := m.lv
	var out []string
	rows := logRows(height, lv.searching)
	switch {
	case lv.err != "" && len(lv.lines) == 0:
		out = append(out, labelStyle.Render(lv.err))
	case len(lv.lines) == 0 && lv.service == "" && !lv.all:
		out = append(out, labelStyle.Render("No service selected."))
	case len(lv.lines) == 0:
		out = append(out, labelStyle.Render("no output yet"))
	default:
		out = append(out, m.visibleLogLines(rows)...)
	}
	if lv.searching {
		for len(out) < rows {
			out = append(out, "")
		}
		out = append(out, "/"+strings.TrimPrefix(lv.query.View(), "> "))
	}
	return out
}

// logRows is how many rows of log a panel height rows inside shows: less
// the search field while it's open.
func logRows(height int, searching bool) int {
	rows := max(height, 1)
	if searching {
		rows = max(rows-1, 1)
	}
	return rows
}

// logWidths is the service column's width in the every-service view (0
// otherwise) and the cells left for each line's text.
func (m Model) logWidths() (prefixW, avail int) {
	width := max(m.width-4, 1)
	if !m.lv.all || m.lv.own != "" {
		return 0, width
	}
	for _, l := range m.lv.lines {
		prefixW = max(prefixW, lipgloss.Width(l.svc))
	}
	prefixW = min(prefixW, 16)
	return prefixW, max(width-prefixW-1, 1)
}

// maxLogOffset is the furthest up the log view scrolls: the offset that puts
// the first line at the top of a full screen. Further would show the first
// line alone above blank rows. Wrapped lines count for the rows they take.
func (m Model) maxLogOffset() int {
	lv := m.lv
	rows := logRows(m.bodyHeight(), lv.searching)
	_, avail := m.logWidths()
	used := 0
	for k := range lv.lines {
		used++
		if lv.wrap {
			used += len(wrapCells(lv.lines[k].text, avail)) - 1
		}
		if used >= rows {
			return len(lv.lines) - (k + 1)
		}
	}
	return 0 // it all fits
}

// visibleLogLines are the rows on screen, bottom aligned, offset lines up
// from the end, each cut or wrapped to the width.
func (m Model) visibleLogLines(rows int) []string {
	lv := m.lv
	prefixW, textW := m.logWidths()
	needle := strings.ToLower(lv.pattern)
	render := func(i int) []string {
		l := lv.lines[i]
		body := l.text
		avail := textW
		prefix := ""
		if lv.all && lv.own == "" {
			prefix = countStyle.Render(pad(truncate(l.svc, prefixW), prefixW)) + " "
		}
		var parts []string
		if lv.wrap {
			parts = wrapCells(body, avail)
		} else {
			parts = []string{truncate(body, avail)}
		}
		out := make([]string, len(parts))
		for j, p := range parts {
			style := lineStyle(l.text)
			if i == lv.match {
				style = focusStyle
			}
			out[j] = highlight(p, needle, style)
			if j == 0 {
				out[j] = prefix + out[j]
			} else if lv.all && lv.own == "" {
				out[j] = strings.Repeat(" ", prefixW+1) + out[j]
			}
		}
		return out
	}
	end := len(lv.lines) - min(lv.offset, max(len(lv.lines)-1, 0))
	var out []string
	for i := end - 1; i >= 0 && len(out) < rows; i-- {
		r := render(i)
		out = append(r, out...)
	}
	if len(out) > rows {
		out = out[len(out)-rows:]
	}
	return out
}

// wrapCells breaks s into pieces of at most width display cells.
func wrapCells(s string, width int) []string {
	if lipgloss.Width(s) <= width {
		return []string{s}
	}
	var out []string
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > width {
			out = append(out, b.String())
			b.Reset()
			used = 0
		}
		b.WriteRune(r)
		used += rw
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// highlight styles s, with every match of needle shown reversed.
func highlight(s, needle string, style lipgloss.Style) string {
	if needle == "" {
		return style.Render(s)
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	for {
		i := strings.Index(lower, needle)
		// A match is found in the lowercased copy; only use its offsets
		// where lowering kept the byte lengths, as it does for ASCII.
		if i < 0 || len(lower) != len(s) {
			b.WriteString(style.Render(s))
			return b.String()
		}
		b.WriteString(style.Render(s[:i]))
		b.WriteString(matchStyle.Render(s[i : i+len(needle)]))
		s, lower = s[i+len(needle):], lower[i+len(needle):]
	}
}

func (m Model) logHelp(key func(k, what string) string, sep string) string {
	if m.lv.searching {
		return strings.Join([]string{key("enter", "search"), key("esc", "cancel")}, sep)
	}
	return strings.Join([]string{key("↑/↓", "scroll"), key("f", "follow"), key("/", "search"), key("n/N", "next/prev"),
		key("v", "level"), key("w", "wrap"), key("[/]", "service"), key("a", "all"), key("e", "activity"), key("o", "outpost"), key(":", "commands"), key("q", "quit")}, sep)
}
