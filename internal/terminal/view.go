package terminal

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
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
	title := m.styles.accent.Bold(true).Render("Mica Mesh")
	if role != "" {
		title += "  " + m.styles.selected.Bold(true).Padding(0, 1).Render(role)
		scope := "LAN"
		if m.state.network == app.Local {
			scope = "Local"
		}
		title += "  " + m.styles.muted.Render(scope)
	} else {
		title += m.styles.muted.Render(" · " + cleanText(m.state.version))
	}
	if m.width < 40 || m.height < 12 {
		return m.frame(title+"\nResize to at least 40 × 12.\nCtrl-C Cancel or exit · F1 Help", nil)
	}
	budget := m.height - 4 // One top/bottom padding row, title and separator.
	var body string
	var cursor *tea.Cursor
	switch {
	case m.help:
		text := m.section("Help") + "\n\nRun one role in each terminal.\nController accepts prompts. Agent owns the runtime.\nEach prompt is a new request. History is not model context.\n\nEnter  Send or select\nCtrl-J  New line\nCtrl-C  Cancel request; exit when idle\nPgUp/PgDn  Scroll history or panel details\nF2  Local diagnosis and service notices\nF3  Stop this role and return to roles\nF4  Enter a Controller address (LAN Agent)\nF5  Retry after a failure\nF6  Change runtime port (restarts owned Agent)"
		if m.state.role == config.RoleWorker && m.state.network == app.Local {
			text += "\n\nTo change the local Controller address, exit and run:\nmica-mesh agent --local\n  --controller-address 127.0.0.1:PORT"
		}
		body = m.panel(text, "PgUp/PgDn Details · Esc Back · Ctrl-C Exit", budget)
	case m.diagnosis:
		body = m.panel(m.section("Diagnosis")+"\n"+cleanText(m.state.notice)+"\n"+suffix(m.diagnostics, 8192), "PgUp/PgDn Details · Esc Back · Ctrl-C Exit", budget)
	case m.editing:
		label := "Controller address (HOST:PORT)"
		if m.input == changePort {
			label = "Runtime port (1–65535)"
		}
		body, cursor = m.editorView(label)
		body += "\n" + m.styles.muted.Render("Enter Apply · Esc Back")
	case m.state.phase == picking:
		body = m.pickerView(budget)
	case m.state.phase == choosingNetwork:
		text := m.section("Network access") + "\nListen: " + cleanText(m.listener()) + "\nDevices on your LAN can connect.\n"
		choices := m.state.interfaces
		start := max(0, m.selection-1)
		end := min(len(choices), start+max(1, budget-5))
		for i := start; i < end; i++ {
			prefix := "  "
			if i == m.selection {
				prefix = "› "
			}
			line := prefix + cleanText(choices[i].Name) + "  " + cleanText(choices[i].IPv4)
			if i == m.selection {
				line = m.styles.selected.Render(line)
			}
			text += line + "\n"
		}
		body = m.panel(text, "↑/↓ Select · Enter Start · F3 Roles", budget)
	case m.state.phase == awaitingConsent:
		d := m.state.download
		text := m.section("Prepare Agent files") + "\nModel: " + cleanText(d.ID) + fmt.Sprintf("\nDownload: %.1f MB over HTTPS\nExact size: %d bytes\n", float64(d.SizeBytes)/1e6, d.SizeBytes) + "Source: " + cleanText(d.URL) + "\nStore: " + cleanText(d.Path) + "\nVerified files need no new download."
		// Consent must show the entire source/destination. A short terminal scrolls
		// this panel with PgUp/PgDn; consent controls remain outside the viewport.
		body = m.panel(text, "PgDn Details · Y Prepare · N Back", budget)
	default:
		if m.state.role == config.RoleController {
			body, cursor = m.controllerView(budget)
		} else {
			body = m.agentView(budget)
		}
	}
	content := title + "\n" + m.styles.muted.Render(strings.Repeat("─", width)) + "\n" + body
	if cursor != nil {
		cursor.Y += 2 // Title and separator; frame padding is applied below.
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
	if cursor != nil {
		cursor.X += 2
		cursor.Y++
		if cursor.Y < m.height-1 && cursor.X < m.width-2 {
			v.Cursor = cursor
		}
	}
	return v
}
func (m *screen) wrap(s string) string {
	return lipgloss.NewStyle().Width(max(1, m.width-4)).MaxWidth(max(1, m.width-4)).Render(s)
}
func (m *screen) panel(text, footer string, budget int) string {
	foot := m.wrap(m.styles.muted.Render(footer))
	lines := strings.Split(m.wrap(strings.TrimSpace(text)), "\n")
	room := max(0, budget-lipgloss.Height(foot))
	start := min(m.scroll, max(0, len(lines)-room))
	end := min(len(lines), start+room)
	content := lipgloss.NewStyle().Height(room).Render(strings.Join(lines[start:end], "\n"))
	return content + "\n" + foot
}

