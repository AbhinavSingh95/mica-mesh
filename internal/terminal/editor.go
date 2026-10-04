package terminal

import (
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

func newPrompt() textarea.Model {
	m := textarea.New()
	m.SetVirtualCursor(false)
	m.CharLimit = 0
	m.MaxHeight = 0
	m.SetHeight(4)
	m.KeyMap.Paste.SetEnabled(false)
	m.KeyMap.CopySelection.SetEnabled(false)
	// These optional case edits can increase UTF-8 length without inserting text.
	// Keep the ordinary movement, deletion and selection bindings instead.
	m.KeyMap.UppercaseWordForward.SetEnabled(false)
	m.KeyMap.LowercaseWordForward.SetEnabled(false)
	m.KeyMap.CapitalizeWordForward.SetEnabled(false)
	m.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	m.Focus()
	return m
}

// updatePrompt rejects the whole insertion before textarea allocates/mutates it.
// Enter belongs to the outer submission model. Only key and paste input enters here.
func updatePrompt(m textarea.Model, msg tea.Msg) (textarea.Model, bool) {
	insertion := ""
	switch v := msg.(type) {
	case tea.PasteMsg:
		insertion = v.Content
	case tea.KeyPressMsg:
		if v.Code == tea.KeyEnter {
			return m, true
		}
		insertion = v.Text
		if key.Matches(v, m.KeyMap.InsertNewline) {
			insertion = "\n"
		}
	default:
		return m, true
	}
	if len(insertion) > 0 {
		// Match Bubbles' pinned runeutil sanitizer before it deletes the selection:
		// tabs become four spaces, CR/LF become newlines, other controls and RuneError
		// disappear. Count without allocating the potentially expanded insertion.
		size := len(m.Value()) - len(m.SelectedText())
		for _, r := range insertion {
			switch {
			case r == '\t':
				size += 4
			case r == '\r' || r == '\n':
				size++
			case r == utf8.RuneError || unicode.IsControl(r):
			default:
				size += utf8.RuneLen(r)
			}
			if size > promptLimit {
				return m, false
			}
		}
	}
	m, _ = m.Update(msg)
	return m, true
}
