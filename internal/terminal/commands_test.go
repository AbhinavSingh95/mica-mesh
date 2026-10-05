package terminal

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/charmbracelet/x/ansi"
)

func typeKeys(m *screen, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestSlashMenuWorksInBothRolesWithoutCapacity(t *testing.T) {
	for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
		m := newScreen(Options{Config: config.Default(), Role: role})
		m.state.phase = running
		typeKeys(m, "/")
		if text := ansi.Strip(m.View().Content); !strings.Contains(text, "/logs") || !strings.Contains(text, "Commands") {
			t.Fatalf("role %d has no command menu: %s", role, text)
		}
		typeKeys(m, "status")
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if text := ansi.Strip(m.View().Content); !strings.Contains(text, "STATUS") {
			t.Fatalf("role %d did not open status: %s", role, text)
		}
		if len(m.controls.actions) != 0 {
			t.Fatal("view navigation dispatched application work")
		}
	}
}

func TestSlashLogsPreserveDraftAndNeverBecomeInference(t *testing.T) {
	m := readyScreen()
	m.prompt.SetValue("unfinished prompt")
	m.diagnostics = "level=INFO msg=\"Agent connected\"\n"
	m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	typeKeys(m, "/logs")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "LOGS") || !strings.Contains(text, "Agent connected") {
		t.Fatalf("logs did not open: %s", text)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.prompt.Value() != "unfinished prompt" || len(m.controls.actions) != 0 {
		t.Fatal("navigation changed the draft or dispatched inference")
	}
}

func TestUnknownSlashCommandIsNotSubmitted(t *testing.T) {
	m := readyScreen()
	typeKeys(m, "/unknown")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.controls.actions) != 0 {
		t.Fatal("unknown slash command became inference")
	}
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "No matching command") {
		t.Fatalf("unknown command has no next step: %s", text)
	}
}

func TestHomeLeavesDetailedAddressesBehindStatus(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.state.phase = running
	m.state.agent.Hostname = "studio-mac"
	text := ansi.Strip(m.View().Content)
	if strings.Contains(text, "Runtime address") || strings.Contains(text, "Agent listen") || !strings.Contains(text, "/ Commands") {
		t.Fatalf("home still contains the detailed dashboard: %s", text)
	}
	typeKeys(m, "/status")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if text = ansi.Strip(m.View().Content); !strings.Contains(text, "Runtime address") || !strings.Contains(text, "Agent listen") || !strings.Contains(text, "Host  studio-mac") {
		t.Fatalf("status lost role details: %s", text)
	}
}

func TestSlashLiteralPasteAndEscapePreserveInput(t *testing.T) {
	for _, pasted := range []bool{false, true} {
		m := readyScreen()
		want := "/quit"
		if pasted {
			m.Update(tea.PasteMsg{Content: want})
		} else {
			typeKeys(m, "/")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.prompt.Value() != "" {
				t.Fatal("canceling the menu changed an empty draft")
			}
			typeKeys(m, "//quit")
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		select {
		case action := <-m.controls.actions:
			if action.kind != submit || action.value != want {
				t.Fatalf("literal input changed: %+v", action)
			}
		default:
			t.Fatal("literal slash prompt was not submitted")
		}
		if len(m.controls.exit) != 0 {
			t.Fatal("literal text executed a command")
		}
	}
}

func TestSlashMenuDoesNotOwnRequestLifetime(t *testing.T) {
	m := readyScreen()
	m.state.request = requestView{id: "active", started: time.Now()}
	typeKeys(m, "/logs")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if len(m.controls.cancelRequest) != 1 || len(m.controls.interrupt) != 0 || len(m.controls.exit) != 0 {
		t.Fatal("cancel from logs changed the role lifetime")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	typeKeys(m, "/clear")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.menu || !m.requestBusy() || !strings.Contains(ansi.Strip(m.View().Content), "cleanup") {
		t.Fatal("clear hid an active request")
	}
}

func TestClearPreservesCleanupWarningAndDoesNotRestoreOldOutput(t *testing.T) {
	m := readyScreen()
	m.state.request = requestView{id: "done", joined: true, cleanup: cleanupUnconfirmed}
	m.records = []record{{id: "done", prompt: "old prompt", response: "old response", cleanup: cleanupUnconfirmed, done: true}}
	typeKeys(m, "/clear")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.consume()
	text := ansi.Strip(m.View().Content)
	if len(m.records) != 0 || strings.Contains(text, "old response") || !strings.Contains(text, "Cleanup unconfirmed") {
		t.Fatalf("clear changed cleanup state or restored output: %s", text)
	}
}

func TestCommandSearchIsBoundedAndSupportsSelectionReplacement(t *testing.T) {
	m := readyScreen()
	typeKeys(m, "/")
	m.Update(tea.PasteMsg{Content: strings.Repeat("x", 65)})
	if m.command.Value() != "" {
		t.Fatal("oversized command search was accepted")
	}
	m.Update(tea.PasteMsg{Content: strings.Repeat("x", 64)})
	m.command.SelectAll()
	typeKeys(m, "logs")
	if m.command.Value() != "logs" {
		t.Fatal("valid replacement was rejected")
	}
	m.Update(tea.PasteMsg{Content: "/quit\n"})
	if m.command.Value() != "logs" || len(m.controls.exit) != 0 {
		t.Fatal("multiline paste executed or changed a command")
	}
}

func TestCommandAndDetailViewsFitAndKeepCursor(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {52, 18}, {80, 24}, {110, 32}} {
		for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
			for _, command := range []string{"", "missing", "logs", "status", "doctor", "help", "retry"} {
				m := readyScreen()
				m.state.role = role
				m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
				typeKeys(m, "/"+command)
				for _, open := range []bool{false, true} {
					if open && command != "" {
						m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					}
					v := m.View()
					if lipgloss.Width(v.Content) > size[0] || lipgloss.Height(v.Content) > size[1] || !strings.Contains(ansi.Strip(v.Content), "Esc Back") {
						t.Fatalf("%v role %d /%s open=%v lost bounds or back control:\n%s", size, role, command, open, ansi.Strip(v.Content))
					}
					if m.menu {
						if v.Cursor == nil || v.Cursor.Y >= size[1]-1 || v.Cursor.X >= size[0]-2 {
							t.Fatal("command search lost its visible cursor")
						}
						lines := strings.Split(ansi.Strip(v.Content), "\n")
						if !strings.Contains(lines[v.Cursor.Y], "/"+command) {
							t.Fatalf("cursor left command input: %+v\n%s", v.Cursor, v.Content)
						}
					}
				}
			}
		}
	}
}