func (m *screen) card(content string) string {
	return m.styles.border.Width(m.width-4).Padding(0, 1).Render(content)
}

// editorView returns the cursor relative to its own layout. Explicit offsets
// keep it aligned through borders, Unicode text, wrapping, and terminal resize.
func (m *screen) editorView(label string) (string, *tea.Cursor) {
	label = m.wrap(m.styles.accent.Render(label))
	input := m.prompt.View()
	cursor := m.prompt.Cursor()
	if m.roomy() {
		input = m.card(input)
		if cursor != nil {
			cursor.X += 2 // Border and one column of inner padding.
			cursor.Y++
		}
	}
	if cursor != nil {
		cursor.Y += lipgloss.Height(label)
	}
	return label + "\n" + input, cursor
}

func (m *screen) pickerView(budget int) string {
	text := m.styles.accent.Bold(true).Render("Choose a role") + "\nOne role per terminal.\n"
	for i, choice := range []struct{ name, description string }{
		{"Controller", "Connect Agents and test inference."},
		{"Agent", "Prepare this Mac and serve requests."},
	} {
		prefix := "  "
		style := m.styles.muted
		if i == m.selection {
			prefix, style = "› ", m.styles.selected
		}
		line := style.Bold(true).Render(prefix + choice.name)
		if m.roomy() {
			line = m.card(line + "\n  " + choice.description)
		}
		text += "\n" + line
	}
	return m.panel(text, "↑/↓ Select · Enter Start · F1 Help\nCtrl-C or q Exit", budget)
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

// controllerLayout shares the actual history height with keyboard paging.
// This keeps every retained line reachable when notices or resize reduce it.
func (m *screen) controllerLayout(budget int) (header, input string, cursor *tea.Cursor, historyRows int) {
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
			agents = append(agents, m.statusLabel(label)+"  "+cleanText(name)+" · "+cleanText(w.Endpoint))
		}
	}
	endpoint := m.state.endpoint
	if endpoint == "" {
		endpoint = m.listener()
	}
	header = "Endpoint  " + cleanText(endpoint) + "\n" + m.styles.ready.Render(fmt.Sprintf("Agents  %d ready", ready)) + m.styles.muted.Render(fmt.Sprintf(" · %d busy", busy))
	if m.state.phase != running {
		header += " · " + m.statusLabel(m.phaseLabel())
		if m.state.phase != failed {
			header += " " + m.styles.waiting.Render(m.activity())
		}
	}
	if m.roomy() && len(agents) > 0 {
		agentRows := strings.Join(agents[:min(2, len(agents))], "\n")
		if len(agents) > 2 {
			agentRows += m.styles.muted.Render(fmt.Sprintf("\n+ %d more Agents", len(agents)-2))
		}
		header += "\n" + m.card(agentRows)
	}
	header += "\n" + m.styles.muted.Render("Each prompt is a new request.")
	if notice := m.cleanupNotice(); notice != "" {
		header = m.styles.waiting.Render("Cleanup unconfirmed · F1 Help") + "\n" + header
	}
	if m.state.statusError {
		header = m.styles.failure.Render("Status unavailable · F2 Diagnose") + "\n" + header
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
		label = m.activity() + " Generating · Ctrl-C Cancel request"
		if m.state.request.cleanup == cleanupChecking {
			label = m.activity() + " Stopping · checking Agent cleanup"
		}
	}
	footer := "Enter Send · Ctrl-C Cancel/exit\nCtrl-J New line · F1 Help · F3 Roles"
	if m.width >= 60 {
		footer = "Enter Send · Ctrl-J New line · Ctrl-C Cancel/exit\nPgUp/PgDn History · F1 Help · F2 Diagnose · F3 Roles"
	}
	if m.requestBusy() {
		footer = "Ctrl-C Cancel request · F1 Help\nPgUp/PgDn History · F2 Diagnose · F3 Roles"
		if m.width < 60 {
			footer = "Ctrl-C Cancel · F1 Help · F3 Roles\nPgUp/PgDn History · F2 Diagnose"
		}
	}
	input, cursor = m.editorView(label)
	if !m.canSubmit() {
		cursor = nil
	}
	input += "\n" + m.styles.muted.Render(footer)
	header = m.wrap(header)
	input = m.wrap(input)
	// Notices can be long. Keep the input and footer visible while the full
	// actionable diagnostic remains available through F2.
	if lipgloss.Height(header)+lipgloss.Height(input) > budget {
		lines := strings.Split(header, "\n")
		keep := max(0, budget-lipgloss.Height(input))
		header = strings.Join(lines[:min(len(lines), keep)], "\n")
	}
	return header, input, cursor, max(0, budget-lipgloss.Height(header)-lipgloss.Height(input))
}

