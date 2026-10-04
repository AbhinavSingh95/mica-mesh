package terminal

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

func TestRolePickerShowsOnlyControllerAndAgent(t *testing.T) {
	text := newScreen(Options{Config: config.Default()}).View().Content
	for _, want := range []string{"Mica Mesh", "Controller", "Agent", "development"} {
		if !strings.Contains(text, want) {
			t.Errorf("picker missing %q: %s", want, text)
		}
	}
	for _, bad := range []string{"Try on this Mac", "Demo", "combined"} {
		if strings.Contains(text, bad) {
			t.Errorf("extra role %q", bad)
		}
	}
}
func TestDirectRoleSkipsPicker(t *testing.T) {
	for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
		text := newScreen(Options{Config: config.Default(), Role: role}).View().Content
		if strings.Contains(text, "Choose a role") || !strings.Contains(text, "Checking") {
			t.Errorf("direct role has no checking screen: %s", text)
		}
	}
}
func TestAgentHasNoInferencePrompt(t *testing.T) {
	text := newScreen(Options{Config: config.Default(), Role: config.RoleWorker}).View().Content
	if strings.Contains(text, "Enter a prompt") || strings.Contains(text, "Each prompt") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "Agent") {
		t.Fatal("missing Agent screen")
	}
}
func TestControllerWaitsForEligibleAgent(t *testing.T) {
	text := newScreen(Options{Config: config.Default(), Role: config.RoleController}).View().Content
	for _, want := range []string{"Each prompt is a new request.", "Waiting for an Agent", "Enter a prompt"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}
func idleWorker() *meshv1.WorkerInfo {
	return &meshv1.WorkerInfo{WorkerId: "selected", State: meshv1.WorkerState_WORKER_STATE_READY, Model: &meshv1.ModelDescriptor{Id: "model"}, Capacity: 1, Report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}}
}
func TestReadyComesFromControllerStatus(t *testing.T) {
	if !eligible(idleWorker(), "model") {
		t.Fatal("ready status rejected")
	}
	for _, change := range []func(*meshv1.WorkerInfo){func(w *meshv1.WorkerInfo) { w.State = 99 }, func(w *meshv1.WorkerInfo) { w.Report = nil }, func(w *meshv1.WorkerInfo) { w.Report.Active = true }, func(w *meshv1.WorkerInfo) { v := ""; w.ReservedRequestId = &v }, func(w *meshv1.WorkerInfo) { v := ""; w.Report.ActiveRequestId = &v }, func(w *meshv1.WorkerInfo) { w.Model.Id = "other" }, func(w *meshv1.WorkerInfo) { w.HeartbeatAgeMilliseconds = 10000 }} {
		w := idleWorker()
		change(w)
		if eligible(w, "model") {
			t.Fatalf("unavailable status enabled input: %v", w)
		}
	}
}
func TestDifferentIdleWorkerDoesNotConfirmCleanup(t *testing.T) {
	now := time.Now()
	w := idleWorker()
	if confirmsCleanup(w, "other", now, now.Add(time.Second)) {
		t.Fatal("different Agent proved cleanup")
	}
	if confirmsCleanup(w, "", now, now.Add(time.Second)) {
		t.Fatal("missing Started proved cleanup")
	}
}
func TestCleanupFreshnessUsesCallStartAndRoundedAge(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		age     uint64
		elapsed time.Duration
		want    bool
	}{{"fresh", 999, time.Second, true}, {"rounded stale", 1000, time.Second, false}, {"old call", 0, -time.Second, false}, {"same instant", 0, 0, false}, {"overflow", math.MaxUint64, time.Second, false}} {
		t.Run(tc.name, func(t *testing.T) {
			w := idleWorker()
			w.HeartbeatAgeMilliseconds = tc.age
			if got := confirmsCleanup(w, "selected", now, now.Add(tc.elapsed)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestControllerEditorHasVisibleCursor(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
	m.state.phase = running
	m.state.now = time.Now()
	m.state.statusAt = m.state.now
	w := idleWorker()
	w.Model.Id = m.state.cfg.Model
	m.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}
	m.prompt.SetValue("hello")
	if m.View().Cursor == nil {
		t.Fatal("focused prompt has no terminal cursor")
	}
}
func TestStatusRefreshPreservesControllerSelection(t *testing.T) {
	m := newScreen(Options{Role: config.RoleWorker})
	m.selection = 1
	m.state.phase = running
	m.mailbox = newMailbox()
	m.mailbox.publish(sessionSnapshot{role: config.RoleWorker, phase: running, candidates: []discovery.Candidate{{Address: "a"}, {Address: "b"}}})
	m.Update(observationMsg{})
	if m.selection != 1 {
		t.Fatal("poll resets keyboard choice")
	}
}
func TestCompletionCannotOvertakeAcceptedText(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController})
	m.mailbox = newMailbox()
	q := newTextQueue(m.mailbox.wake)
	if err := q.put(context.Background(), "first\x1b]52;blocked"); err != nil {
		t.Fatal(err)
	}
	m.mailbox.publish(sessionSnapshot{role: config.RoleController, request: requestView{id: "one", prompt: "prompt", text: q}})
	m.Update(observationMsg{})
	if err := q.put(context.Background(), "payload\a last"); err != nil {
		t.Fatal(err)
	}
	m.mailbox.publish(sessionSnapshot{role: config.RoleController, request: requestView{id: "one", text: q, joined: true, finish: "stop"}})
	m.Update(observationMsg{})
	r := m.records[0]
	if !r.done || r.response != "first last" {
		t.Fatalf("terminal state passed pending text or control: %+v", r)
	}
}
func TestOversizedPromptHasVisibleRejection(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController, Config: config.Default()})
	m.editing = true
	m.prompt.SetValue(strings.Repeat("a", 16384))
	m.Update(tea.PasteMsg{Content: "x"})
	if len(m.prompt.Value()) != 16384 || !strings.Contains(m.notice, "Input rejected") {
		t.Fatal("excess edit silently changed input")
	}
}

