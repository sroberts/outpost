package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Runner runs an outpost subcommand and returns what it printed. The CLI
// supplies it, so commands whose logic lives on the client side (add,
// update, remove, apply's validation) run exactly the code they do in a
// shell, as the portal's add and update pages do. Without one, the palette
// offers only what goes over the socket.
type Runner func(args ...string) (string, error)

// WithRunner gives the dashboard the CLI's own commands.
func (m Model) WithRunner(r Runner) Model {
	m.runner = r
	return m
}

// fieldKind is how a form field takes input.
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldSecret
	fieldChoice
)

// formField is one input on an action's form.
type formField struct {
	name, label, placeholder string
	kind                     fieldKind
	choices                  []string
	optional                 bool

	input  field
	choice int
}

func (f formField) value() string {
	if f.kind == fieldChoice {
		return f.choices[f.choice]
	}
	return strings.TrimSpace(f.input.Value())
}

func text(name, label, placeholder string) formField {
	return formField{name: name, label: label, placeholder: placeholder}
}

func optional(name, label, placeholder string) formField {
	return formField{name: name, label: label, placeholder: placeholder, optional: true}
}

func secretField(name, label string, opt bool) formField {
	return formField{name: name, label: label, placeholder: "typed hidden", kind: fieldSecret, optional: opt}
}

func choice(name, label string, choices ...string) formField {
	return formField{name: name, label: label, kind: fieldChoice, choices: choices}
}

// vals are a filled-in form, by field name.
type vals map[string]string

// action is one thing the palette can do.
type action struct {
	id, group, title, about string
	fields                  []formField
	// prefill gives fields their starting values, typically the selected
	// service or user.
	prefill func(m Model) vals
	// confirm, when set, is the question asked before running: for anything
	// that deletes, replaces or ends something.
	confirm func(v vals) string
	run     func(m Model, v vals) (Model, tea.Cmd)
	// needs says whether this client can do it at all; nil is always.
	needs func(m Model) bool
	// covers names what this reaches, as "ipc:<Client method>" or
	// "cli:<command>", for the parity test.
	covers []string
}

// palette is the command palette's state, and the form it opened.
type palette struct {
	open   bool
	input  field
	cursor int
	form   *form
}

// form is an action being filled in.
type form struct {
	act        action
	fields     []formField
	at         int
	confirming bool
	err        string
}

// outputPane is a command's output, shown over the body until dismissed.
type outputPane struct {
	title  string
	text   string
	isErr  bool
	offset int
	// then, when set, is a follow-up the output offers: an add's preview
	// offers the add.
	then      func(m Model) (Model, tea.Cmd)
	thenKey   string
	thenLabel string
}

type outputMsg struct {
	title string
	text  string
	err   error
	// then and its key carry through to the pane.
	then      func(m Model) (Model, tea.Cmd)
	thenKey   string
	thenLabel string
}

// available are the actions this client can do.
func (m Model) available() []action {
	var out []action
	for _, a := range actions() {
		if a.needs == nil || a.needs(m) {
			out = append(out, a)
		}
	}
	return out
}

// matching are the available actions matching what's typed: every word must
// appear somewhere in the group, title or description.
func (m Model) matching() []action {
	q := strings.Fields(strings.ToLower(m.pal.input.Value()))
	var out []action
	for _, a := range m.available() {
		hay := strings.ToLower(a.group + " " + a.title + " " + a.about + " " + a.id)
		ok := true
		for _, w := range q {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, a)
		}
	}
	return out
}

// typing reports whether a view has a text field taking keys, so ":" is
// typed rather than opening the palette.
func (m Model) typing() bool {
	return (m.view == viewAI && m.ai.mode != aiBrowse) || (m.view == viewLogs && m.lv.searching)
}

// openPalette shows the palette, empty.
func (m Model) openPalette() Model {
	m.pal = palette{open: true, input: newInput("type to find a command", false)}
	m.confirm = ""
	return m
}

// openAction starts an action by id, as a view's shortcut key does: its
// form if it has one, else its question, else it just runs.
func (m Model) openAction(id string) (Model, tea.Cmd) {
	for _, a := range m.available() {
		if a.id == id {
			return m.begin(a)
		}
	}
	return m, nil
}

func (m Model) begin(a action) (Model, tea.Cmd) {
	m.pal.open = false
	f := &form{act: a}
	var pre vals
	if a.prefill != nil {
		pre = a.prefill(m)
	}
	for _, tmpl := range a.fields {
		fl := tmpl
		fl.input = newInput(fl.placeholder, fl.kind == fieldSecret)
		if fl.optional && fl.kind != fieldChoice && fl.placeholder == "" {
			fl.input = newInput("optional", false)
		}
		if v, ok := pre[fl.name]; ok {
			if fl.kind == fieldChoice {
				for i, c := range fl.choices {
					if c == v {
						fl.choice = i
					}
				}
			} else {
				fl.input.SetValue(v)
			}
		}
		f.fields = append(f.fields, fl)
	}
	if len(f.fields) == 0 {
		if a.confirm != nil {
			f.confirming = true
			m.pal.form = f
			return m, nil
		}
		m.pal.form = nil
		return a.run(m, vals{})
	}
	m.pal.form = f
	return m, nil
}