func TestLogsPauseWhileReadingAndResumeLatestOutput(t *testing.T) {
	m := readyScreen()
	m.console = &Console{}
	for i := range 300 {
		fmt.Fprintf(m.console.Diagnostics(), "log line %03d\n", i)
	}
	m.Update(diagnosticsMsg{})
	m.activateCommand("logs")
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "log line 299") || strings.Contains(text, "log line 000") {
		t.Fatalf("logs did not start at the latest output: %s", text)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	before := m.View().Content
	fmt.Fprintln(m.console.Diagnostics(), "newest line")
	m.Update(diagnosticsMsg{})
	if m.View().Content != before {
		t.Fatal("new output moved the paused log view")
	}
	// Admission can arrive after a user has already opened and paused logs.
	m.mailbox = newMailbox()
	next := m.state
	next.request = requestView{id: "new-request", prompt: "test prompt"}
	m.mailbox.publish(next)
	m.Update(observationMsg{})
	if m.View().Content != before {
		t.Fatal("request observation moved the paused log view")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "newest line") || !strings.Contains(text, "Live") {
		t.Fatalf("logs did not resume: %s", text)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if strings.Contains(m.View().Content, "log line") || strings.Contains(m.View().Content, "newest line") {
		t.Fatal("logs leaked into the home screen")
	}
}

func TestCommandMenuCanReachLastItemByKeyboard(t *testing.T) {
	m := readyScreen()
	typeKeys(m, "/")
	for range len(m.commands()) {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "/quit") {
		t.Fatal("menu did not scroll to the final command")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.command.Value() != "quit" || len(m.controls.exit) != 0 {
		t.Fatal("completion executed a command")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.controls.exit) != 1 {
		t.Fatal("explicit quit did not request owned shutdown")
	}
}

func TestCleanupFeedbackRemainsVisibleInDetails(t *testing.T) {
	for _, command := range []string{"status", "agents", "logs", "help", "doctor"} {
		m := readyScreen()
		m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
		m.state.request = requestView{id: "canceling", cleanup: cleanupChecking}
		m.activateCommand(command)
		m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		v := m.View()
		if !strings.Contains(ansi.Strip(v.Content), "Cleanup is in progress.") || !strings.Contains(ansi.Strip(v.Content), "Esc Back") || lipgloss.Height(v.Content) > 12 {
			t.Fatalf("/%s hid cleanup feedback or navigation: %s", command, ansi.Strip(v.Content))
		}
		if len(m.controls.cancelRequest) != 1 || len(m.controls.interrupt) != 0 {
			t.Fatal("cleanup feedback changed request ownership")
		}
	}
}

func TestCompactLogsKeepControlsWithInputRejection(t *testing.T) {
	m := readyScreen()
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.activateCommand("logs")
	m.console = &Console{}
	fmt.Fprintln(m.console.Diagnostics(), "Input rejected.")
	m.Update(diagnosticsMsg{})
	v := m.View()
	text := ansi.Strip(v.Content)
	if !strings.Contains(text, "Input rejected.") || !strings.Contains(text, "Esc Back") || !strings.Contains(text, "Ctrl-C") || lipgloss.Height(v.Content) > 12 {
		t.Fatalf("compact logs lost feedback or controls: %s", text)
	}
}
