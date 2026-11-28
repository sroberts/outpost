package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
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

// TerminalRunner prepares an outpost subcommand to run on the terminal
// itself, for the few that can stop to ask something there: restarting the
// daemon, directly or by updating Outpost, can run launchctl or systemctl
// under sudo. The dashboard steps aside while it runs (tea.Exec) and puts
// what it printed, which output returns once it has run, in the output pane.
type TerminalRunner func(args ...string) (cmd tea.ExecCommand, output func() string)

// WithTerminal lets the dashboard hand the terminal to the commands that
// may need it. Without one, they run through the Runner like the rest.
func (m Model) WithTerminal(t TerminalRunner) Model {
	m.terminal = t
	return m
}

// inTerminal runs an outpost subcommand with the terminal handed to it, or
// through the Runner when there's no TerminalRunner.
func (m Model) inTerminal(title string, args ...string) tea.Cmd {
	if m.terminal == nil {
		return m.cli(title, args...)
	}
	cmd, output := m.terminal(args...)
	return tea.Exec(cmd, func(err error) tea.Msg {
		return outputMsg{title: title, text: output(), err: err}
	})
}

// fieldKind is how a form field takes input.
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldSecret
	fieldChoice
)

// formField is one input on an action's form. The form itself is a huh
// form built from these when the action opens.
type formField struct {
	name, label, placeholder string
	kind                     fieldKind
	choices                  []string
	optional                 bool
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

// form is an action being filled in: a huh form over the action's fields,
// then, for anything that can't be undone, a question of its own.
//
// The question stays outside huh on purpose: it is answered with y and
// cancelled by any other key, so a stray enter can never confirm a delete,
// where a huh Confirm would take enter for whichever button is lit.
type form struct {
	act        action
	values     map[string]*string // what each huh field writes to
	hf         *huh.Form
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

// formMsg carries a message one of the form's own commands produced back
// to the form.
type formMsg struct{ msg tea.Msg }

// formCmdWait is how long a form's command is given to produce its
// message. huh moves between fields with commands that answer at once;
// the only ones that take longer are the cursor's blink timers, and those
// are dropped, so the cursor stays lit rather than blinking. Dropping them
// also keeps a form from scheduling work forever after it's gone.
const formCmdWait = 100 * time.Millisecond

// formCmd runs a command the form returned, handing its message back as a
// formMsg; a batch has each of its commands handled the same way.
func formCmd(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		got := make(chan tea.Msg, 1)
		go func() { got <- cmd() }()
		var msg tea.Msg
		select {
		case msg = <-got:
		case <-time.After(formCmdWait):
			return nil
		}
		switch msg := msg.(type) {
		case nil:
			return nil
		case tea.BatchMsg:
			out := make(tea.BatchMsg, 0, len(msg))
			for _, c := range msg {
				out = append(out, formCmd(c))
			}
			return out
		}
		return formMsg{msg}
	}
}

// formTheme is huh's base theme in the dashboard's colours, with fields
// a line apart so a long form still fits a panel.
func formTheme() *huh.Theme {
	t := huh.ThemeBase()
	t.FieldSeparator = lipgloss.NewStyle().SetString("\n")
	t.Focused.Base = t.Focused.Base.BorderForeground(colAccent)
	t.Focused.Title = t.Focused.Title.Foreground(colAccent).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(colDim)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(colErr)
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(colErr)
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(colAccent)
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(colAccent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(colAccent)
	t.Focused.Option = t.Focused.Option.Foreground(colText)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(colAccent)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(colAccent)
	t.Focused.TextInput.Placeholder = t.Focused.TextInput.Placeholder.Foreground(colDim)
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(colAccent)
	t.Blurred = t.Focused
	t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Title = t.Blurred.Title.Foreground(colDim).Bold(false)
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()
	return t
}

// formKeys are huh's keys, plus space and the arrows to step through a
// choice as the dashboard's forms always have, and without ctrl+c, which
// quits the dashboard before the form sees it.
func formKeys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	k.Quit = key.NewBinding(key.WithDisabled())
	k.Select.Left = key.NewBinding(key.WithKeys("left", "h"))
	k.Select.Right = key.NewBinding(key.WithKeys("right", "l", " "))
	k.Input.Next = key.NewBinding(key.WithKeys("enter", "tab", "down"))
	k.Input.Prev = key.NewBinding(key.WithKeys("shift+tab", "up"))
	k.Select.Next = key.NewBinding(key.WithKeys("enter", "tab", "down"))
	k.Select.Prev = key.NewBinding(key.WithKeys("shift+tab", "up"))
	return k
}

// suggestions are what a field can be completed to with ctrl+e: the names
// it's asking for, from what the dashboard already knows.
func (m Model) suggestions(fl formField) []string {
	var out []string
	switch fl.name {
	case "service":
		for _, s := range m.services {
			out = append(out, s.Name)
		}
	case "user":
		for _, u := range m.users {
			out = append(out, u.Username)
		}
	}
	return out
}

// build makes the huh form for f's action, width cells wide, its fields
// holding f's values.
func (m Model) build(f *form, width int) {
	// Inline inputs put the value right after the label, so the labels are
	// padded to one width to line the values up.
	labelW := 0
	for _, fl := range f.act.fields {
		labelW = max(labelW, lipgloss.Width(fl.label))
	}
	var fields []huh.Field
	for _, fl := range f.act.fields {
		p := f.values[fl.name]
		if fl.kind == fieldChoice {
			fields = append(fields, huh.NewSelect[string]().
				Key(fl.name).Title(fl.label).Inline(true).
				Options(huh.NewOptions(fl.choices...)...).Value(p))
			continue
		}
		placeholder := fl.placeholder
		if fl.optional && placeholder == "" {
			placeholder = "optional"
		}
		in := huh.NewInput().Key(fl.name).Title(pad(fl.label, labelW)).Inline(true).
			Prompt(" › ").Placeholder(placeholder).CharLimit(4096).Value(p)
		if fl.kind == fieldSecret {
			// Shown as dots, never the text: the screen may be shared.
			in = in.EchoMode(huh.EchoModePassword)
		}
		if s := m.suggestions(fl); len(s) > 0 {
			in = in.Suggestions(s)
		}
		if !fl.optional {
			label := fl.label
			in = in.Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("%s is needed", label)
				}
				return nil
			})
		}
		fields = append(fields, in)
	}
	f.hf = huh.NewForm(huh.NewGroup(fields...)).
		WithTheme(formTheme()).WithKeyMap(formKeys()).
		WithShowHelp(false).WithShowErrors(true).
		WithWidth(max(width, 20))
	// Init focuses the first field. What it returns only sizes the form to
	// the window, which WithWidth has done, and starts the cursor blinking.
	_ = f.hf.Init()
}

