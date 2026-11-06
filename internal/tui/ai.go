package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ai"
)

// AIExtras is the daemon API the AI view needs: the models apps are given,
// the providers they come from, and which apps may use each. *ipc.Client
// satisfies it. A client without it gets no AI view.
//
// The socket is the operator's, as `outpost ai` is, so everything the
// portal's AI models page lets an admin do can be done here.
type AIExtras interface {
	AIStatus() (ai.Overview, error)
	AISetFrom(kind, provider, model string) (string, error)
	AIReset(kind string) (string, error)
	AIApps(kind string, every bool, apps []string) (string, error)
	AIAppsReset(kind string) (string, error)
	AIKey(provider, value string) (string, error)
	AITest(kind string) (ai.TestResult, error)
}

// aiMode is what the AI view is doing with the keyboard.
type aiMode int

const (
	aiBrowse aiMode = iota // moving between the rows
	aiPick                 // choosing a model, typing to filter
	aiApps                 // choosing the apps
	aiKey                  // typing a provider's key
)

// aiState is the AI view's.
type aiState struct {
	overview *ai.Overview
	err      error
	cursor   int // over the kinds, then the hosted providers
	mode     aiMode

	input field
	kind  string // the kind being picked for, or given apps

	pickCursor int

	appsCursor int // 0 is "every app"; then the services
	appsEvery  bool
	appsSel    map[string]bool

	keyProvider string
}

// aiPickItem is one model the picker offers.
type aiPickItem struct {
	provider, model, note string
	// custom is a model typed in, for a provider whose list isn't complete.
	custom bool
}

type aiMsg struct {
	overview *ai.Overview
	err      error
}

// aiDoneMsg is the end of a change or a test: what to say, and whether to
// read the settings again.
type aiDoneMsg struct {
	message string
	err     error
}

func (m Model) aiClient() (AIExtras, bool) {
	x, ok := m.client.(AIExtras)
	return x, ok
}

// fetchAI reads the AI settings and what each provider has. It isn't done
// on every tick, since it lists Ollama's models and perhaps a provider's:
// on opening the view, after a change, and on R.
func (m Model) fetchAI() tea.Cmd {
	x, ok := m.aiClient()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		o, err := x.AIStatus()
		if err != nil {
			return aiMsg{err: err}
		}
		return aiMsg{overview: &o}
	}
}

func (m Model) aiDo(f func(AIExtras) (string, error)) tea.Cmd {
	x, ok := m.aiClient()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		msg, err := f(x)
		return aiDoneMsg{message: msg, err: err}
	}
}

// aiRows are what the cursor moves over: the two kinds, then the hosted
// providers, whose keys can be set.
func (m Model) aiRows() (kinds []ai.Choice, providers []ai.ProviderStatus) {
	if m.ai.overview == nil {
		return nil, nil
	}
	kinds = m.ai.overview.Choices
	for _, p := range m.ai.overview.Providers {
		if p.Keyed {
			providers = append(providers, p)
		}
	}
	return kinds, providers
}

// updateAI handles the AI view's messages; ok is false for any other.
func (m Model) updateAI(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case aiMsg:
		if msg.err != nil {
			m.ai.err = msg.err
			return m, nil, true
		}
		m.ai.overview, m.ai.err = msg.overview, nil
		kinds, provs := m.aiRows()
		if m.ai.cursor >= len(kinds)+len(provs) {
			m.ai.cursor = 0
		}
		return m, nil, true
	case aiDoneMsg:
		if msg.err != nil {
			m.notice, m.noticeIsErr = msg.err.Error(), true
		} else {
			m.notice, m.noticeIsErr = msg.message, false
		}
		return m, m.fetchAI(), true
	}
	return m, nil, false
}

