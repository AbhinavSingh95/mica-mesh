package terminal

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

// View lays out copied display state. It performs no application work.
func (m *screen) View() tea.View {
	width := max(1, m.width-4)
	role := ""
	if m.state.role == config.RoleController {
		role = "Controller"
	}
	if m.state.role == config.RoleWorker {
		role = "Agent"
	}
	title := "Mica Mesh"
	if role != "" {
		title += " / " + role
	} else {
		title += " · " + cleanText(m.state.version)
	}
	if m.width < 40 || m.height < 12 {
		return m.frame(title+"\nResize to at least 40 × 12.\nCtrl-C Cancel or exit · F1 Help", nil)
	}
	title = lipgloss.NewStyle().Bold(true).Render(title)
	budget := m.height - 4 // One top/bottom padding row, title and separator.
	var body string
	switch {
	case m.help:
		body = m.panel("Help\n\nRun one role in each terminal.\nController accepts prompts. Agent owns the runtime.\nEach prompt is a new request. History is not model context.\n\nEnter  Send or select\nCtrl-J  New line\nCtrl-C  Cancel request; exit when idle\nPgUp/PgDn  Scroll response history\nF2  Local diagnosis and service notices\nF3  Stop this role and return to roles\nF4  Enter a Controller address (LAN Agent)\nF5  Retry after a failure\nF6  Change runtime port (restarts owned Agent)", "Esc Back · Ctrl-C Cancel or exit", budget)
	case m.diagnosis:
		body = m.panel("Diagnosis\n"+cleanText(m.state.notice)+"\n"+suffix(m.diagnostics, 8192), "Esc Back · Ctrl-C Cancel or exit", budget)
	case m.editing:
		label := "Controller address (HOST:PORT)"
		if m.input == changePort {
			label = "Runtime port (1–65535)"
		}
		body = label + "\n" + m.prompt.View() + "\nEnter Apply · Esc Back"
	case m.state.phase == picking:
		a, b := "  ", "  "
		if m.selection == 0 {
			a = "› "
		} else {
			b = "› "
		}
		body = "Choose a role\n" + a + "Controller — Send prompts\n" + b + "Agent — Serve requests\nOne role per terminal.\n↑/↓ Select · Enter Start · F1 Help\nPress Ctrl-C or q to exit."
	case m.state.phase == choosingNetwork:
		text := "Network access\nListen: " + cleanText(m.listener()) + "\nDevices on your LAN can connect.\n"
		choices := m.state.interfaces
		start := max(0, m.selection-1)
		end := min(len(choices), start+max(1, budget-5))
		for i := start; i < end; i++ {
			prefix := "  "
			if i == m.selection {
				prefix = "› "
			}
			text += prefix + cleanText(choices[i].Name) + "  " + cleanText(choices[i].IPv4) + "\n"
		}
		body = m.panel(text, "↑/↓ Select · Enter Start · F3 Roles", budget)
	case m.state.phase == awaitingConsent:
		d := m.state.download
		text := "Prepare Agent files\nModel: " + cleanText(d.ID) + fmt.Sprintf("\nDownload: %d bytes over HTTPS\n", d.SizeBytes) + "Source: " + cleanText(d.URL) + "\nStore: " + cleanText(d.Path) + "\nVerified files need no new download."
		// Consent must show the entire source/destination. A short terminal scrolls
		// this panel with PgUp/PgDn; consent controls remain outside the viewport.
		body = m.panel(text, "PgDn Details · Y Prepare · N Back", budget)
	default:
		if m.state.role == config.RoleController {
			body = m.controllerView(budget)
		} else {
			body = m.agentView(budget)
		}
	}
	content := title + "\n" + strings.Repeat("─", min(width, 72)) + "\n" + body
	var cursor *tea.Cursor
	if !m.help && !m.diagnosis && (m.editing || m.canSubmit()) {
		if index := strings.LastIndex(content, strings.TrimRight(strings.Split(m.prompt.View(), "\n")[0], " ")); index >= 0 {
			cursor = m.prompt.Cursor()
			if cursor != nil {
				cursor.X += 2
				cursor.Y += strings.Count(content[:index], "\n") + 1
			}
		}
	}
	return m.frame(content, cursor)
}
func (m *screen) frame(content string, cursor *tea.Cursor) tea.View {
	width := max(1, m.width-4)
	content = lipgloss.NewStyle().Width(width).MaxWidth(width).Render(content)
	lines := strings.Split(content, "\n")
	if len(lines) > max(1, m.height-2) {
		lines = lines[:max(1, m.height-2)]
	}
	v := tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render(strings.Join(lines, "\n")))
	v.AltScreen = true
	if cursor != nil && cursor.Y < m.height-1 {
		v.Cursor = cursor
	}
	return v
}
func (m *screen) wrap(s string) string {
	return lipgloss.NewStyle().Width(max(1, m.width-4)).MaxWidth(max(1, m.width-4)).Render(s)
}
func (m *screen) panel(text, footer string, budget int) string {
	foot := m.wrap(footer)
	lines := strings.Split(m.wrap(strings.TrimSpace(text)), "\n")
	room := max(0, budget-lipgloss.Height(foot))
	start := min(m.scroll, max(0, len(lines)-room))
	end := min(len(lines), start+room)
	return strings.Join(lines[start:end], "\n") + "\n" + foot
}
func (m *screen) listener() string {
	if m.state.role == config.RoleController {
		return m.state.cfg.ControllerListen
	}
	return m.state.cfg.WorkerListen
}
func (m *screen) phaseLabel() string {
	switch m.state.phase {
	case checking:
		return "Checking"
	case preparing:
		return "Preparing"
	case starting:
		return "Starting"
	case stopping:
		return "Stopping"
	case failed:
		return "Failed"
	}
	return "Waiting"
}
func (m *screen) cleanupNotice() string {
	if m.forgottenCleanup {
		return "Cleanup unconfirmed for an earlier request."
	}
	for _, r := range m.records {
		if r.cleanup == cleanupUnconfirmed {
			return "Cleanup unconfirmed for an earlier request."
		}
	}
	return ""
}
func (m *screen) controllerView(budget int) string {
	ready, busy := 0, 0
	var agents []string
	fresh := capacityFresh(m.state.statusAt, m.state.now)
	if m.state.status != nil {
		for _, w := range m.state.status.Workers {
			if w == nil {
				continue
			}
			label := "Unavailable"
			if fresh && eligible(w, m.state.cfg.Model) {
				label = "Ready"
				ready++
			} else if fresh && w.State == meshv1.WorkerState_WORKER_STATE_BUSY {
				label = "Busy"
				busy++
			}
			name := w.GetHardware().GetHostname()
			if name == "" {
				name = w.WorkerId
			}
			agents = append(agents, cleanText(name)+" · "+label)
		}
	}
	endpoint := m.state.endpoint
	if endpoint == "" {
		endpoint = m.listener()
	}
	header := "Endpoint  " + cleanText(endpoint) + fmt.Sprintf("\nAgents  %d ready · %d busy", ready, busy)
	if m.state.phase != running {
		header += " · " + m.phaseLabel()
	}
	if m.height >= 24 && len(agents) > 0 {
		header += "\n" + strings.Join(agents[:min(2, len(agents))], "\n")
	}
	header += "\nEach prompt is a new request."
	if notice := m.cleanupNotice(); notice != "" {
		header = "Cleanup unconfirmed · F1 Help\n" + header
	}
	if m.state.statusError {
		header = "Status unavailable · F2 Diagnose\n" + header
	}
	if m.notice != "" {
		header += "\n" + cleanText(m.notice)
	} else if m.state.notice != "" {
		header += "\n" + cleanText(m.state.notice)
	}
	label := "Enter a prompt"
	if !m.canSubmit() {
		label += " · Waiting for an Agent"
	}
	if m.requestBusy() {
		label = "Generating · Ctrl-C Cancel request"
		if m.state.request.cleanup == cleanupChecking {
			label = "Stopping · checking Agent cleanup"
		}
	}
	footer := "Enter Send · Ctrl-C Cancel/exit\nCtrl-J New line · F1 Help · F3 Roles"
	if m.width >= 60 {
		footer = "Enter Send · Ctrl-J New line · Ctrl-C Cancel/exit\nPgUp/PgDn History · F1 Help · F2 Diagnose · F3 Roles"
	}
	input := label + "\n" + m.prompt.View() + "\n" + footer
	header = m.wrap(header)
	input = m.wrap(input)
	room := max(0, budget-lipgloss.Height(header)-lipgloss.Height(input))
	// Notices can be long. Keep the input and footer visible while the full
	// actionable diagnostic remains available through F2.
	if lipgloss.Height(header)+lipgloss.Height(input) > budget {
		lines := strings.Split(header, "\n")
		keep := max(0, budget-lipgloss.Height(input))
		header = strings.Join(lines[:min(len(lines), keep)], "\n")
	}
	response := ""
	for _, r := range m.records {
		response += "› " + r.prompt + "\n"
		if r.omitted {
			response += omission
		}
		response += r.response + "\n"
		if r.worker != "" {
			response += "Agent " + r.worker + " · "
		}
		switch {
		case r.cleanup == cleanupChecking:
			response += "Stopping · cleanup in progress"
		case r.cleanup == cleanupConfirmed:
			response += "Canceled · cleanup confirmed"
		case r.cleanup == cleanupUnconfirmed:
			response += "Canceled · cleanup unconfirmed"
		case !r.done:
			response += "Generating · " + duration(r.total)
		case r.failure != "":
			response += "Failed: " + r.failure + " · Send a new prompt to retry."
		default:
			response += "Complete · " + r.finish + " · First text " + duration(r.first) + " · Total " + duration(r.total)
		}
		response += "\n"
	}
	if room > 0 {
		lines := strings.Split(m.wrap(strings.TrimSpace(response)), "\n")
		end := max(1, len(lines)-min(m.scroll, max(0, len(lines)-1)))
		start := max(0, end-room)
		response = strings.Join(lines[start:end], "\n")
		return header + "\n" + response + "\n" + input
	}
	return header + "\n" + input
}
func (m *screen) agentView(budget int) string {
	runtimeState := "Starting"
	if r := m.state.agent.Report; r != nil {
		switch r.RuntimeState {
		case meshv1.RuntimeState_RUNTIME_STATE_READY:
			runtimeState = "Ready"
		case meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY:
			runtimeState = "Failed"
		}
		if r.Active {
			runtimeState = "Busy"
		}
	}
	membership := "Waiting for Controller"
	if m.state.agent.Membership.Registered {
		membership = "Connected"
	} else if m.state.agent.Membership.ControllerAddress != "" {
		membership = "Connecting"
	}
	if m.state.phase != running {
		runtimeState = m.phaseLabel()
	}
	endpoint := m.state.agent.Endpoint
	if endpoint == "" {
		endpoint = m.listener()
	}
	model := m.state.cfg.Model
	if model == config.Default().Model {
		model = "Qwen2.5 0.5B"
	}
	out := "Runtime  " + runtimeState + "\nModel  " + cleanText(model) + "\nListen  " + cleanText(endpoint) + "\n" + membership
	if m.state.agent.Membership.ControllerAddress != "" {
		out += "\n" + cleanText(m.state.agent.Membership.ControllerAddress)
	}
	if m.height >= 20 && m.state.agent.WorkerID != "" {
		out += "\nAgent ID  " + cleanText(m.state.agent.WorkerID)
	}
	if m.state.phase == preparing {
		p := m.state.progress
		out += fmt.Sprintf("\n%s · %d / %d bytes", p.Phase, p.CompletedBytes, p.TotalBytes)
	}
	if m.state.agent.Report != nil && m.state.agent.Report.LastError != "" {
		out += "\n" + cleanText(m.state.agent.Report.LastError) + "\nF6 Change runtime port"
	}
	if m.state.notice != "" {
		out += "\n" + cleanText(m.state.notice)
	}
	if m.notice != "" {
		out += "\n" + cleanText(m.notice)
	}
	footer := "Ctrl-C Exit · F1 Help · F3 Roles\nF4 Address · F5 Retry · F6 Port"
	if m.state.network == app.Local {
		out += "\nController address  " + cleanText(m.state.cfg.ControllerAddress)
		out += "\nTo change it, exit and run:\nmica-mesh agent --local\n  --controller-address 127.0.0.1:PORT"
		footer = "Ctrl-C Exit · F1 Help · F3 Roles\nF2 Diagnose · F5 Retry · F6 Port"
	} else if len(m.state.candidates) > 1 && !m.state.agent.Membership.Registered {
		out += "\nChoose a Controller (↑/↓, Enter):"
		start := max(0, m.selection-1)
		for i := start; i < min(len(m.state.candidates), start+2); i++ {
			c := m.state.candidates[i]
			prefix := "  "
			if i == m.selection {
				prefix = "› "
			}
			out += "\n" + prefix + cleanText(c.Hostname) + " · " + cleanText(c.Address) + "\n" + cleanText(c.InstanceID)
		}
	} else if m.height >= 20 {
		out += "\nLeave this terminal open. Start a Controller\nin another terminal or on another Mac."
	}
	return m.panel(out, footer, budget)
}