// values is the form filled in, text trimmed.
func (f form) vals() vals {
	v := vals{}
	for _, fl := range f.act.fields {
		s := ""
		if p := f.values[fl.name]; p != nil {
			s = *p
		}
		if fl.kind != fieldChoice {
			s = strings.TrimSpace(s)
		}
		v[fl.name] = s
	}
	return v
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

// formWidth is the room a form has inside the overlay's panel.
func (m Model) formWidth() int { return boxInner(min(m.width, 120)) }

func (m Model) begin(a action) (Model, tea.Cmd) {
	m.pal.open = false
	f := &form{act: a, values: map[string]*string{}}
	var pre vals
	if a.prefill != nil {
		pre = a.prefill(m)
	}
	for _, fl := range a.fields {
		v := pre[fl.name]
		if fl.kind == fieldChoice && !slices.Contains(fl.choices, v) {
			v = fl.choices[0]
		}
		f.values[fl.name] = &v
	}
	if len(a.fields) == 0 {
		if a.confirm != nil {
			f.confirming = true
			m.pal.form = f
			return m, nil
		}
		m.pal.form = nil
		return a.run(m, vals{})
	}
	m.build(f, m.formWidth())
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
	case "home", "g":
		o.offset = 0
	case "end", "G":
		o.offset = 1 << 30
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
	if f.confirming {
		m.pal.form = nil
		if msg.String() == "y" || msg.String() == "Y" {
			next, cmd := f.act.run(m, f.vals())
			return next, cmd, true
		}
		m.notice, m.noticeIsErr = "cancelled: "+strings.ToLower(f.act.title), false
		return m, nil, true
	}
	if msg.String() == "esc" {
		m.pal.form = nil
		return m, nil, true
	}
	next, cmd := m.stepForm(msg)
	return next, cmd, true
}

// stepForm hands a message to the huh form, and once the form is filled
// in, asks the action's question or runs it.
func (m Model) stepForm(msg tea.Msg) (Model, tea.Cmd) {
	f := *m.pal.form
	f.err = ""
	next, cmd := f.hf.Update(msg)
	f.hf = next.(*huh.Form)
	if f.hf.State != huh.StateCompleted {
		m.pal.form = &f
		return m, formCmd(cmd)
	}
	v := f.vals()
	// huh checks a field as it's left; one never visited can still be empty.
	for _, fl := range f.act.fields {
		if !fl.optional && fl.kind != fieldChoice && v[fl.name] == "" {
			f.err = fl.label + " is needed"
			m.build(&f, m.formWidth())
			m.pal.form = &f
			return m, nil
		}
	}
	if f.act.confirm != nil {
		f.confirming = true
		m.pal.form = &f
		return m, nil
	}
	m.pal.form = nil
	return f.act.run(m, v)
}

// paletteLines is the palette inside its panel, rows tall, and a note
// saying how many of the matches are shown.
func (m Model) paletteLines(rows int) ([]string, string) {
	out := []string{m.pal.input.View(), ""}
	items := m.matching()
	if len(items) == 0 {
		return append(out, labelStyle.Render("nothing matches")), ""
	}
	room := max(rows-len(out), 1)
	start := 0
	if m.pal.cursor >= room {
		start = m.pal.cursor - room + 1
	}
	end := min(start+room, len(items))
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
		out = append(out, marker+labelStyle.Render(pad(a.group, groupW))+"  "+style.Render(pad(a.title, titleW))+"  "+labelStyle.Render(a.about))
	}
	note := ""
	if len(items) > room {
		note = fmt.Sprintf("%d–%d of %d", start+1, end, len(items))
	}
	return out, note
}