// handleAIKey handles a key on the AI view. While a picker or the key field
// is open, every key is theirs: a q typed into a filter is a letter, not
// quit, and esc closes the picker rather than the dashboard.
func (m Model) handleAIKey(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch m.ai.mode {
	case aiPick:
		return m.handlePick(msg)
	case aiApps:
		return m.handleApps(msg)
	case aiKey:
		return m.handleKeyEntry(msg)
	}
	kinds, provs := m.aiRows()
	n := len(kinds) + len(provs)
	onKind := m.ai.cursor < len(kinds)
	switch msg.String() {
	case "up", "k":
		if m.ai.cursor > 0 {
			m.ai.cursor--
		}
		return m, nil, true
	case "down", "j":
		if m.ai.cursor < n-1 {
			m.ai.cursor++
		}
		return m, nil, true
	case "R":
		m.notice = ""
		return m, tea.Batch(m.fetchServices(), m.refreshExtras(), m.fetchAI()), true
	case "enter", "c":
		if n == 0 {
			return m, nil, true
		}
		if onKind {
			m.ai.mode, m.ai.kind, m.ai.pickCursor = aiPick, kinds[m.ai.cursor].Kind, 0
			m.ai.input = newInput("type to filter, or a model ID", false)
		} else {
			m.ai.mode, m.ai.keyProvider = aiKey, provs[m.ai.cursor-len(kinds)].Name
			m.ai.input = newInput("paste the key", true)
		}
		m.notice = ""
		return m, nil, true
	case "a":
		if !onKind || n == 0 {
			return m, nil, true
		}
		c := kinds[m.ai.cursor]
		m.ai.mode, m.ai.kind, m.ai.appsCursor = aiApps, c.Kind, 0
		m.ai.appsEvery = c.Every
		m.ai.appsSel = map[string]bool{}
		for _, a := range c.Apps {
			m.ai.appsSel[a] = true
		}
		m.notice = ""
		return m, nil, true
	case "t":
		if !onKind || n == 0 {
			return m, nil, true
		}
		kind := kinds[m.ai.cursor].Kind
		m.notice, m.noticeIsErr = "asking the "+ai.KindName(kind)+"…", false
		return m, m.aiDo(func(x AIExtras) (string, error) {
			r, err := x.AITest(kind)
			if err != nil {
				return "", err
			}
			reply := r.Reply
			if reply == "" {
				reply = "(an empty reply)"
			}
			return fmt.Sprintf("%s answered in %s: %s", r.Model, r.Took.Round(100*time.Millisecond), reply), nil
		}), true
	case "u":
		if !onKind || n == 0 {
			return m, nil, true
		}
		c := kinds[m.ai.cursor]
		switch {
		case c.FromPortal:
			return m, m.aiDo(func(x AIExtras) (string, error) { return x.AIReset(c.Kind) }), true
		case c.AppsFromPortal:
			return m, m.aiDo(func(x AIExtras) (string, error) { return x.AIAppsReset(c.Kind) }), true
		}
		m.notice, m.noticeIsErr = "the "+ai.KindName(c.Kind)+" is already config.toml's", false
		return m, nil, true
	case "s", "x", "r", "l":
		// The services view's actions mean nothing here.
		return m, nil, true
	}
	return m, nil, false
}

// pickItems are the models the picker offers for its kind, every provider's
// that serves it, filtered by what's typed.
func (m Model) pickItems() (items []aiPickItem, unlisted []string) {
	if m.ai.overview == nil {
		return nil, nil
	}
	filter := strings.ToLower(strings.TrimSpace(m.ai.input.Value()))
	keyless := map[string]bool{}
	for _, p := range m.ai.overview.Providers {
		keyless[p.Name] = p.Keyed && !p.HasKey
	}
	for _, cat := range m.ai.overview.Catalogs {
		if cat.Kind != m.ai.kind {
			continue
		}
		title := ai.ProviderTitle(cat.Provider)
		if keyless[cat.Provider] {
			unlisted = append(unlisted, title+" (no key)")
			continue
		}
		if cat.Error != "" && len(cat.Models) == 0 {
			unlisted = append(unlisted, title+" ("+cat.Error+")")
			continue
		}
		exact := false
		for _, mo := range cat.Models {
			hay := strings.ToLower(cat.Provider + " " + mo.Name + " " + mo.Note)
			if filter == "" || strings.Contains(hay, filter) {
				items = append(items, aiPickItem{provider: cat.Provider, model: mo.Name, note: mo.Note})
			}
			exact = exact || strings.EqualFold(mo.Name, filter)
		}
		// A provider without a complete list takes any model ID typed in.
		if !cat.Complete && filter != "" && !exact && !strings.ContainsAny(filter, " \t") {
			items = append(items, aiPickItem{provider: cat.Provider, model: strings.TrimSpace(m.ai.input.Value()), custom: true})
		}
	}
	return items, unlisted
}

func (m Model) handlePick(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.ai.mode = aiBrowse
		return m, nil, true
	case "up", "ctrl+p":
		if m.ai.pickCursor > 0 {
			m.ai.pickCursor--
		}
		return m, nil, true
	case "down", "ctrl+n":
		if items, _ := m.pickItems(); m.ai.pickCursor < len(items)-1 {
			m.ai.pickCursor++
		}
		return m, nil, true
	case "enter":
		items, _ := m.pickItems()
		if len(items) == 0 {
			return m, nil, true
		}
		it := items[min(m.ai.pickCursor, len(items)-1)]
		kind := m.ai.kind
		m.ai.mode = aiBrowse
		m.notice, m.noticeIsErr = "choosing "+it.model+"…", false
		return m, m.aiDo(func(x AIExtras) (string, error) { return x.AISetFrom(kind, it.provider, it.model) }), true
	}
	m.ai.input = m.ai.input.Update(msg)
	m.ai.pickCursor = 0
	return m, nil, true
}