// handleOverlayKey gives the output pane, the form and the palette every
// key while one is up, and opens the palette on ":". ok is false when none
// of them is up and the key isn't ":".
func (m Model) handleOverlayKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	if msg.String() == "ctrl+c" {
		return m, nil, false
	}
	switch {
	case m.out != nil:
		return m.handleOutputKey(msg)
	case m.pal.form != nil:
		return m.handleFormKey(msg)
	case m.pal.open:
		return m.handlePaletteKey(msg)
	case msg.String() == ":" && !m.typing() && m.err == nil:
		return m.openPalette(), nil, true
	}
	return m, nil, false
}

func (m Model) handleOutputKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	o := *m.out
	switch msg.String() {
	case "esc", "q", "enter":
		m.out = nil
		return m, nil, true
	case "up", "k":
		o.offset = max(o.offset-1, 0)
	case "down", "j":
		o.offset++
	case "pgup", "b":
		o.offset = max(o.offset-10, 0)
	case "pgdown", " ":
		o.offset += 10
	default:
		if o.then != nil && msg.String() == o.thenKey {
			m.out = nil
			next, cmd := o.then(m)
			return next, cmd, true
		}
	}
	// Held to the last screenful, so scrolling past the end doesn't take as
	// many presses to come back from.
	lines, rows := o.layout(m.bodyHeight())
	o.offset = min(o.offset, max(len(lines)-rows, 0))
	m.out = &o
	return m, nil, true
}

func (m Model) handlePaletteKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	items := m.matching()
	switch msg.String() {
	case "esc":
		m.pal.open = false
		return m, nil, true
	case "up", "ctrl+p":
		if m.pal.cursor > 0 {
			m.pal.cursor--
		}
		return m, nil, true
	case "down", "ctrl+n", "tab":
		if m.pal.cursor < len(items)-1 {
			m.pal.cursor++
		}
		return m, nil, true
	case "enter":
		if m.pal.cursor < len(items) {
			next, cmd := m.begin(items[m.pal.cursor])
			return next, cmd, true
		}
		return m, nil, true
	}
	m.pal.input = m.pal.input.Update(msg)
	m.pal.cursor = 0
	return m, nil, true
}

func (m Model) handleFormKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	f := *m.pal.form
	f.fields = append([]formField(nil), f.fields...)
	if f.confirming {
		m.pal.form = nil
		if msg.String() == "y" || msg.String() == "Y" {
			next, cmd := f.act.run(m, f.values())
			return next, cmd, true
		}
		m.notice, m.noticeIsErr = "cancelled: "+strings.ToLower(f.act.title), false
		return m, nil, true
	}
	cur := &f.fields[f.at]
	switch msg.String() {
	case "esc":
		m.pal.form = nil
		return m, nil, true
	case "tab", "down":
		f.at = (f.at + 1) % len(f.fields)
	case "shift+tab", "up":
		f.at = (f.at + len(f.fields) - 1) % len(f.fields)
	case "left", "right", " ":
		if cur.kind == fieldChoice {
			step := 1
			if msg.String() == "left" {
				step = len(cur.choices) - 1
			}
			cur.choice = (cur.choice + step) % len(cur.choices)
		} else {
			cur.input = cur.input.Update(msg)
		}
	case "enter":
		if f.at < len(f.fields)-1 {
			f.at++
			break
		}
		for i, fl := range f.fields {
			if !fl.optional && fl.value() == "" {
				f.at, f.err = i, fl.label+" is needed"
				m.pal.form = &f
				return m, nil, true
			}
		}
		v := f.values()
		if f.act.confirm != nil {
			f.confirming, f.err = true, ""
			m.pal.form = &f
			return m, nil, true
		}
		m.pal.form = nil
		next, cmd := f.act.run(m, v)
		return next, cmd, true
	default:
		if cur.kind != fieldChoice {
			cur.input = cur.input.Update(msg)
		}
	}
	f.err = ""
	m.pal.form = &f
	return m, nil, true
}

func (f form) values() vals {
	v := vals{}
	for _, fl := range f.fields {
		v[fl.name] = fl.value()
	}
	return v
}

// overlay draws whatever is up over the body, in height rows; "" for none.
func (m Model) overlay(height int) string {
	switch {
	case m.out != nil:
		return m.outputView(height)
	case m.pal.form != nil:
		return m.formView()
	case m.pal.open:
		return m.paletteView(height)
	}
	return ""
}