// formLines is the form inside its panel: what the action does, then its
// fields, or once they're filled in, the question.
func (m Model) formLines() []string {
	f := m.pal.form
	inner := boxInner(m.width)
	var out []string
	if f.act.about != "" {
		out = append(out, labelStyle.Render(truncate(f.act.about, inner)), "")
	}
	if f.confirming {
		q := "Go ahead?"
		if f.act.confirm != nil {
			q = f.act.confirm(f.vals())
		}
		for _, line := range strings.Split(q, "\n") {
			out = append(out, errorStyle.Render(truncate(line, inner)))
		}
		return append(out, "", valueStyle.Render("y to go ahead, any other key to cancel"))
	}
	out = append(out, strings.Split(strings.TrimRight(f.hf.View(), "\n"), "\n")...)
	if f.err != "" {
		out = append(out, "", errorStyle.Render(f.err))
	}
	return out
}

// layout is the pane's lines, and how many of them fit in height rows.
func (o outputPane) layout(height int) ([]string, int) {
	lines := strings.Split(strings.TrimRight(plainText(o.text), "\n"), "\n")
	if strings.TrimSpace(o.text) == "" {
		lines = []string{"(no output)"}
	}
	rows := max(height, 1)
	if o.then != nil {
		rows = max(rows-2, 1)
	}
	return lines, rows
}

// outputLines is a command's output inside its panel, height rows, and a
// note saying which of its lines are on screen.
func (m Model) outputLines(height int) ([]string, string) {
	o := m.out
	lines, rows := o.layout(height)
	off := min(o.offset, max(len(lines)-rows, 0))
	style := valueStyle
	if o.isErr {
		style = errorStyle
	}
	var out []string
	for _, line := range lines[off:min(off+rows, len(lines))] {
		out = append(out, style.Render(truncate(line, boxInner(m.width))))
	}
	note := ""
	if len(lines) > rows {
		note = fmt.Sprintf("lines %d–%d of %d", off+1, min(off+rows, len(lines)), len(lines))
	}
	if o.then != nil {
		for len(out) < rows {
			out = append(out, "")
		}
		out = append(out, "", noticeStyle.Render(o.thenKey+" "+o.thenLabel))
	}
	return out, note
}

// overlayHelp is the key help while an overlay is up.
func (m Model) overlayHelp(key func(k, what string) string, sep string) string {
	switch {
	case m.out != nil:
		return strings.Join([]string{key("↑/↓", "scroll"), key("g/G", "top/end"), key("esc", "close")}, sep)
	case m.pal.form != nil && m.pal.form.confirming:
		return strings.Join([]string{key("y", "go ahead"), key("any key", "cancel")}, sep)
	case m.pal.form != nil:
		return strings.Join([]string{key("enter", "next/run"), key("tab", "move"), key("←/→", "choose"), key("ctrl+e", "complete"), key("esc", "cancel")}, sep)
	case m.pal.open:
		return strings.Join([]string{key("enter", "choose"), key("↑/↓", "move"), key("esc", "close")}, sep)
	}
	return ""
}

// overlayUp reports whether an overlay is drawn over the body.
func (m Model) overlayUp() bool { return m.out != nil || m.pal.form != nil || m.pal.open }
