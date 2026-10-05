package terminal

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

type flow uint8

const (
	picking flow = iota
	checking
	choosingNetwork
	awaitingConsent
	preparing
	starting
	running
	stopping
	failed
)

type cleanupState uint8

const (
	cleanupNone cleanupState = iota
	cleanupChecking
	cleanupConfirmed
	cleanupUnconfirmed
)

type actionKind uint8

const (
	chooseRole actionKind = iota
	acceptNetwork
	consent
	retry
	returnToPicker
	diagnose
	chooseInterface
	chooseController
	changeAddress
	changePort
	submit
)

type action struct {
	kind  actionKind
	role  config.Role
	value string
}
type controls struct {
	actions                        chan action
	interrupt, exit, cancelRequest chan struct{}
}

func newControls() controls {
	return controls{make(chan action, 1), make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)}
}

type requestView struct {
	id, prompt, workerID, hostname, finish, failure string
	started, firstText, ended, canceled             time.Time
	joined                                          bool
	cleanup                                         cleanupState
	text                                            *textQueue
}
type sessionSnapshot struct {
	network                   app.NetworkMode
	role                      config.Role
	phase                     flow
	cfg                       config.Config
	endpoint, version, notice string
	status                    *meshv1.GetClusterStatusResponse
	statusAt, now             time.Time
	statusError               bool
	agent                     app.AgentStatus
	candidates                []discovery.Candidate
	interfaces                []discovery.InterfaceAddress
	progress                  setup.Progress
	download                  setup.Download
	request                   requestView
	submissions               uint64
}
type observationMsg struct{}
type record struct {
	id, prompt, response, worker, finish, failure string
	omitted                                       bool
	first, total                                  time.Duration
	cleanup                                       cleanupState
	done                                          bool
}
type screen struct {
	state                    sessionSnapshot
	width, height, selection int
	prompt                   textarea.Model
	progress                 progress.Model
	styles                   screenStyles
	controls                 controls
	mailbox                  *sessionMailbox
	console                  *Console
	help, diagnosis          bool
	input                    actionKind
	editing                  bool
	notice, diagnostics      string
	records                  []record
	forgottenCleanup         bool
	clean                    sanitizer
	scroll                   int
	pendingSubmission        uint64
	previousRequest          string
}

func newScreen(o Options) *screen {
	phase := picking
	if o.Role != 0 {
		phase = checking
	}
	m := &screen{state: sessionSnapshot{network: o.Network, role: o.Role, phase: phase, cfg: o.Config, version: "development", now: time.Now()}, width: 80, height: 24, prompt: newPrompt(), progress: progress.New(progress.WithoutPercentage()), controls: newControls()}
	m.setTheme(true)
	m.resizeEditor()
	return m
}
func (m *screen) Init() tea.Cmd { return tea.RequestBackgroundColor }