func TestControllerLayoutKeepsPromptAndControls(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {52, 18}, {80, 24}} {
		m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.state.phase = running
		m.state.now = time.Now()
		m.state.statusAt = m.state.now
		w := idleWorker()
		w.Model.Id = m.state.cfg.Model
		m.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}
		m.records = []record{{id: "one", prompt: "request", response: strings.Repeat("answer\n", 30), done: true}}
		v := m.View()
		for _, text := range []string{"Each prompt is a new request.", "Enter a prompt", "Enter Send", "Ctrl-C"} {
			if !strings.Contains(v.Content, text) {
				t.Errorf("%dx%d hid %q:\n%s", size[0], size[1], text, v.Content)
			}
		}
		if v.Cursor == nil {
			t.Errorf("%dx%d hid editable cursor", size[0], size[1])
		}
		if lipgloss.Height(v.Content) > size[1] || lipgloss.Width(v.Content) > size[0] {
			t.Errorf("view escaped %dx%d: %dx%d", size[0], size[1], lipgloss.Width(v.Content), lipgloss.Height(v.Content))
		}
	}
}
func TestAgentCompactRetainsStateAndControls(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.state.phase = running
	m.state.agent.Report = &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}
	text := m.View().Content
	for _, want := range []string{"Runtime  Ready", "Waiting for Controller", "Ctrl-C", "F4"} {
		if !strings.Contains(text, want) {
			t.Errorf("compact Agent lost %q:\n%s", want, text)
		}
	}
}
func TestCleanupNoticeSurvivesNewResponseAndCompactView(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.forgottenCleanup = true
	m.records = []record{{id: "new", prompt: "new request", response: strings.Repeat("latest\n", 50)}}
	if !strings.Contains(m.View().Content, "Cleanup unconfirmed") {
		t.Fatal("old uncertainty hidden behind new output")
	}
}
func TestPageKeysScrollRetainedResponse(t *testing.T) {
	m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
	m.records = []record{{id: "one", response: "FIRST LINE\n" + strings.Repeat("middle\n", 30) + "LAST LINE"}}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.scroll == 0 {
		t.Fatal("retained response cannot be inspected")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if m.scroll != 0 {
		t.Fatal("cannot return to newest text")
	}
}

func TestPromptCancelUsesRequestControl(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController})
	m.state.request = requestView{id: "pending"}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case <-m.controls.cancelRequest:
	default:
		t.Fatal("pending request cancellation can become idle exit")
	}
	select {
	case <-m.controls.interrupt:
		t.Fatal("request cancel reached idle-role exit channel")
	default:
	}
}

func TestAcceptedEditClearsRejectionNotice(t *testing.T) {
	m := newScreen(Options{})
	m.editing = true
	m.rejectEdit()
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if strings.Contains(m.notice, "Input rejected") {
		t.Fatal("accepted input still shows old rejection")
	}
}

func TestQueuedPromptSurvivesStatusChangeBeforeAdmission(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			now := time.Now()
			o := Options{Config: config.Default(), Role: config.RoleController}
			m := newScreen(o)
			m.mailbox = newMailbox()
			e := testEffects()
			e.now = func() time.Time { return now }
			s := newSession(o, e, m.controls, m.mailbox)
			s.roleCtx = context.Background()
			s.client = &controllerConnection{generate: func(context.Context, *meshv1.InferenceRequest, func(*meshv1.InferenceEvent) error) error { return nil }}
			w := idleWorker()
			w.Model.Id = o.Config.Model
			s.state.phase = running
			s.state.statusAt = now
			s.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}
			s.publish()
			m.Update(observationMsg{})
			m.prompt.SetValue("keep my draft")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			a := <-m.controls.actions
			// A queued status observation must not unlock editing or double submit.
			s.publish()
			m.Update(observationMsg{})
			m.Update(tea.PasteMsg{Content: "hidden"})
			if !m.requestBusy() || m.prompt.Value() != "keep my draft" {
				t.Fatalf("pending draft lost or unlocked: %q busy=%v", m.prompt.Value(), m.requestBusy())
			}
			if !accepted {
				now = now.Add(3 * time.Second)
			}
			s.submit(a.value)
			m.Update(observationMsg{})
			if accepted {
				if m.prompt.Value() != "" || len(m.records) != 1 || m.records[0].prompt != "keep my draft" {
					t.Fatal("accepted draft did not move into transcript")
				}
				<-s.request.done
				s.request.cancel()
			} else if m.prompt.Value() != "keep my draft" || m.requestBusy() || len(m.records) != 0 {
				t.Fatal("rejected draft was lost or became a request")
			}
		})
	}
}
