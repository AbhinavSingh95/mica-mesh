package terminal

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/charmbracelet/x/ansi"
)

func TestRoleChromeAdaptsColorWithoutChangingMeaning(t *testing.T) {
	for _, role := range []config.Role{0, config.RoleController, config.RoleWorker} {
		m := newScreen(Options{Config: config.Default(), Role: role})
		m.Update(tea.BackgroundColorMsg{Color: color.Black})
		dark := m.View().Content
		m.Update(tea.BackgroundColorMsg{Color: color.White})
		light := m.View().Content
		colored := regexp.MustCompile(`\x1b\[[0-9;]*38[;:]`)
		if !colored.MatchString(dark) || !colored.MatchString(light) || dark == light {
			t.Errorf("role %v has no adaptive color", role)
		}
		if ansi.Strip(dark) != ansi.Strip(light) {
			t.Errorf("role %v changed meaning with background", role)
		}
	}
}

func TestControllerDistinguishesAgentsOnTheSameHost(t *testing.T) {
	m := readyScreen()
	a, b := idleWorker(), idleWorker()
	for _, w := range []*meshv1.WorkerInfo{a, b} {
		w.Model.Id = m.state.cfg.Model
		w.Hardware = &meshv1.HardwareInfo{Hostname: "Mac.lan"}
	}
	a.Endpoint, b.Endpoint = "127.0.0.1:50052", "127.0.0.1:50057"
	m.state.status.Workers = []*meshv1.WorkerInfo{a, b}
	text := ansi.Strip(m.View().Content)
	for _, want := range []string{a.Endpoint, b.Endpoint, "2 ready"} {
		if !strings.Contains(text, want) {
			t.Errorf("Agent identity or capacity missing %q:\n%s", want, text)
		}
	}
}

func TestAgentShowsDistinctRuntimeAndMembershipEndpoints(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.state.phase = running
	m.state.cfg.RuntimePort = 8081
	m.state.agent.Endpoint = "127.0.0.1:50057"
	m.state.agent.Membership.ControllerAddress = "127.0.0.1:50051"
	m.state.agent.Membership.Registered = true
	m.state.agent.Report = &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}
	text := ansi.Strip(m.View().Content)
	for _, want := range []string{"127.0.0.1:50057", "127.0.0.1:8081", "127.0.0.1:50051", "Connected"} {
		if !strings.Contains(text, want) {
			t.Errorf("Agent endpoint/state missing %q:\n%s", want, text)
		}
	}
}

func TestDownloadProgressReportsMeasuredBytes(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.state.phase = preparing
	m.state.progress = setup.Progress{Phase: setup.Downloading, CompletedBytes: 25000000, TotalBytes: 100000000}
	text := ansi.Strip(m.View().Content)
	for _, want := range []string{"25%", "25.0", "100.0", "MB"} {
		if !strings.Contains(text, want) {
			t.Errorf("measured progress missing %q:\n%s", want, text)
		}
	}
	m.state.progress.CompletedBytes = 99999999
	if strings.Contains(ansi.Strip(m.View().Content), "100%") {
		t.Fatal("incomplete download displayed 100 percent")
	}
	// Verification is separate work: downloaded bytes must not imply readiness.
	m.state.progress.Phase = setup.Verifying
	text = ansi.Strip(m.View().Content)
	if !strings.Contains(text, "Verifying") || strings.Contains(text, "Runtime  Ready") {
		t.Fatalf("verification presented as readiness:\n%s", text)
	}
}

func TestActivityChangesOnlyWhileWorkIsPending(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.state.now = time.Unix(100, 0)
	before := m.View().Content
	m.state.now = m.state.now.Add(250 * time.Millisecond)
	if before == m.View().Content {
		t.Error("pending startup has no activity feedback")
	}
	m.state.phase = running
	m.state.agent.Membership.Registered = true
	m.state.agent.Report = &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}
	before = m.View().Content
	m.state.now = m.state.now.Add(250 * time.Millisecond)
	if before != m.View().Content {
		t.Error("idle ready screen animates without work")
	}
}

func TestStyledEditorCursorTracksUnicodeAndResize(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {52, 18}, {80, 24}, {110, 32}} {
		m := readyScreen()
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.prompt.SetValue("hello 世界")
		v := m.View()
		if v.Cursor == nil {
			t.Fatalf("%v has no cursor", size)
		}
		lines := strings.Split(ansi.Strip(v.Content), "\n")
		if v.Cursor.Y < 0 || v.Cursor.Y >= len(lines) {
			t.Fatalf("%v cursor outside frame: %+v", size, v.Cursor)
		}
		line := lines[v.Cursor.Y]
		index := strings.Index(line, "hello 世界")
		if index < 0 || v.Cursor.X != lipgloss.Width(line[:index]+"hello 世界") {
			t.Errorf("%v cursor does not follow visible text: %+v line=%q", size, v.Cursor, line)
		}
		if lipgloss.Width(v.Content) > size[0] || lipgloss.Height(v.Content) > size[1] {
			t.Errorf("%v frame overflow", size)
		}
	}
}

