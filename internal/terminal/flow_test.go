package terminal

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/doctor"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

func testEffects() sessionEffects {
	return sessionEffects{
		now:   time.Now,
		check: func(ctx context.Context, o Options) (Options, bool, error) { return o, false, nil },
		prepare: func(ctx context.Context, o Options, p func(setup.Progress) error) (config.Config, error) {
			return o.Config, nil
		},
		start: func(ctx context.Context, c config.Config, r config.Role, o app.Options) (*roleHandle, error) {
			child, cancel := context.WithCancel(ctx)
			return &roleHandle{endpoint: "127.0.0.1:1", snapshot: func() (app.AgentStatus, bool) {
				return app.AgentStatus{Report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}}, true
			}, wait: func() error { <-child.Done(); return nil }, close: func() error { cancel(); return nil }}, nil
		},
		connect: func(string) (*controllerConnection, error) {
			return &controllerConnection{status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
				return &meshv1.GetClusterStatusResponse{}, nil
			}, generate: func(ctx context.Context, r *meshv1.InferenceRequest, f func(*meshv1.InferenceEvent) error) error {
				return nil
			}, close: func() error { return nil }}, nil
		},
		interfaces: func() ([]discovery.InterfaceAddress, error) {
			return []discovery.InterfaceAddress{{Name: "en0", IPv4: "192.168.1.2"}}, nil
		},
		candidates: func(context.Context) ([]discovery.Candidate, error) { return nil, nil },
		diagnose: func(context.Context, config.Config, setup.Layout, doctor.Role, doctor.Options) (doctor.Report, error) {
			return doctor.Report{}, nil
		},
	}
}

type sessionHarness struct {
	s        *session
	box      *sessionMailbox
	controls controls
	ticks    chan time.Time
	done     chan error
	cancel   context.CancelFunc
}

func startSession(t *testing.T, o Options, e sessionEffects) *sessionHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &sessionHarness{box: newMailbox(), controls: newControls(), ticks: make(chan time.Time), done: make(chan error, 1), cancel: cancel}
	h.s = newSession(o, e, h.controls, h.box)
	go func() { h.done <- h.s.run(ctx, h.ticks) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Error("session did not join")
		}
	})
	return h
}
func (h *sessionHarness) await(t *testing.T, p func(sessionSnapshot) bool) sessionSnapshot {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		s := h.box.snapshot()
		if p(s) {
			return s
		}
		select {
		case <-h.box.wake:
		case <-timer.C:
			t.Fatalf("missing state; last %+v", s)
		}
	}
}
func (h *sessionHarness) act(a action) { h.controls.actions <- a }
func TestControllerSkipsModelSetup(t *testing.T) {
	e := testEffects()
	e.check = func(context.Context, Options) (Options, bool, error) {
		t.Error("Controller checked model")
		return Options{}, false, errors.New("not installed")
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleController, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == running })
}

func TestDiagnosisAfterFailedStartupStillReportsPortConflict(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := config.Default()
	cfg.ControllerListen = listener.Addr().String()
	cfg.WorkerListen = "127.0.0.1:0"
	e := testEffects()
	e.start = func(context.Context, config.Config, config.Role, app.Options) (*roleHandle, error) {
		return nil, errors.New("role failed to start")
	}
	e.diagnose = doctor.Run
	h := startSession(t, Options{Config: cfg, Role: config.RoleController, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == failed })
	h.act(action{kind: diagnose})
	h.await(t, func(s sessionSnapshot) bool {
		return strings.Contains(s.notice, "controller port · failed") && strings.Contains(s.notice, "--controller-listen")
	})
}