func (m Model) paletteView(height int) string {
	var b strings.Builder
	b.WriteString("  " + m.rule("commands") + "\n")
	b.WriteString("  " + m.pal.input.View() + "\n\n")
	items := m.matching()
	if len(items) == 0 {
		b.WriteString("  " + labelStyle.Render("nothing matches") + "\n")
		return b.String()
	}
	rows := max(height-4, 1)
	if len(items) > rows {
		rows = max(rows-1, 1) // for the count below
	}
	start := 0
	if m.pal.cursor >= rows {
		start = m.pal.cursor - rows + 1
	}
	end := min(start+rows, len(items))
	groupW, titleW := 0, 0
	for _, a := range items {
		groupW = max(groupW, lipgloss.Width(a.group))
		titleW = max(titleW, lipgloss.Width(a.title))
	}
	for i := start; i < end; i++ {
		a := items[i]
		marker, style := "  ", rowStyle
		if i == m.pal.cursor {
			marker, style = "▸ ", selectedRow
		}
		line := marker + labelStyle.Render(pad(a.group, groupW)) + "  " + style.Render(pad(a.title, titleW)) + "  " + labelStyle.Render(a.about)
		b.WriteString("  " + lipgloss.NewStyle().MaxWidth(max(m.width-4, 1)).Render(line) + "\n")
	}
	if len(items) > rows {
		b.WriteString("  " + countStyle.Render(fmt.Sprintf("%d of %d", end-start, len(items))) + "\n")
	}
	return b.String()
}

func (m Model) formView() string {
	f := m.pal.form
	var b strings.Builder
	b.WriteString("  " + m.rule(f.act.title) + "\n")
	if f.act.about != "" {
		b.WriteString("  " + labelStyle.Render(truncate(f.act.about, m.width-4)) + "\n")
	}
	b.WriteString("\n")
	if f.confirming {
		q := "Go ahead?"
		if f.act.confirm != nil {
			q = f.act.confirm(f.values())
		}
		for _, line := range strings.Split(q, "\n") {
			b.WriteString("  " + errorStyle.Render(truncate(line, m.width-4)) + "\n")
		}
		b.WriteString("\n  " + valueStyle.Render("y to go ahead, any other key to cancel") + "\n")
		return b.String()
	}
	labelW := 0
	for _, fl := range f.fields {
		labelW = max(labelW, lipgloss.Width(fl.label))
	}
	for i, fl := range f.fields {
		marker := "  "
		lstyle := labelStyle
		if i == f.at {
			marker, lstyle = "▸ ", selectedRow
		}
		var v string
		switch {
		case fl.kind == fieldChoice:
			v = "‹ " + fl.value() + " ›"
		case i == f.at:
			v = fl.input.View()
		case fl.input.Value() == "":
			v = labelStyle.Render(fl.input.placeholder)
		case fl.kind == fieldSecret:
			v = strings.Repeat("•", min(len([]rune(fl.input.Value())), 32))
		default:
			v = fl.input.Value()
		}
		line := marker + lstyle.Render(pad(fl.label, labelW)) + "  " + v
		b.WriteString("  " + lipgloss.NewStyle().MaxWidth(max(m.width-4, 1)).Render(line) + "\n")
	}
	if f.err != "" {
		b.WriteString("\n  " + errorStyle.Render(f.err) + "\n")
	}
	return b.String()
}

// layout is the pane's lines, and how many of them fit in height rows.
func (o outputPane) layout(height int) ([]string, int) {
	lines := strings.Split(strings.TrimRight(plainText(o.text), "\n"), "\n")
	if strings.TrimSpace(o.text) == "" {
		lines = []string{"(no output)"}
	}
	rows := max(height-2, 1)
	if o.then != nil {
		rows = max(rows-2, 1)
	}
	return lines, rows
}

func (m Model) outputView(height int) string {
	o := m.out
	var b strings.Builder
	b.WriteString("  " + m.rule(o.title) + "\n")
	lines, rows := o.layout(height)
	off := min(o.offset, max(len(lines)-rows, 0))
	style := valueStyle
	if o.isErr {
		style = errorStyle
	}
	for _, line := range lines[off:min(off+rows, len(lines))] {
		b.WriteString("  " + style.Render(truncate(line, m.width-4)) + "\n")
	}
	if len(lines) > rows {
		b.WriteString("  " + countStyle.Render(fmt.Sprintf("lines %d–%d of %d", off+1, min(off+rows, len(lines)), len(lines))) + "\n")
	}
	if o.then != nil {
		b.WriteString("\n  " + noticeStyle.Render(o.thenKey+" "+o.thenLabel) + "\n")
	}
	return b.String()
}

// overlayHelp is the key help while an overlay is up.
func (m Model) overlayHelp(key func(k, what string) string, sep string) string {
	switch {
	case m.out != nil:
		return strings.Join([]string{key("↑/↓", "scroll"), key("esc", "close")}, sep)
	case m.pal.form != nil && m.pal.form.confirming:
		return strings.Join([]string{key("y", "go ahead"), key("any key", "cancel")}, sep)
	case m.pal.form != nil:
		return strings.Join([]string{key("enter", "next/run"), key("tab", "move"), key("←/→", "choose"), key("esc", "cancel")}, sep)
	case m.pal.open:
		return strings.Join([]string{key("enter", "choose"), key("↑/↓", "move"), key("esc", "close")}, sep)
	}
	return ""
}

// overlayUp reports whether an overlay is drawn over the body.
func (m Model) overlayUp() bool { return m.out != nil || m.pal.form != nil || m.pal.open }