func readyScreen() *screen {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
	m.state.phase = running
	m.state.statusAt = m.state.now
	w := idleWorker()
	w.Model.Id = m.state.cfg.Model
	m.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}
	return m
}

func TestRolePanelsFitSmallAndLargeTerminals(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {52, 18}, {60, 24}, {80, 24}, {110, 32}} {
		for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
			for _, panel := range []string{"roles", "network", "consent", "download", "help", "diagnosis", "edit", "running", "failed"} {
				t.Run(fmt.Sprintf("%v/%v/%s", size, role, panel), func(t *testing.T) {
					m := newScreen(Options{Config: config.Default(), Role: role})
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					m.state.phase = running
					switch panel {
					case "roles":
						m.state.phase = picking
					case "network":
						m.state.phase = choosingNetwork
						m.state.interfaces = []discovery.InterfaceAddress{{Name: "en0", IPv4: "192.168.1.20"}}
					case "consent":
						m.state.phase = awaitingConsent
						m.state.download = setup.Download{ID: "model", URL: "https://example.com/" + strings.Repeat("model/", 30), Path: "/model.gguf", SizeBytes: 100000000}
					case "download":
						m.state.phase = preparing
						m.state.progress = setup.Progress{Phase: setup.Downloading, CompletedBytes: 25000000, TotalBytes: 100000000}
					case "help":
						m.help = true
					case "diagnosis":
						m.diagnosis = true
						m.diagnostics = strings.Repeat("Diagnostic information\n", 40)
					case "edit":
						m.editing = true
						m.prompt.SetValue("127.0.0.1:50051")
					case "failed":
						m.state.phase = failed
						m.state.notice = strings.Repeat("Failure details ", 30) + "F5 Retry"
					}
					v := m.View()
					if lipgloss.Width(v.Content) > size[0] || lipgloss.Height(v.Content) > size[1] {
						t.Fatalf("frame overflow: %dx%d\n%s", lipgloss.Width(v.Content), lipgloss.Height(v.Content), v.Content)
					}
					text := ansi.Strip(v.Content)
					if !strings.Contains(text, "Ctrl-C") && !strings.Contains(text, "Back") && !strings.Contains(text, "F3 Roles") {
						t.Fatalf("exit or back action hidden:\n%s", text)
					}
					if panel == "download" && role == config.RoleWorker && !strings.Contains(text, "25%") {
						t.Fatalf("download progress hidden:\n%s", text)
					}
				})
			}
		}
	}
}

func TestEditorCursorFollowsWrappedAndMultilineText(t *testing.T) {
	for _, value := range []string{strings.Repeat("世界", 50) + "END", "first line\nsecond line\nEND"} {
		m := readyScreen()
		m.Update(tea.PasteMsg{Content: value})
		for _, size := range [][2]int{{80, 24}, {40, 12}, {110, 32}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			v := m.View()
			if v.Cursor == nil {
				t.Fatalf("%v has no cursor", size)
			}
			lines := strings.Split(ansi.Strip(v.Content), "\n")
			line := lines[v.Cursor.Y]
			index := strings.Index(line, "END")
			if index < 0 || v.Cursor.X != lipgloss.Width(line[:index]+"END") {
				t.Fatalf("%v cursor left visible insertion point: %+v line=%q editor=%+v\n%s\nEditor:\n%s", size, v.Cursor, line, m.prompt.Cursor(), v.Content, m.prompt.View())
			}
		}
	}
}

func TestCompactControllerKeepsStreamingTextAndHistory(t *testing.T) {
	m := readyScreen()
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.state.request = requestView{id: "stream"}
	m.records = []record{{id: "stream", prompt: "ORIGINAL INPUT", response: "first text\nlatest text", worker: "Mac", total: time.Second}}
	text := ansi.Strip(m.View().Content)
	for _, want := range []string{"latest text", "Generating", "Ctrl-C", "History"} {
		if !strings.Contains(text, want) {
			t.Errorf("compact stream missing %q:\n%s", want, text)
		}
	}
	for _, want := range []string{"first text", "Agent Mac", "ORIGINAL INPUT"} {
		m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
		if text := ansi.Strip(m.View().Content); !strings.Contains(text, want) {
			t.Fatalf("compact history skipped %q:\n%s", want, text)
		}
	}
}
