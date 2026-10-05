package terminal

import (
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

type panelKind uint8

const (
	homePanel panelKind = iota
	helpPanel
	statusPanel
	agentsPanel
	logsPanel
	diagnosisPanel
)

type menuCommand struct{ name, description, unavailable string }

// Commands route to the existing session actions. Views never start a second
// status poller, runtime, or log owner.
func (m *screen) commands() []menuCommand {
	commands := []menuCommand{{"status", "Show role health and addresses", ""}, {"logs", "Read this process's recent logs", ""}, {"doctor", "Check local configuration and files", ""}, {"help", "Show commands and keyboard controls", ""}}
	if m.state.role == config.RoleController {
		commands = append(commands, menuCommand{"agents", "Show Agent names, health and details", ""}, menuCommand{"clear", "Clear completed test output", ""}, menuCommand{"cancel", "Cancel the current inference request", ""})
	}
	if m.state.role == config.RoleWorker {
		commands = append(commands, menuCommand{"controller", "View or select the Controller address", ""}, menuCommand{"port", "Change runtime port and restart Agent", ""})
	}
	if m.state.role != 0 {
		commands = append(commands, menuCommand{"retry", "Retry after a startup or runtime failure", ""})
	}
	commands = append(commands, menuCommand{"roles", "Stop this role and choose another", ""}, menuCommand{"quit", "Stop this role and exit", ""})
	for i := range commands {
		c := &commands[i]
		switch c.name {
		case "status", "doctor":
			if m.state.role == 0 {
				c.unavailable = "Choose a role first."
			}
		case "clear":
			if m.requestBusy() {
				c.unavailable = "Wait for request cleanup before clearing output."
			}
		case "cancel":
			if !m.requestBusy() {
				c.unavailable = "There is no active request."
			}
		case "retry":
			r := m.state.agent.Report
			if m.state.phase != failed && !(m.state.phase == running && m.state.role == config.RoleWorker && r != nil && r.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY) {
				c.unavailable = "Retry is available after a failure."
			}
		case "controller":
			if m.state.network != app.Local && m.state.agent.Membership.Registered {
				c.unavailable = "Already connected. Stop this role before changing Controller."
			}
		}
	}
	return commands
}

func (m *screen) matchingCommands() []menuCommand {
	query := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(m.command.Value())), "/")
	var matches []menuCommand
	for _, c := range m.commands() {
		if strings.Contains(c.name, query) || strings.Contains(strings.ToLower(c.description), query) {
			matches = append(matches, c)
		}
	}
	return matches
}

func (m *screen) openPanel(panel panelKind) {
	m.panelKind, m.scroll = panel, 0
	m.menu, m.editing, m.logsPaused = false, false, false
	m.notice = ""
	if panel == logsPanel && m.console != nil {
		m.diagnostics = m.console.diagnostics.text()
	}
}

func (m *screen) activateCommand(name string) {
	for _, c := range m.commands() {
		if c.name == name && c.unavailable != "" {
			m.notice = c.unavailable
			return
		}
	}
	m.openPanel(homePanel)
	switch name {
	case "help":
		m.openPanel(helpPanel)
	case "status":
		m.openPanel(statusPanel)
	case "agents":
		m.openPanel(agentsPanel)
	case "logs":
		m.openPanel(logsPanel)
	case "doctor":
		m.openPanel(diagnosisPanel)
		m.send(action{kind: diagnose})
	case "clear":
		if m.cleanupNotice() != "" {
			m.forgottenCleanup = true
		}
		m.records = nil
		// A completed snapshot can be delivered again after clearing the view.
		m.clearedRequest = m.state.request.id
	case "cancel":
		notifyModel(m.controls.cancelRequest)
	case "roles":
		m.send(action{kind: returnToPicker})
	case "quit":
		notifyModel(m.controls.exit)
	case "retry":
		m.send(action{kind: retry})
	case "controller":
		if m.state.network == app.Local {
			m.openPanel(statusPanel)
			m.notice = "To change the local target, exit and run:\nmica-mesh agent --local\n  --controller-address 127.0.0.1:PORT"
			return
		}
		m.editing, m.input = true, changeAddress
		m.prompt.Reset()
	case "port":
		m.editing, m.input = true, changePort
		m.prompt.Reset()
	}
}

func (m *screen) menuKey(v tea.KeyPressMsg) {
	matches := m.matchingCommands()
	switch v.String() {
	case "esc":
		m.menu = false
		m.notice = ""
	case "up":
		m.menuSelection = max(0, m.menuSelection-1)
	case "down":
		m.menuSelection = min(max(0, len(matches)-1), m.menuSelection+1)
	case "tab":
		if len(matches) > 0 {
			m.command.SetValue(matches[min(m.menuSelection, len(matches)-1)].name)
			m.command.CursorEnd()
			m.menuSelection = 0
		}
	case "enter":
		if len(matches) > 0 {
			m.activateCommand(matches[min(m.menuSelection, len(matches)-1)].name)
		}
	case "/":
		if m.command.Value() == "" && m.state.role == config.RoleController && m.panelKind == homePanel && !m.requestBusy() {
			// A second slash starts a literal prompt, without command parsing.
			m.menu = false
			m.prompt.SetValue("/")
			m.prompt.CursorEnd()
		}
	default:
		m.updateCommand(v)
	}
}

func (m *screen) updateCommand(msg tea.Msg) {
	insertion := ""
	switch v := msg.(type) {
	case tea.PasteMsg:
		insertion = v.Content
	case tea.KeyPressMsg:
		insertion = v.Text
		if v.String() == "ctrl+j" {
			return
		}
	}
	if strings.ContainsAny(insertion, "\r\n\t") || !utf8.ValidString(insertion) || len(m.command.Value())-len(m.command.SelectedText())+len(insertion) > 64 {
		m.notice = "Use a command name of at most 64 bytes."
		return
	}
	m.command, _ = updatePrompt(m.command, msg)
	m.menuSelection = 0
	m.notice = ""
}
