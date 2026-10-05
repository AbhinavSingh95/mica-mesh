package terminal

import (
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

// screenStyles keeps the presentation consistent across roles and recovery panels.
// Colors adapt to the terminal background; the Console owns NO_COLOR conversion.
type screenStyles struct {
	accent, muted, ready, waiting, failure lipgloss.Style
	border, selected                       lipgloss.Style
}

func (m *screen) setTheme(dark bool) {
	choose := lipgloss.LightDark(dark)
	accent := choose(lipgloss.Color("#00766C"), lipgloss.Color("#59DDC5"))
	muted := choose(lipgloss.Color("#526B66"), lipgloss.Color("#9BB4AE"))
	border := choose(lipgloss.Color("#708A83"), lipgloss.Color("#58716B"))
	m.styles = screenStyles{
		accent:   lipgloss.NewStyle().Foreground(accent),
		muted:    lipgloss.NewStyle().Foreground(muted),
		ready:    lipgloss.NewStyle().Foreground(choose(lipgloss.Color("#287331"), lipgloss.Color("#A1D887"))),
		waiting:  lipgloss.NewStyle().Foreground(choose(lipgloss.Color("#905B05"), lipgloss.Color("#EEC275"))),
		failure:  lipgloss.NewStyle().Foreground(choose(lipgloss.Color("#A43B3B"), lipgloss.Color("#F7948B"))),
		border:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border),
		selected: lipgloss.NewStyle().Foreground(accent).Background(choose(lipgloss.Color("#DFEFE9"), lipgloss.Color("#173A34"))),
	}
	styles := textarea.DefaultStyles(dark)
	styles.Focused.Base = lipgloss.NewStyle()
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Focused.Prompt = m.styles.accent
	styles.Focused.Placeholder = m.styles.muted
	styles.Focused.EndOfBuffer = m.styles.muted
	styles.Blurred = styles.Focused
	// Cursor color uses OSC rather than SGR and bypasses NO_COLOR conversion.
	// Keep the user's terminal cursor color in both themes.
	styles.Cursor.Color = nil
	m.prompt.SetStyles(styles)
	m.progress.FullColor, m.progress.EmptyColor = accent, border
}

func (m *screen) roomy() bool { return m.width >= 60 && m.height >= 24 }

func (m *screen) section(label string) string {
	return m.styles.muted.Render(strings.ToUpper(label))
}

func (m *screen) activity() string {
	// The owned session already publishes time every 250 ms. No extra timer or
	// goroutine is needed, and idle views never render this indicator.
	frames := [...]string{"⠋", "⠙", "⠹", "⠸"}
	return frames[uint64(m.state.now.UnixMilli()/250)%uint64(len(frames))]
}

func (m *screen) statusLabel(label string) string {
	s := m.styles.waiting
	switch label {
	case "Ready", "Connected", "Complete":
		s = m.styles.ready
	case "Failed", "Unavailable":
		s = m.styles.failure
	}
	return s.Render(label)
}