func (m Model) handleApps(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	n := len(m.services) + 1
	switch msg.String() {
	case "esc", "ctrl+c", "q":
		m.ai.mode = aiBrowse
		return m, nil, true
	case "up", "k":
		if m.ai.appsCursor > 0 {
			m.ai.appsCursor--
		}
	case "down", "j":
		if m.ai.appsCursor < n-1 {
			m.ai.appsCursor++
		}
	case " ", "x":
		if m.ai.appsCursor == 0 {
			m.ai.appsEvery = !m.ai.appsEvery
		} else if s := m.services[m.ai.appsCursor-1].Name; s != "" {
			m.ai.appsSel[s] = !m.ai.appsSel[s]
		}
	case "enter":
		kind, every := m.ai.kind, m.ai.appsEvery
		var apps []string
		for _, s := range m.services {
			if m.ai.appsSel[s.Name] {
				apps = append(apps, s.Name)
			}
		}
		m.ai.mode = aiBrowse
		return m, m.aiDo(func(x AIExtras) (string, error) { return x.AIApps(kind, every, apps) }), true
	}
	return m, nil, true
}

func (m Model) handleKeyEntry(msg tea.KeyMsg) (Model, tea.Cmd, bool) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.ai.mode = aiBrowse
		m.ai.input.SetValue("")
		return m, nil, true
	case "enter":
		provider, value := m.ai.keyProvider, m.ai.input.Value()
		m.ai.input.SetValue("") // not kept once it's been sent
		m.ai.mode = aiBrowse
		if strings.TrimSpace(value) == "" {
			m.notice, m.noticeIsErr = "no key entered; nothing saved", true
			return m, nil, true
		}
		m.notice, m.noticeIsErr = "saving "+ai.ProviderTitle(provider)+"'s key…", false
		return m, m.aiDo(func(x AIExtras) (string, error) { return x.AIKey(provider, value) }), true
	}
	m.ai.input = m.ai.input.Update(msg)
	return m, nil, true
}

// aiPane is the AI view.
func (m Model) aiPane() string {
	var b strings.Builder
	line := func(s string) { b.WriteString("  " + truncate(s, m.width-4) + "\n") }
	switch {
	case m.ai.err != nil:
		line(errorStyle.Render(m.ai.err.Error()))
		return b.String()
	case m.ai.overview == nil:
		line(labelStyle.Render("loading…"))
		return b.String()
	}
	switch m.ai.mode {
	case aiPick:
		return m.pickPane()
	case aiApps:
		return m.appsPane()
	case aiKey:
		title := ai.ProviderTitle(m.ai.keyProvider)
		line(sectionStyle.Render(title + "'s API key"))
		line(labelStyle.Render("Kept as a secret and given to no app; it can be replaced, never shown."))
		b.WriteString("\n  " + m.ai.input.View() + "\n")
		return b.String()
	}

	kinds, provs := m.aiRows()
	row := func(i int, label, value, note string) {
		marker, style := "  ", rowStyle
		if i == m.ai.cursor {
			marker, style = "▸ ", selectedRow
		}
		text := style.Render(marker+pad(label, 12)+value) + "  " + labelStyle.Render(note)
		b.WriteString("  " + truncate(text, m.width-4) + "\n")
	}
	for i, c := range kinds {
		model := c.Model
		if model == "" {
			model = "none chosen"
		}
		row(i, ai.KindName(c.Kind), model, strings.Join(choiceNotes(c), " · "))
		if c.Missing || c.NoKey {
			what := ai.ProviderTitle(c.Provider) + " doesn't list it"
			if c.NoKey {
				what = "no API key for " + ai.ProviderTitle(c.Provider)
			}
			line("    " + errorStyle.Render(what+"; apps asking for it get an error"))
		}
	}
	b.WriteString("\n  " + m.rule("providers") + "\n")
	for i, p := range provs {
		key := "no key"
		if p.HasKey {
			key = "key set"
		}
		row(len(kinds)+i, p.Title, key, "serves "+strings.Join(p.Kinds, ", ")+" · secret "+p.KeySecret)
	}
	if u := m.ai.overview.Usage; len(u) > 0 {
		b.WriteString("\n  " + m.rule("since the daemon started") + "\n")
		for _, us := range u {
			s := fmt.Sprintf("%s  %s  %d requests, %d failed, %d tokens in, %d out",
				pad(us.Service, 12), pad(us.Kind, 8), us.Requests, us.Errors, us.InputTokens, us.OutputTokens)
			line(valueStyle.Render(s))
		}
	}
	return b.String()
}