func TestLocalDiagnosisAcceptsEphemeralListeners(t *testing.T) {
	for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
		cfg := config.Default()
		cfg.ControllerListen, cfg.WorkerListen = "127.0.0.1:0", "127.0.0.1:0"
		if role == config.RoleWorker {
			cfg.ControllerAddress = "127.0.0.1:50051"
		}
		e := testEffects()
		e.diagnose = doctor.Run
		h := startSession(t, Options{Config: cfg, Role: role, Network: app.Local}, e)
		h.await(t, func(s sessionSnapshot) bool { return s.phase == running })
		h.act(action{kind: diagnose})
		s := h.await(t, func(s sessionSnapshot) bool { return s.notice != "" })
		if !strings.Contains(s.notice, "configuration · passed") {
			t.Errorf("role %d rejected valid local configuration: %s", role, s.notice)
		}
	}
}
func TestAgentConsentPrecedesEffects(t *testing.T) {
	e := testEffects()
	e.check = func(ctx context.Context, o Options) (Options, bool, error) { return o, true, nil }
	var prepares atomic.Int32
	e.prepare = func(ctx context.Context, o Options, p func(setup.Progress) error) (config.Config, error) {
		prepares.Add(1)
		return o.Config, nil
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == awaitingConsent })
	if prepares.Load() != 0 {
		t.Fatal("setup started without consent")
	}
	h.act(action{kind: consent})
	h.await(t, func(s sessionSnapshot) bool { return s.phase == running })
	if prepares.Load() != 1 {
		t.Fatal("consent did not prepare")
	}
}
func TestSetupFailureKeepsRecoveryAction(t *testing.T) {
	e := testEffects()
	e.check = func(ctx context.Context, o Options) (Options, bool, error) {
		return o, false, errors.New("bundle missing")
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, e)
	s := h.await(t, func(s sessionSnapshot) bool { return s.phase == failed })
	if s.notice == "" {
		t.Fatal("no recovery notice")
	}
	h.act(action{kind: returnToPicker})
	h.await(t, func(s sessionSnapshot) bool { return s.phase == picking })
}
func TestReturnToPickerStopsOnlyOwnedRole(t *testing.T) {
	e := testEffects()
	closed := make(chan struct{})
	joined := make(chan struct{})
	e.start = func(ctx context.Context, c config.Config, r config.Role, o app.Options) (*roleHandle, error) {
		return &roleHandle{endpoint: "local", wait: func() error { <-joined; return nil }, close: func() error { close(closed); <-joined; return nil }}, nil
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleController, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == running })
	h.act(action{kind: returnToPicker})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("role not closed")
	}
	if h.box.snapshot().phase == picking {
		t.Fatal("picker before join")
	}
	close(joined)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == picking })
}
func TestAmbiguousLANInterfaceOffersNamedChoices(t *testing.T) {
	e := testEffects()
	e.interfaces = func() ([]discovery.InterfaceAddress, error) {
		return []discovery.InterfaceAddress{{Name: "en0", IPv4: "192.168.1.2"}, {Name: "en1", IPv4: "10.1.0.2"}}, nil
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleController}, e)
	s := h.await(t, func(s sessionSnapshot) bool { return s.phase == choosingNetwork })
	if len(s.interfaces) != 2 || s.interfaces[1].Name != "en1" {
		t.Fatal("choices lost")
	}
}

