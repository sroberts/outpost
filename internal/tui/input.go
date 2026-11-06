package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// field is a one-line text input: typing, pasting, backspace, ctrl+u to
// clear. bubbles' textinput would do, but brings a clipboard dependency for
// a feature this doesn't use.
type field struct {
	value       []rune
	placeholder string
	secret      bool
	limit       int
}

func newInput(placeholder string, secret bool) field {
	return field{placeholder: placeholder, secret: secret, limit: 4096}
}

// Value is what's been typed.
func (f field) Value() string { return string(f.value) }

// SetValue replaces it.
func (f *field) SetValue(s string) { f.value = []rune(s) }

// Update takes one key.
func (f field) Update(msg tea.KeyMsg) field {
	switch msg.Type {
	case tea.KeyBackspace:
		if len(f.value) > 0 {
			f.value = f.value[:len(f.value)-1]
		}
	case tea.KeyCtrlU:
		f.value = nil
	case tea.KeySpace:
		f.add([]rune{' '})
	case tea.KeyRunes:
		f.add(msg.Runes)
	}
	return f
}

func (f *field) add(rs []rune) {
	for _, r := range rs {
		// A paste can carry a newline or a tab; neither belongs in a model
		// name or a key.
		if unicode.IsControl(r) || len(f.value) >= f.limit {
			continue
		}
		f.value = append(f.value, r)
	}
}

// View draws it. A secret shows a count of what's typed, never the text, and
// no more than a line's worth.
func (f field) View() string {
	if len(f.value) == 0 {
		return "> " + labelStyle.Render(f.placeholder) + "█"
	}
	if f.secret {
		return "> " + strings.Repeat("•", min(len(f.value), 32)) + "█"
	}
	return "> " + string(f.value) + "█"
}