// choiceNotes are what to say about a kind's choice, after its model.
func choiceNotes(c ai.Choice) []string {
	var notes []string
	if c.Model != "" {
		notes = append(notes, ai.ProviderTitle(c.Provider))
		if c.FromPortal {
			notes = append(notes, "chosen by "+c.UpdatedBy)
		} else {
			notes = append(notes, "config.toml")
		}
	}
	switch {
	case c.Every:
		notes = append(notes, "every app")
	case len(c.Apps) == 0:
		notes = append(notes, "no app")
	default:
		notes = append(notes, "only "+strings.Join(c.Apps, ", "))
	}
	if c.Kind == ai.LLM && len(c.APIs) == 1 {
		notes = append(notes, "apps must call "+c.APIs[0])
	}
	return notes
}

func (m Model) pickPane() string {
	var b strings.Builder
	line := func(s string) { b.WriteString("  " + truncate(s, m.width-4) + "\n") }
	line(sectionStyle.Render("choose the " + ai.KindName(m.ai.kind)))
	b.WriteString("  " + m.ai.input.View() + "\n\n")
	items, unlisted := m.pickItems()
	// Enough rows to see a choice in context, not all of OpenRouter's.
	const show = 12
	start := 0
	if m.ai.pickCursor >= show {
		start = m.ai.pickCursor - show + 1
	}
	for i := start; i < len(items) && i < start+show; i++ {
		it := items[i]
		marker, style := "  ", rowStyle
		if i == m.ai.pickCursor {
			marker, style = "▸ ", selectedRow
		}
		name := it.model
		if it.custom {
			name = "use " + it.model + " as typed"
		}
		text := style.Render(marker+pad(ai.ProviderTitle(it.provider), 11)+name) + "  " + labelStyle.Render(it.note)
		b.WriteString("  " + truncate(text, m.width-4) + "\n")
	}
	switch {
	case len(items) == 0:
		line(labelStyle.Render("nothing matches"))
	case len(items) > show:
		line(labelStyle.Render(fmt.Sprintf("%d of %d; keep typing to narrow it", min(show, len(items)-start), len(items))))
	}
	if len(unlisted) > 0 {
		line(labelStyle.Render("not listed: " + strings.Join(unlisted, ", ")))
	}
	return b.String()
}

func (m Model) appsPane() string {
	var b strings.Builder
	line := func(s string) { b.WriteString("  " + truncate(s, m.width-4) + "\n") }
	line(sectionStyle.Render("who may use the " + ai.KindName(m.ai.kind)))
	line(labelStyle.Render("Hosted models cost money per request; an app left out gets a 403."))
	b.WriteString("\n")
	box := func(on bool) string {
		if on {
			return "[x] "
		}
		return "[ ] "
	}
	opt := func(i int, on bool, label string) {
		marker, style := "  ", rowStyle
		if i == m.ai.appsCursor {
			marker, style = "▸ ", selectedRow
		}
		line(style.Render(marker + box(on) + label))
	}
	opt(0, m.ai.appsEvery, "every app, including ones added later")
	for i, s := range m.services {
		label := s.Name
		if m.ai.appsEvery {
			label += labelStyle.Render("  (every app has it)")
		}
		opt(i+1, m.ai.appsSel[s.Name], label)
	}
	return b.String()
}

// aiHelp is the key help on the AI view, for what it's doing.
func (m Model) aiHelp(key func(k, what string) string, sep string) string {
	switch m.ai.mode {
	case aiPick:
		return strings.Join([]string{key("type", "filter"), key("↑/↓", "move"), key("enter", "choose"), key("esc", "cancel")}, sep)
	case aiApps:
		return strings.Join([]string{key("↑/↓", "move"), key("space", "toggle"), key("enter", "save"), key("esc", "cancel")}, sep)
	case aiKey:
		return strings.Join([]string{key("enter", "save"), key("esc", "cancel")}, sep)
	}
	kinds, _ := m.aiRows()
	if m.ai.cursor < len(kinds) {
		return strings.Join([]string{key("↑/↓", "move"), key("enter", "choose"), key("a", "apps"), key("t", "test"),
			key("u", "reset"), key("tab", "view"), key("q", "quit")}, sep)
	}
	return strings.Join([]string{key("↑/↓", "move"), key("enter", "set key"), key("tab", "view"), key("R", "refresh"), key("q", "quit")}, sep)
}