func (m *screen) controllerView(budget int) (string, *tea.Cursor) {
	header, input, cursor, room := m.controllerLayout(budget)
	response := ""
	for _, r := range m.records {
		prompt := m.styles.accent.Render("› "+r.prompt) + "\n"
		answer := r.response
		if r.omitted {
			answer = omission + answer
		}
		meta := ""
		if r.worker != "" {
			meta = "Agent " + r.worker + " · "
		}
		switch {
		case r.cleanup == cleanupChecking:
			meta += "Stopping · cleanup in progress"
		case r.cleanup == cleanupConfirmed:
			meta += "Canceled · cleanup confirmed"
		case r.cleanup == cleanupUnconfirmed:
			meta += "Canceled · cleanup unconfirmed"
		case !r.done:
			meta += "Generating · " + duration(r.total)
		case r.failure != "":
			meta += "Failed: " + r.failure + " · Send a new prompt to retry."
		default:
			meta += "Complete · " + r.finish + " · First text " + duration(r.first) + " · Total " + duration(r.total)
		}
		style := m.styles.muted
		if r.cleanup == cleanupChecking || r.cleanup == cleanupUnconfirmed {
			style = m.styles.waiting
		} else if r.failure != "" {
			style = m.styles.failure
		}
		if room <= 2 {
			// Put metadata before the answer in a small viewport so new text
			// remains visible. Page Up still reaches every retained field.
			response += prompt + style.Render(meta) + "\n" + answer + "\n"
		} else {
			response += prompt + answer + "\n" + style.Render(meta) + "\n"
		}
	}
	prefix := header
	if room > 0 {
		if response == "" && m.roomy() {
			response = m.styles.muted.Render("INFERENCE TEST\nSend a prompt to check the mesh.\nYour application supplies its own context.")
			if !m.canSubmit() {
				response = m.styles.muted.Render("INFERENCE TEST\nStart an Agent in another terminal.\nThis screen updates when it connects.")
			}
		}
		lines := strings.Split(m.wrap(strings.TrimSpace(response)), "\n")
		end := max(1, len(lines)-min(m.scroll, max(0, len(lines)-1)))
		start := max(0, end-room)
		response = lipgloss.NewStyle().Height(room).Render(strings.Join(lines[start:end], "\n"))
		prefix += "\n" + response
	}
	if cursor != nil {
		cursor.Y += lipgloss.Height(prefix)
	}
	return prefix + "\n" + input, cursor
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
	runtimeLine := "Runtime  " + runtimeState
	style := m.styles.waiting
	if runtimeState == "Ready" {
		style = m.styles.ready
	} else if runtimeState == "Failed" {
		style = m.styles.failure
	} else {
		runtimeLine += " " + m.activity()
	}
	memberLine := m.statusLabel(membership)
	if m.state.phase == running && membership != "Connected" {
		memberLine += " " + m.styles.waiting.Render(m.activity())
	}
	out := style.Bold(true).Render(runtimeLine) + "\n" + memberLine
	if m.roomy() {
		out = m.card(out)
	}
	if m.state.phase == preparing {
		out += "\n" + m.preparationView()
	}
	out += "\nModel  " + cleanText(model) + "\nAgent listen  " + cleanText(endpoint)
	out += fmt.Sprintf("\nRuntime address  127.0.0.1:%d", m.state.cfg.RuntimePort)
	controller := m.state.agent.Membership.ControllerAddress
	if controller == "" {
		controller = m.state.cfg.ControllerAddress
	}
	if controller == "" {
		controller = "Not selected"
	}
	out += "\nController address  " + cleanText(controller)
	if m.state.agent.Report != nil && m.state.agent.Report.LastError != "" {
		out += "\n" + m.styles.failure.Render(cleanText(m.state.agent.Report.LastError)) + "\nF6 Change runtime port"
	}
	if m.state.notice != "" {
		out += "\n" + m.styles.waiting.Render(cleanText(m.state.notice))
	}
	if m.notice != "" {
		out += "\n" + m.styles.waiting.Render(cleanText(m.notice))
	}
	footer := "Ctrl-C Exit · F1 Help · F3 Roles\nF4 Address · F5 Retry · F6 Port"
	if m.state.network == app.Local {
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
			line := prefix + cleanText(c.Hostname) + " · " + cleanText(c.Address)
			if i == m.selection {
				line = m.styles.selected.Render(line)
			}
			out += "\n" + line + "\n" + m.styles.muted.Render(cleanText(c.InstanceID))
		}
	}
	if m.roomy() && m.state.phase == running {
		if m.state.agent.Membership.Registered {
			out += "\n\n" + m.styles.muted.Render("Leave this terminal open to serve requests.")
		} else {
			out += "\n\n" + m.styles.muted.Render("Start a Controller in another terminal or on another Mac.")
		}
	}
	if m.height >= 28 && m.state.agent.WorkerID != "" {
		out += "\n" + m.styles.muted.Render("Agent ID  "+cleanText(m.state.agent.WorkerID))
	}
	if lipgloss.Height(m.wrap(out))+lipgloss.Height(m.wrap(footer)) > budget {
		footer = "PgUp/PgDn Details\n" + footer
	}
	return m.panel(out, footer, budget)
}

func (m *screen) preparationView() string {
	p := m.state.progress
	switch p.Phase {
	case setup.Downloading:
		if p.TotalBytes > 0 {
			fraction := min(1.0, max(0.0, float64(p.CompletedBytes)/float64(p.TotalBytes)))
			return m.styles.accent.Render(fmt.Sprintf("Downloading · %d%% · %.1f / %.1f MB", int(fraction*100), float64(p.CompletedBytes)/1e6, float64(p.TotalBytes)/1e6)) + "\n" + m.progress.ViewAs(fraction)
		}
		return m.styles.waiting.Render("Downloading · " + m.activity())
	case setup.Verifying:
		return m.styles.waiting.Render("Verifying files · " + m.activity())
	case setup.Saving:
		return m.styles.waiting.Render("Saving files · " + m.activity())
	case setup.Complete:
		return m.styles.ready.Render("Files verified · Starting runtime")
	default:
		return m.styles.waiting.Render("Checking files · " + m.activity())
	}
}