func TestNoControllerKeepsRuntimeReadyAndOffersAddressEntry(t *testing.T) {
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, testEffects())
	s := h.await(t, func(s sessionSnapshot) bool { return s.agent.Report != nil })
	m := newScreen(Options{})
	m.state = s
	text := m.View().Content
	if !strings.Contains(text, "Runtime  Ready") || !strings.Contains(text, "Waiting for Controller") || !strings.Contains(text, "Controller address") {
		t.Fatal(text)
	}
}
func TestMultipleControllersRequireSelection(t *testing.T) {
	e := testEffects()
	choices := []discovery.Candidate{{InstanceID: "one", Hostname: "Mac One", Address: "192.168.1.3:50051"}, {InstanceID: "two", Hostname: "Mac Two", Address: "192.168.1.4:50051"}}
	e.candidates = func(context.Context) ([]discovery.Candidate, error) { return choices, nil }
	resolver := make(chan func(context.Context, string) (string, error), 1)
	e.start = func(ctx context.Context, c config.Config, r config.Role, o app.Options) (*roleHandle, error) {
		resolver <- o.ResolveController
		return testEffects().start(ctx, c, r, o)
	}
	o := Options{Config: config.Default(), Role: config.RoleWorker}
	o.Config.RuntimeBinary = "/runtime"
	o.Config.ModelPath = "/model"
	h := startSession(t, o, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == choosingNetwork })
	h.act(action{kind: acceptNetwork})
	h.await(t, func(s sessionSnapshot) bool { return s.phase == running })
	resolve := <-resolver
	if _, err := resolve(context.Background(), ""); err == nil {
		t.Fatal("ambiguous candidates connected automatically")
	}
	h.await(t, func(s sessionSnapshot) bool { return len(s.candidates) == 2 })
	h.act(action{kind: chooseController, value: choices[1].Address})
	h.await(t, func(s sessionSnapshot) bool { return strings.Contains(s.notice, "selected") })
	got, err := resolve(context.Background(), "")
	if err != nil || got != choices[1].Address {
		t.Fatalf("choice ignored: %s %v", got, err)
	}
}
func TestSetupCancelJoinsBeforePicker(t *testing.T) {
	e := testEffects()
	e.check = func(ctx context.Context, o Options) (Options, bool, error) { return o, true, nil }
	cleaning := make(chan struct{})
	release := make(chan struct{})
	e.prepare = func(ctx context.Context, o Options, _ func(setup.Progress) error) (config.Config, error) {
		<-ctx.Done()
		close(cleaning)
		<-release
		return o.Config, ctx.Err()
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == awaitingConsent })
	h.act(action{kind: consent})
	h.await(t, func(s sessionSnapshot) bool { return s.phase == preparing })
	notifyModel(h.controls.interrupt)
	select {
	case <-cleaning:
	case <-time.After(time.Second):
		t.Fatal("setup not canceled")
	}
	if h.box.snapshot().phase == picking {
		t.Fatal("picker overtook setup cleanup")
	}
	close(release)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == picking })
}
func TestOccupiedRuntimePortOffersPortChoice(t *testing.T) {
	e := testEffects()
	e.start = func(ctx context.Context, c config.Config, r config.Role, o app.Options) (*roleHandle, error) {
		p, err := testEffects().start(ctx, c, r, o)
		p.snapshot = func() (app.AgentStatus, bool) {
			return app.AgentStatus{Report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, LastError: "runtime port is occupied"}}, true
		}
		return p, err
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, e)
	s := h.await(t, func(s sessionSnapshot) bool { return s.agent.Report != nil })
	m := newScreen(Options{})
	m.state = s
	if !strings.Contains(m.View().Content, "Change runtime port") {
		t.Fatal("no runtime port recovery")
	}
}
func TestStatusFailureClearsCapacity(t *testing.T) {
	s := newSession(Options{}, testEffects(), newControls(), newMailbox())
	s.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{idleWorker()}}
	s.applyPoll(pollResult{started: time.Now(), err: errors.New("unreachable")})
	if s.state.status != nil {
		t.Fatal("kept stale ready capacity after failure")
	}
}
func TestCleanupCannotBeConfirmedAfterBudget(t *testing.T) {
	now := time.Now()
	e := testEffects()
	e.now = func() time.Time { return now.Add(11 * time.Second) }
	s := newSession(Options{}, e, newControls(), newMailbox())
	s.request = &requestOwner{view: requestView{id: "req", workerID: "selected", canceled: now, cleanup: cleanupChecking}}
	s.applyPoll(pollResult{started: now.Add(9 * time.Second), status: &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{idleWorker()}}})
	if s.request.snapshot().cleanup != cleanupUnconfirmed {
		t.Fatal("late response confirmed expired observation")
	}
}
func TestMissingStartedAndUnreachableStatusKeepCleanupUnconfirmed(t *testing.T) {
	now := time.Now()
	for _, unreachable := range []bool{false, true} {
		e := testEffects()
		e.now = func() time.Time { return now.Add(time.Second) }
		s := newSession(Options{}, e, newControls(), newMailbox())
		s.request = &requestOwner{view: requestView{id: "req", canceled: now, cleanup: cleanupChecking}}
		p := pollResult{started: now.Add(time.Second), status: &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{idleWorker()}}}
		if unreachable {
			p.err = errors.New("unreachable")
		}
		s.applyPoll(p)
		if s.request.snapshot().cleanup != cleanupChecking {
			t.Fatal("unproved cleanup changed state")
		}
		s.effects.now = func() time.Time { return now.Add(10 * time.Second) }
		s.observeCleanup()
		if s.request.snapshot().cleanup != cleanupUnconfirmed {
			t.Fatal("timeout erased uncertainty")
		}
	}
}

