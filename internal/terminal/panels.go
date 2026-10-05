package terminal

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

func (m *screen) commandMenuView(budget int) (string, *tea.Cursor) {
	input, cursor := m.editorView("Commands · type to filter", m.command)
	footer := m.wrap(m.styles.muted.Render("↑/↓ Select · Tab Complete\nEnter Open · Esc Back"))
	if m.notice != "" {
		footer = m.wrap(m.styles.waiting.Render(cleanText(m.notice))) + "\n" + footer
	}
	room := max(1, budget-lipgloss.Height(input)-lipgloss.Height(footer))
	matches := m.matchingCommands()
	rows := 1
	if m.width < 70 {
		rows = 2
	}
	count := min(7, max(1, room/rows))
	selected := min(m.menuSelection, max(0, len(matches)-1))
	start := max(0, selected-count+1)
	var lines []string
	for i := start; i < min(len(matches), start+count); i++ {
		c := matches[i]
		style := m.styles.muted
		prefix := "  "
		if i == selected {
			style, prefix = m.styles.selected, "› "
		}
		detail := c.description
		if c.unavailable != "" {
			detail = c.unavailable
		}
		line := fmt.Sprintf("%s%-13s %s", prefix, "/"+c.name, detail)
		if rows == 2 {
			line = prefix + "/" + c.name + "\n  " + detail
		}
		// Menu descriptions are summaries; full help remains scrollable.
		for _, row := range strings.Split(line, "\n") {
			lines = append(lines, style.Width(max(1, m.width-4)).MaxWidth(max(1, m.width-4)).Render(row))
		}
	}
	if len(matches) == 0 {
		lines = []string{m.styles.waiting.Render("No matching command. Esc returns to your draft.")}
	}
	list := lipgloss.NewStyle().Height(room).AlignVertical(lipgloss.Bottom).Render(m.wrap(strings.Join(lines, "\n")))
	if cursor != nil {
		cursor.Y += lipgloss.Height(list)
	}
	return list + "\n" + input + "\n" + footer, cursor
}

func (m *screen) helpView() string {
	text := m.section("Help") + "\n\nRun one role in each terminal.\nController accepts prompts. Agent owns the runtime.\nEach prompt is a new request. History is not model context.\n\nType / to find a command. Use ↑/↓, Tab and Enter.\nEsc returns without changing your draft.\nType // to begin a prompt with a literal slash.\nPasted text stays literal.\n"
	for _, c := range m.commands() {
		text += "\n/" + c.name + "  " + c.description
	}
	text += "\n\nKeyboard controls\nEnter  Send or select\nCtrl-J  New line\nCtrl-C  Cancel request; exit when idle\nPgUp/PgDn  Scroll history or details\nF1  Help · F2  Doctor · F3  Roles\nF4  Controller address · F5  Retry · F6  Runtime port"
	return text
}

func (m *screen) statusView() string {
	out := m.section("Status") + "\n" + m.diagnosisStatus()
	if m.state.role == config.RoleWorker {
		out += "\n\n" + m.agentDetails()
	} else {
		endpoint := m.state.endpoint
		if endpoint == "" {
			endpoint = m.listener()
		}
		out += "\n\nController endpoint  " + cleanText(endpoint) + "\nModel  " + cleanText(m.state.cfg.Model) + "\n" + m.agentCounts()
		out += "\nUse /agents for membership and capacity details."
	}
	if m.state.notice != "" {
		out += "\n\n" + cleanText(m.state.notice)
	}
	if strings.Contains(m.notice, "\n") {
		out += "\n\n" + cleanText(m.notice)
	}
	return out
}

func (m *screen) workerLabel(w *meshv1.WorkerInfo) string {
	if !capacityFresh(m.state.statusAt, m.state.now) {
		return "Unavailable"
	}
	if eligible(w, m.state.cfg.Model) {
		return "Ready"
	}
	if w.State == meshv1.WorkerState_WORKER_STATE_BUSY {
		return "Busy"
	}
	return "Unavailable"
}

func (m *screen) agentCounts() string {
	ready, busy := 0, 0
	if m.state.status != nil {
		for _, w := range m.state.status.Workers {
			if w == nil {
				continue
			}
			switch m.workerLabel(w) {
			case "Ready":
				ready++
			case "Busy":
				busy++
			}
		}
	}
	return m.styles.ready.Render(fmt.Sprintf("Agents  %d ready", ready)) + m.styles.muted.Render(fmt.Sprintf(" · %d busy", busy))
}

func (m *screen) agentsView() string {
	out := m.section("Agents") + "\n" + m.agentCounts()
	if m.state.status == nil || len(m.state.status.Workers) == 0 {
		return out + "\n\nNo Agents in the current status. Start an Agent in another terminal or on another Mac."
	}
	for _, w := range m.state.status.Workers {
		if w == nil {
			continue
		}
		out += "\n\n" + m.styles.accent.Bold(true).Render(agentName(w.WorkerId)) + "  " + m.statusLabel(m.workerLabel(w))
		out += "\nHost  " + cleanText(w.GetHardware().GetHostname()) + "\nAgent listen  " + cleanText(w.Endpoint) + "\nAgent ID  " + cleanText(w.WorkerId)
		if detail := w.GetReport().GetLastError(); detail != "" {
			out += "\n" + m.styles.failure.Render(cleanText(detail))
		}
	}
	return out
}

func (m *screen) logsView(budget int) string {
	label := "Live · recent logs from this process · up to 64 KiB"
	if m.logsPaused {
		label = "Paused · End resumes live logs"
	}
	header := m.wrap(m.section("Logs") + "\n" + m.styles.muted.Render(label))
	footer := m.detailFooter("PgUp/PgDn Scroll · End Live\n/ Commands · Esc Back · Ctrl-C")
	room := max(0, budget-lipgloss.Height(header)-lipgloss.Height(footer))
	if room == 0 {
		return header + "\n" + footer
	}
	text := cleanText(m.diagnostics)
	if text == "" {
		text = "No session logs yet."
	}
	lines := strings.Split(m.wrap(strings.TrimSpace(text)), "\n")
	end := max(1, len(lines)-min(m.scroll, max(0, len(lines)-room)))
	start := max(0, end-room)
	body := lipgloss.NewStyle().Height(room).Render(strings.Join(lines[start:end], "\n"))
	return header + "\n" + body + "\n" + footer
}

// Keep action feedback outside the scrollable details, including while logs
// are paused. Longer address instructions remain in the status content.
func (m *screen) detailFooter(controls string) string {
	footer := m.wrap(m.styles.muted.Render(controls))
	if m.notice != "" && !strings.Contains(m.notice, "\n") {
		footer = m.wrap(m.styles.waiting.Render(cleanText(m.notice))) + "\n" + footer
	}
	return footer
}