func (m *screen) resizeEditor() {
	width := max(1, m.width-4)
	if m.roomy() {
		width = max(1, width-4) // Border and one padding cell on each side.
	}
	m.prompt.SetWidth(width)
	m.prompt.SetHeight(min(3, max(1, m.height-24)))
	// Refresh wrapped viewport content before Bubbles positions the cursor.
	// Size setters alone retain the old scroll bounds. A nil update starts no
	// effects because this editor has no virtual cursor or viewport animation.
	m.prompt, _ = m.prompt.Update(nil)
	m.progress.SetWidth(min(40, max(1, m.width-8)))
}
func (m *screen) requestBusy() bool {
	r := m.state.request
	return r.id != "" && (!r.joined || r.cleanup == cleanupChecking)
}
func (m *screen) canSubmit() bool {
	if m.state.role != config.RoleController || m.state.phase != running || m.requestBusy() || m.state.status == nil || !capacityFresh(m.state.statusAt, m.state.now) {
		return false
	}
	for _, w := range m.state.status.Workers {
		if eligible(w, m.state.cfg.Model) {
			return true
		}
	}
	return false
}
func (m *screen) send(a action) bool {
	select {
	case m.controls.actions <- a:
		return true
	default:
		m.notice = "An action is in progress. Wait, then try again."
		return false
	}
}
func notifyModel(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (m *screen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.BackgroundColorMsg:
		m.setTheme(v.IsDark())
	case tea.WindowSizeMsg:
		m.width, m.height = v.Width, v.Height
		m.resizeEditor()
	case observationMsg:
		previous := m.state
		m.state = m.mailbox.snapshot()
		if m.pendingSubmission != 0 {
			if previous.role != m.state.role {
				m.pendingSubmission = 0
			} else if m.state.submissions < m.pendingSubmission {
				// Keep the draft frozen until its action has been consumed. A status
				// update can reach the mailbox before the pending submission does.
				m.state.request = previous.request
			} else {
				if m.state.request.id != "" && m.state.request.id != m.previousRequest {
					m.prompt.Reset()
				}
				m.pendingSubmission = 0
			}
		}
		if m.pendingSubmission == 0 {
			m.consume()
		}
		if previous.role != m.state.role || previous.phase != m.state.phase {
			m.selection = 0
		}
		choices := len(m.state.candidates)
		if m.state.phase == choosingNetwork {
			choices = len(m.state.interfaces)
		}
		m.selection = min(m.selection, max(0, choices-1))
	case diagnosticsMsg:
		if m.console != nil {
			m.diagnostics = m.console.diagnostics.text()
			m.notice = "Input or service notice. Press F2 for details."
			if strings.Contains(m.diagnostics, "Input rejected.") {
				m.notice = "Input rejected. Use at most 16 KiB per paste and shorter terminal sequences."
			}
		}
	case interruptMsg:
		m.interrupt()
	case tea.KeyPressMsg:
		key := v.String()
		if key == "ctrl+c" {
			m.interrupt()
			break
		}
		if key == "pgup" || key == "pgdown" {
			step := max(1, m.height-10)
			history := m.state.role == config.RoleController && !m.help && !m.diagnosis && !m.editing && m.state.phase != awaitingConsent && m.state.phase != choosingNetwork
			if history {
				_, _, _, rows := m.controllerLayout(max(0, m.height-4))
				step = max(1, rows)
			}
			if (key == "pgup") == history {
				m.scroll += step
			} else {
				m.scroll = max(0, m.scroll-step)
			}
			break
		}
		if key == "f1" {
			m.help = !m.help
			m.scroll = 0
			break
		}
		if key == "esc" {
			m.help = false
			m.diagnosis = false
			m.editing = false
			m.selection = 0
			break
		}
		if m.help {
			break
		}
		if key == "f2" {
			m.diagnosis = !m.diagnosis
			m.scroll = 0
			if m.diagnosis {
				m.send(action{kind: diagnose})
			}
			break
		}
		if key == "f3" {
			if m.state.phase != stopping {
				m.send(action{kind: returnToPicker})
			}
			break
		}
		if m.diagnosis {
			break
		}
		if m.editing {
			if key == "enter" {
				if m.send(action{kind: m.input, value: m.prompt.Value()}) {
					m.editing = false
					m.prompt.Reset()
				}
				break
			}
			var accepted bool
			m.prompt, accepted = updatePrompt(m.prompt, v)
			if !accepted {
				m.rejectEdit()
			} else {
				m.notice = ""
			}
			break
		}
		if m.state.phase == picking {
			switch key {
			case "up", "k", "down", "j", "tab":
				m.selection = 1 - m.selection
			case "enter":
				role := config.RoleController
				if m.selection == 1 {
					role = config.RoleWorker
				}
				if m.send(action{kind: chooseRole, role: role}) {
					m.state.role = role
					m.state.phase = checking
				}
			case "q":
				notifyModel(m.controls.exit)
			}
			break
		}
		if key == "f5" {
			m.send(action{kind: retry})
			break
		}
		if key == "f4" && m.state.role == config.RoleWorker && !m.state.agent.Membership.Registered {
			if m.state.network == app.Local {
				m.notice = "To change the local target, exit and run:\nmica-mesh agent --local\n  --controller-address 127.0.0.1:PORT"
				break
			}
			m.editing = true
			m.input = changeAddress
			m.prompt.Reset()
			break
		}
		if key == "f6" && m.state.role == config.RoleWorker {
			m.editing = true
			m.input = changePort
			m.prompt.Reset()
			break
		}
		choices := len(m.state.interfaces)
		if m.state.phase != choosingNetwork {
			choices = len(m.state.candidates)
		}
		if m.state.phase == choosingNetwork || m.state.role == config.RoleWorker && choices > 1 && !m.state.agent.Membership.Registered {
			switch key {
			case "up":
				m.selection = max(0, m.selection-1)
			case "down", "tab":
				m.selection = min(max(0, choices-1), m.selection+1)
			case "enter":
				if m.state.phase == choosingNetwork {
					if choices > 1 {
						m.send(action{kind: chooseInterface, value: m.state.interfaces[m.selection].IPv4})
					} else {
						m.send(action{kind: acceptNetwork})
					}
				} else if choices > 0 {
					m.send(action{kind: chooseController, value: m.state.candidates[m.selection].Address})
				}
			}
			break
		}
		if m.state.phase == awaitingConsent {
			if key == "y" || key == "enter" {
				m.send(action{kind: consent})
			}
			if key == "n" {
				m.send(action{kind: returnToPicker})
			}
			break
		}
		if m.state.role == config.RoleController && m.canSubmit() {
			if key == "enter" {
				prompt := m.prompt.Value()
				if strings.TrimSpace(prompt) == "" {
					m.notice = "Enter a prompt first."
					break
				}
				if m.send(action{kind: submit, value: prompt}) {
					m.pendingSubmission = m.state.submissions + 1
					m.previousRequest = m.state.request.id
					m.state.request = requestView{id: "pending"}
				}
				break
			}
			var accepted bool
			m.prompt, accepted = updatePrompt(m.prompt, v)
			if !accepted {
				m.rejectEdit()
			} else {
				m.notice = ""
			}
		} else if key == "enter" && m.state.role == config.RoleController {
			m.notice = "Wait for a ready Agent before sending a prompt."
		}
	case tea.PasteMsg:
		if m.editing || m.canSubmit() {
			var accepted bool
			m.prompt, accepted = updatePrompt(m.prompt, v)
			if !accepted {
				m.rejectEdit()
			} else {
				m.notice = ""
			}
		}
	}
	return m, nil
}
func (m *screen) rejectEdit() {
	m.notice = "Input rejected. A prompt can contain at most 16 KiB of UTF-8 text."
}
func (m *screen) interrupt() {
	if m.requestBusy() && m.state.request.cleanup == cleanupChecking {
		m.notice = "Cleanup is in progress. Wait for the result."
	}
	if m.requestBusy() {
		notifyModel(m.controls.cancelRequest)
	} else {
		notifyModel(m.controls.interrupt)
	}
}
func (m *screen) consume() {
	r := m.state.request
	if r.id == "" {
		return
	}
	if len(m.records) == 0 || m.records[len(m.records)-1].id != r.id {
		m.records = append(m.records, record{id: r.id, prompt: cleanText(r.prompt)})
		m.clean = sanitizer{}
		m.scroll = 0
	}
	rec := &m.records[len(m.records)-1]
	if r.text != nil {
		rec.response += m.clean.text(r.text.drain())
	}
	rec.worker = cleanText(r.hostname)
	rec.finish = r.finish
	rec.failure = cleanText(r.failure)
	rec.cleanup = r.cleanup
	rec.done = r.joined && r.cleanup != cleanupChecking
	if !r.firstText.IsZero() {
		rec.first = r.firstText.Sub(r.started)
	}
	end := r.ended
	if end.IsZero() {
		end = m.state.now
	}
	rec.total = end.Sub(r.started)
	m.trimHistory()
}