func TestLocalAgentAddressChangeGivesRestartAction(t *testing.T) {
	o := Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}
	o.Config.ControllerAddress = "127.0.0.1:50101"
	h := startSession(t, o, testEffects())
	s := h.await(t, func(s sessionSnapshot) bool { return s.agent.Report != nil })
	m := newScreen(o)
	m.state = s
	m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if m.editing {
		t.Fatal("local address offered ineffective live edit")
	}
	text := m.View().Content
	if !strings.Contains(text, "127.0.0.1:50101") || !strings.Contains(text, "--controller-address") {
		t.Fatalf("local target/restart action absent:\n%s", text)
	}
}

func TestCancelPendingSubmissionCannotExitRole(t *testing.T) {
	e := testEffects()
	e.connect = func(string) (*controllerConnection, error) {
		return &controllerConnection{close: func() error { return nil }, status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
			w := idleWorker()
			w.Model.Id = config.Default().Model
			return &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}, nil
		}, generate: func(ctx context.Context, r *meshv1.InferenceRequest, f func(*meshv1.InferenceEvent) error) error {
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	h := startSession(t, Options{Role: config.RoleController, Config: config.Default(), Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.status != nil })
	h.act(action{kind: submit, value: "prompt"})
	notifyModel(h.controls.cancelRequest)
	s := h.await(t, func(s sessionSnapshot) bool { return s.request.joined })
	if s.phase != running || s.request.cleanup != cleanupChecking {
		t.Fatalf("cancel closed role or dropped request: %+v", s)
	}
}
func TestAgentFileCheckPreservesExplicitSettings(t *testing.T) {
	o := Options{Config: config.Default(), Role: config.RoleWorker, Assets: config.AssetFields{RuntimeBinary: true, ModelPath: true}}
	o.Config.RuntimeBinary = "/manual/runtime"
	o.Config.ModelPath = "/manual/model"
	got, consent, err := checkAgentFiles(context.Background(), o)
	if err != nil || consent || got.Config.RuntimeBinary != o.Config.RuntimeBinary {
		t.Fatalf("manual assets altered: %+v %v", got, err)
	}
	o.Config.ModelPath = ""
	if _, consent, err := checkAgentFiles(context.Background(), o); err == nil || consent {
		t.Fatal("invalid explicit path offered download")
	}
}

func TestRequestDeadlineObservesCleanup(t *testing.T) {
	e := testEffects()
	e.connect = func(string) (*controllerConnection, error) {
		return &controllerConnection{close: func() error { return nil }, status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
			w := idleWorker()
			w.Model.Id = config.Default().Model
			return &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}, nil
		}, generate: func(ctx context.Context, r *meshv1.InferenceRequest, f func(*meshv1.InferenceEvent) error) error {
			<-ctx.Done()
			return ctx.Err()
		}}, nil
	}
	cfg := config.Default()
	cfg.Timeout = 20 * time.Millisecond
	h := startSession(t, Options{Role: config.RoleController, Config: cfg, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.status != nil })
	h.act(action{kind: submit, value: "prompt"})
	s := h.await(t, func(s sessionSnapshot) bool { return s.request.joined })
	if s.request.cleanup != cleanupChecking {
		t.Fatal("deadline did not retain cleanup uncertainty")
	}
}

func TestRecoveredStatusClearsUnavailableNotice(t *testing.T) {
	s := newSession(Options{}, testEffects(), newControls(), newMailbox())
	s.applyPoll(pollResult{started: time.Now(), err: errors.New("offline")})
	s.applyPoll(pollResult{started: time.Now(), status: &meshv1.GetClusterStatusResponse{}})
	if strings.Contains(s.state.notice, "unavailable") {
		t.Fatal("recovered status still reports failure")
	}
}