const transcriptLimit = 32 * 1024
const omission = "[Earlier text omitted.]\n"

func (m *screen) trimHistory() {
	bytes := func() int {
		n := 0
		for _, r := range m.records {
			n += len(r.prompt) + len(r.response)
			if r.omitted {
				n += len(omission)
			}
		}
		return n
	}
	for i := 0; i < len(m.records)-1 && bytes() > transcriptLimit; i++ {
		if m.records[i].done && m.records[i].response != "" {
			m.records[i].response = ""
			m.records[i].omitted = true
		}
	}
	for len(m.records) > 1 && (len(m.records) > 32 || bytes() > transcriptLimit) {
		if m.records[0].cleanup == cleanupUnconfirmed {
			m.forgottenCleanup = true
		}
		copy(m.records, m.records[1:])
		m.records[len(m.records)-1] = record{}
		m.records = m.records[:len(m.records)-1]
	}
	if len(m.records) > 0 && bytes() > transcriptLimit {
		r := &m.records[0]
		r.omitted = true
		keep := max(0, transcriptLimit-len(r.prompt)-len(omission))
		r.response = suffix(r.response, keep)
	}
}
func suffix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return strings.Clone(s[i:])
}
func cleanText(s string) string { var c sanitizer; return c.text(s) }
func eligible(w *meshv1.WorkerInfo, model string) bool {
	return w != nil && w.State == meshv1.WorkerState_WORKER_STATE_READY && w.Report != nil && w.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY && !w.Report.Active && w.Report.ActiveRequestId == nil && w.ReservedRequestId == nil && w.GetModel().GetId() == model && w.Capacity == 1 && w.HeartbeatAgeMilliseconds < 10000
}
func confirmsCleanup(w *meshv1.WorkerInfo, id string, canceled, started time.Time) bool {
	if id == "" || w == nil || w.WorkerId != id || canceled.IsZero() || !started.After(canceled) || w.State != meshv1.WorkerState_WORKER_STATE_READY || w.Report == nil || w.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_READY || w.Report.Active || w.Report.ActiveRequestId != nil || w.ReservedRequestId != nil {
		return false
	}
	elapsed := uint64(started.Sub(canceled) / time.Millisecond)
	return elapsed > 0 && w.HeartbeatAgeMilliseconds < elapsed
}
func duration(d time.Duration) string { return fmt.Sprintf("%.1f s", max(0, d.Seconds())) }
