package terminal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/doctor"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Options carries the parsed role and connection policy. A zero Role opens the
// picker. Agent resolves an absent Layout or Release only when it needs assets.
type Options struct {
	Config  config.Config
	Assets  config.AssetFields
	Layout  setup.Layout
	Release *setup.Release
	Role    config.Role
	Network app.NetworkMode
}

// Run owns one role at a time inside the Console's single program lifetime.
func Run(ctx context.Context, o Options, c *Console) error {
	if o.Role != 0 && o.Role != config.RoleController && o.Role != config.RoleWorker {
		return errors.New("choose one role: Controller or Agent")
	}
	model := newScreen(o)
	model.console = c
	box := newMailbox()
	model.mailbox = box
	s := newSession(o, productionEffects(), model.controls, box)
	return c.run(ctx, model, func(ctx context.Context, p *tea.Program) error {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		bridge, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-bridge.Done():
					return
				case <-box.wake:
					p.Send(observationMsg{})
				}
			}
		}()
		err := s.run(ctx, ticker.C)
		stop()
		<-done
		return err
	})
}

// textQueue charges accepted bytes until the model consumes them.
type textQueue struct {
	mu    sync.Mutex
	data  [32 * 1024]byte
	n     int
	space chan struct{}
	wake  chan struct{}
}

func newTextQueue(wake chan struct{}) *textQueue {
	return &textQueue{space: make(chan struct{}, 1), wake: wake}
}
func (q *textQueue) put(ctx context.Context, text string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.mu.Lock()
		if len(text) <= len(q.data)-q.n {
			q.n += copy(q.data[q.n:], text)
			q.mu.Unlock()
			notifyModel(q.wake)
			return nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.space:
		}
	}
}
func (q *textQueue) drain() string {
	q.mu.Lock()
	text := string(q.data[:q.n])
	q.n = 0
	q.mu.Unlock()
	notifyModel(q.space)
	return text
}

type sessionMailbox struct {
	mu    sync.Mutex
	state sessionSnapshot
	wake  chan struct{}
}

func newMailbox() *sessionMailbox                   { return &sessionMailbox{wake: make(chan struct{}, 1)} }
func (m *sessionMailbox) snapshot() sessionSnapshot { m.mu.Lock(); defer m.mu.Unlock(); return m.state }
func (m *sessionMailbox) publish(s sessionSnapshot) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
	notifyModel(m.wake)
}
func capacityFresh(at, now time.Time) bool {
	return !at.IsZero() && !now.Before(at) && now.Sub(at) < 3*time.Second
}

// The coordinator owns role transitions. Operation channels hold one terminal
// result; replaceable progress lives in observations. No effect runs in tea.Cmd.
type operationResult struct {
	kind         flow
	options      Options
	needsConsent bool
	process      *roleHandle
	interfaces   []discovery.InterfaceAddress
	diagnosis    string
	err          error
}
type pollResult struct {
	started time.Time
	status  *meshv1.GetClusterStatusResponse
	agent   app.AgentStatus
	err     error
}
type observations struct {
	mu         sync.Mutex
	progress   setup.Progress
	candidates []discovery.Candidate
	generation uint64
	wake       chan struct{}
}
type requestOwner struct {
	mu     sync.Mutex
	view   requestView
	cancel context.CancelFunc
	done   chan error
}

func (r *requestOwner) snapshot() requestView { r.mu.Lock(); defer r.mu.Unlock(); return r.view }

type session struct {
	options      Options
	state        sessionSnapshot
	effects      sessionEffects
	controls     controls
	mailbox      *sessionMailbox
	roleCtx      context.Context
	cancelRole   context.CancelFunc
	operation    chan operationResult
	process      *roleHandle
	processDone  chan error
	client       *controllerConnection
	poll         chan pollResult
	lastPoll     time.Time
	request      *requestOwner
	requestWake  chan struct{}
	observations observations
	choiceMu     sync.Mutex
	choice       string
	generation   uint64
	needsConsent bool
}

func newSession(o Options, e sessionEffects, c controls, m *sessionMailbox) *session {
	phase := picking
	if o.Role != 0 {
		phase = checking
	}
	return &session{options: o, state: sessionSnapshot{network: o.Network, role: o.Role, phase: phase, cfg: o.Config, version: "development", now: e.now()}, effects: e, controls: c, mailbox: m, requestWake: make(chan struct{}, 1), observations: observations{wake: make(chan struct{}, 1)}}
}
func (s *session) publish() {
	s.state.now = s.effects.now()
	if s.request != nil {
		s.state.request = s.request.snapshot()
	}
	s.mailbox.publish(s.state)
}
func (s *session) run(ctx context.Context, ticks <-chan time.Time) (result error) {
	defer func() { result = errors.Join(result, s.stopRole()) }()
	if s.options.Role != 0 {
		s.beginRole(ctx, s.options.Role)
	} else {
		s.publish()
	}
	for {
		var requestDone <-chan error
		if s.request != nil && !s.request.snapshot().joined {
			requestDone = s.request.done
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.controls.exit:
			return nil
		case <-s.controls.cancelRequest:
			// Enter and Ctrl-C can arrive before the coordinator sees either. Process
			// the accepted submission first; this request control can never exit a role.
			select {
			case a := <-s.controls.actions:
				if err := s.handle(ctx, a); err != nil {
					return err
				}
			default:
			}
			if s.request != nil && !s.request.snapshot().joined {
				s.cancelRequest()
			} else {
				s.state.notice = "The request has ended. Cleanup status remains shown."
				s.publish()
			}
		case <-s.controls.interrupt:
			if s.request != nil && !s.request.snapshot().joined {
				s.cancelRequest()
				break
			}
			if s.request != nil && s.request.snapshot().cleanup == cleanupChecking {
				s.state.notice = "Cleanup is in progress. Wait for the result."
				s.publish()
				break
			}
			if s.state.phase == checking || s.state.phase == preparing || s.state.phase == awaitingConsent {
				if err := s.toPicker(); err != nil {
					return err
				}
				break
			}
			return nil
		case a := <-s.controls.actions:
			if err := s.handle(ctx, a); err != nil {
				return err
			}
		case op := <-s.operation:
			s.operation = nil
			s.completeOperation(op)
		case err := <-s.processDone:
			s.processDone = nil
			closeErr := s.stopRole()
			s.fail(errors.Join(err, closeErr, errors.New("role stopped; press F5 to retry or F3 to choose a role")))
		case p := <-s.poll:
			s.poll = nil
			s.applyPoll(p)
		case err := <-requestDone:
			r := s.request
			r.mu.Lock()
			r.view.joined = true
			r.view.ended = s.effects.now()
			if err != nil && r.view.canceled.IsZero() {
				r.view.failure = err.Error()
				code := status.Code(err)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || code == codes.Canceled || code == codes.DeadlineExceeded {
					r.view.canceled = s.effects.now()
					deadline := r.view.started.Add(s.options.Config.Timeout)
					if (errors.Is(err, context.DeadlineExceeded) || code == codes.DeadlineExceeded) && deadline.Before(r.view.canceled) {
						r.view.canceled = deadline
					}
					r.view.cleanup = cleanupChecking
				}
			}
			r.mu.Unlock()
			r.cancel()
			s.observeCleanup()
			s.publish()
		case <-s.requestWake:
			s.publish()
		case <-s.observations.wake:
			s.observations.mu.Lock()
			if s.observations.generation == s.generation {
				s.state.progress = s.observations.progress
				s.state.candidates = append([]discovery.Candidate(nil), s.observations.candidates...)
			}
			s.observations.mu.Unlock()
			s.publish()
		case <-ticks:
			s.state.now = s.effects.now()
			s.observeCleanup()
			if !capacityFresh(s.state.statusAt, s.state.now) {
				s.state.status = nil
			}
			s.startPoll()
			s.publish()
		}
	}
}
func (s *session) beginRole(parent context.Context, role config.Role) {
	s.generation++
	s.options.Role = role
	s.state = sessionSnapshot{network: s.options.Network, role: role, phase: checking, cfg: s.options.Config, version: "development"}
	s.roleCtx, s.cancelRole = context.WithCancel(parent)
	s.choiceMu.Lock()
	s.choice = ""
	s.choiceMu.Unlock()
	s.observations.mu.Lock()
	s.observations.generation = s.generation
	s.observations.candidates = nil
	s.observations.progress = setup.Progress{}
	s.observations.mu.Unlock()
	s.operation = make(chan operationResult, 1)
	out := s.operation
	ctx := s.roleCtx
	o := s.options
	e := s.effects
	go func() {
		r := operationResult{kind: checking, options: o}
		if o.Network == app.LAN {
			r.interfaces, r.err = e.interfaces()
			if r.err != nil {
				out <- r
				return
			}
			host, _, _ := net.SplitHostPort(s.roleListener(o))
			filtered := r.interfaces[:0]
			for _, v := range r.interfaces {
				if o.Config.AdvertiseAddress != "" && v.IPv4 != o.Config.AdvertiseAddress {
					continue
				}
				if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() && ip.String() != v.IPv4 {
					continue
				}
				filtered = append(filtered, v)
			}
			r.interfaces = filtered
			if len(filtered) == 0 {
				r.err = errors.New("no usable LAN interface; connect to your LAN, then press F5 to retry")
				out <- r
				return
			}
		}
		if o.Role == config.RoleWorker {
			r.options, r.needsConsent, r.err = e.check(ctx, o)
		}
		out <- r
	}()
	s.publish()
}
func (s *session) roleListener(o Options) string {
	if o.Role == config.RoleController {
		return o.Config.ControllerListen
	}
	return o.Config.WorkerListen
}
func (s *session) completeOperation(r operationResult) {
	if r.err != nil {
		s.fail(r.err)
		return
	}
	switch r.kind {
	case checking:
		s.options = r.options
		s.state.cfg = r.options.Config
		s.state.interfaces = r.interfaces
		s.state.download = setup.ModelDownload(s.options.Layout)
		if s.options.Release != nil {
			s.state.version = s.options.Release.Version
		}
		if s.options.Network == app.LAN {
			s.state.phase = choosingNetwork
			if len(r.interfaces) == 1 {
				s.options.Config.AdvertiseAddress = r.interfaces[0].IPv4
				s.state.cfg = s.options.Config
			}
			s.needsConsent = r.needsConsent
		} else if r.needsConsent {
			s.state.phase = awaitingConsent
		} else {
			s.startRole()
		}
	case preparing:
		s.options.Config = r.options.Config
		s.state.cfg = r.options.Config
		s.startRole()
	case starting:
		s.process = r.process
		s.state.endpoint = r.process.endpoint
		s.processDone = make(chan error, 1)
		out := s.processDone
		p := r.process
		go func() { out <- p.wait() }()
		if s.options.Role == config.RoleController {
			var err error
			s.client, err = s.effects.connect(r.process.endpoint)
			if err != nil {
				s.fail(err)
				return
			}
		}
		s.state.phase = running
		s.lastPoll = time.Time{}
		s.startPoll()
	default:
		s.state.notice = r.diagnosis
	}
	s.publish()
}
func (s *session) startRole() {
	s.state.phase = starting
	s.state.notice = ""
	s.operation = make(chan operationResult, 1)
	out := s.operation
	o := s.options
	ctx := s.roleCtx
	e := s.effects
	generation := s.generation
	resolver := func(ctx context.Context, explicit string) (string, error) {
		s.choiceMu.Lock()
		choice := s.choice
		s.choiceMu.Unlock()
		if choice != "" {
			return choice, nil
		}
		if explicit != "" {
			return explicit, nil
		}
		choices, err := e.candidates(ctx)
		s.observations.mu.Lock()
		if s.observations.generation == generation {
			s.observations.candidates = append([]discovery.Candidate(nil), choices...)
		}
		s.observations.mu.Unlock()
		notifyModel(s.observations.wake)
		if err != nil {
			return "", err
		}
		if len(choices) == 1 {
			return choices[0].Address, nil
		}
		return "", errors.New("waiting for Controller selection")
	}
	go func() {
		p, err := e.start(ctx, o.Config, o.Role, app.Options{Network: o.Network, ResolveController: resolver})
		out <- operationResult{kind: starting, process: p, err: err}
	}()
}
func (s *session) prepare() {
	s.state.phase = preparing
	s.state.notice = ""
	s.operation = make(chan operationResult, 1)
	out := s.operation
	o := s.options
	ctx := s.roleCtx
	generation := s.generation
	go func() {
		cfg, err := s.effects.prepare(ctx, o, func(p setup.Progress) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.observations.mu.Lock()
			if s.observations.generation == generation {
				s.observations.progress = p
			}
			s.observations.mu.Unlock()
			notifyModel(s.observations.wake)
			return nil
		})
		o.Config = cfg
		out <- operationResult{kind: preparing, options: o, err: err}
	}()
	s.publish()
}
func (s *session) fail(err error) {
	s.state.phase = failed
	s.state.status = nil
	s.state.notice = cleanText(err.Error()) + "\nPress F5 to retry, F2 to diagnose, or F3 to choose a role."
	s.publish()
}
func (s *session) handle(ctx context.Context, a action) error {
	switch a.kind {
	case chooseRole:
		if s.state.phase == picking && (a.role == config.RoleController || a.role == config.RoleWorker) {
			s.beginRole(ctx, a.role)
		}
	case acceptNetwork, chooseInterface:
		if s.state.phase != choosingNetwork {
			return nil
		}
		if a.kind == chooseInterface {
			found := false
			for _, v := range s.state.interfaces {
				if v.IPv4 == a.value {
					found = true
				}
			}
			if !found {
				return nil
			}
			s.options.Config.AdvertiseAddress = a.value
			s.state.cfg = s.options.Config
		} else if len(s.state.interfaces) > 1 {
			return nil
		}
		if s.needsConsent {
			s.state.phase = awaitingConsent
		} else {
			s.startRole()
		}
		s.publish()
	case consent:
		if s.state.phase == awaitingConsent {
			s.prepare()
		}
	case returnToPicker:
		return s.toPicker()
	case retry:
		if s.state.phase == failed || s.state.phase == running && s.state.role == config.RoleWorker && s.state.agent.Report != nil && s.state.agent.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY {
			role := s.options.Role
			if err := s.stopRole(); err != nil {
				return err
			}
			s.beginRole(ctx, role)
		}
	case chooseController, changeAddress:
		if s.options.Network == app.Local {
			s.state.notice = "To change the local target, exit and run mica-mesh agent --local --controller-address 127.0.0.1:PORT."
			s.publish()
			return nil
		}
		if s.state.role != config.RoleWorker || s.state.agent.Membership.Registered {
			return nil
		}
		cfg := s.options.Config
		cfg.ControllerAddress = strings.TrimSpace(a.value)
		o := s.options
		o.Config = cfg
		if a.kind == chooseController {
			found := false
			for _, c := range s.state.candidates {
				if c.Address == cfg.ControllerAddress {
					found = true
				}
			}
			if !found {
				return nil
			}
		}
		if cfg.ControllerAddress == "" {
			s.state.notice = "Enter a Controller address as HOST:PORT."
		} else if err := validateRole(o); err != nil {
			s.state.notice = err.Error()
		} else {
			s.choiceMu.Lock()
			s.choice = cfg.ControllerAddress
			s.choiceMu.Unlock()
			s.state.notice = "Controller selected. Connecting on the next attempt."
		}
		s.publish()
	case changePort:
		port, err := strconv.Atoi(strings.TrimSpace(a.value))
		if err != nil || port < 1 || port > 65535 {
			s.state.notice = "Use a runtime port from 1 to 65535."
			s.publish()
			return nil
		}
		if s.state.role != config.RoleWorker {
			return nil
		}
		if err := s.stopRole(); err != nil {
			return err
		}
		s.options.Config.RuntimePort = port
		s.beginRole(ctx, config.RoleWorker)
	case submit:
		s.submit(a.value)
	case diagnose:
		if s.operation != nil {
			s.state.notice = "A check is in progress. Wait, then press F2 again."
			s.publish()
			return nil
		}
		s.operation = make(chan operationResult, 1)
		out := s.operation
		o := s.options
		role := doctor.Controller
		if s.state.role == config.RoleWorker {
			role = doctor.Agent
		}
		e := s.effects
		go func() {
			check, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			r, err := e.diagnose(check, o.Config, o.Layout, role, false)
			var b strings.Builder
			for _, c := range r.Checks {
				fmt.Fprintf(&b, "%s · %s\n%s\n%s\n", c.Name, c.State, c.Detail, c.Action)
			}
			if err != nil {
				b.WriteString(err.Error())
			}
			out <- operationResult{kind: picking, diagnosis: b.String()}
		}()
	}
	return nil
}
func (s *session) toPicker() error {
	if err := s.stopRole(); err != nil {
		return err
	}
	s.options.Role = 0
	s.state = sessionSnapshot{network: s.options.Network, phase: picking, cfg: s.options.Config, version: "development"}
	s.publish()
	return nil
}
func (s *session) stopRole() error {
	if s.cancelRole == nil && s.operation == nil {
		return nil
	}
	s.state.phase = stopping
	s.publish()
	if s.cancelRole != nil {
		s.cancelRole()
	}
	if s.request != nil {
		s.request.cancel()
	}
	var result error
	// Startup may return a handle after cancellation. Adopt and close it before join.
	if s.operation != nil {
		r := <-s.operation
		s.operation = nil
		if r.process != nil {
			s.process = r.process
		}
		if r.err != nil && !errors.Is(r.err, context.Canceled) {
			result = errors.Join(result, r.err)
		}
	}
	if s.process != nil {
		result = errors.Join(result, s.process.close())
	}
	if s.poll != nil {
		<-s.poll
		s.poll = nil
	}
	if s.request != nil && !s.request.snapshot().joined {
		<-s.request.done
	}
	if s.processDone != nil {
		result = errors.Join(result, <-s.processDone)
		s.processDone = nil
	}
	if s.client != nil {
		result = errors.Join(result, s.client.close())
	}
	s.process = nil
	s.client = nil
	s.request = nil
	s.cancelRole = nil
	s.state.request = requestView{}
	s.state.status = nil
	s.lastPoll = time.Time{}
	return result
}
func (s *session) startPoll() {
	now := s.effects.now()
	if s.state.phase != running || s.poll != nil || !s.lastPoll.IsZero() && now.Sub(s.lastPoll) < time.Second {
		return
	}
	s.lastPoll = now
	s.poll = make(chan pollResult, 1)
	out := s.poll
	p := s.process
	c := s.client
	ctx := s.roleCtx
	var cleanupUntil time.Time
	if s.request != nil {
		r := s.request.snapshot()
		if r.cleanup == cleanupChecking {
			cleanupUntil = r.canceled.Add(10 * time.Second)
		}
	}
	go func() {
		// Timestamp immediately before dispatch, not when this goroutine is scheduled.
		r := pollResult{started: s.effects.now()}
		deadline := r.started.Add(2 * time.Second)
		if !cleanupUntil.IsZero() && cleanupUntil.Before(deadline) {
			deadline = cleanupUntil
		}
		if c != nil {
			call, cancel := context.WithDeadline(ctx, deadline)
			r.status, r.err = c.status(call)
			cancel()
		} else {
			r.agent, _ = p.snapshot()
		}
		out <- r
	}()
}
func (s *session) applyPoll(p pollResult) {
	s.state.agent = p.agent
	s.state.statusAt = p.started
	s.state.status = p.status
	s.state.statusError = p.err != nil
	s.lastPoll = p.started
	if p.err != nil {
		s.state.status = nil
	}
	if s.request != nil {
		r := s.request.snapshot()
		if r.cleanup == cleanupChecking && p.err == nil && p.status != nil && s.effects.now().Before(r.canceled.Add(10*time.Second)) && !p.started.After(r.canceled.Add(10*time.Second)) {
			for _, w := range p.status.Workers {
				if confirmsCleanup(w, r.workerID, r.canceled, p.started) {
					s.request.mu.Lock()
					s.request.view.cleanup = cleanupConfirmed
					s.request.mu.Unlock()
					break
				}
			}
		}
	}
	s.observeCleanup()
	s.publish()
}
func (s *session) observeCleanup() {
	if s.request == nil {
		return
	}
	s.request.mu.Lock()
	defer s.request.mu.Unlock()
	r := &s.request.view
	if r.cleanup == cleanupChecking && !s.effects.now().Before(r.canceled.Add(10*time.Second)) {
		r.cleanup = cleanupUnconfirmed
	}
}
func (s *session) cancelRequest() {
	r := s.request
	r.mu.Lock()
	if r.view.canceled.IsZero() {
		r.view.canceled = s.effects.now()
		r.view.cleanup = cleanupChecking
	}
	r.mu.Unlock()
	r.cancel()
	s.publish()
}
func (s *session) submit(prompt string) {
	s.state.submissions++
	now := s.effects.now()
	if s.state.phase != running || s.client == nil || !capacityFresh(s.state.statusAt, now) {
		s.state.notice = "Wait for fresh Agent status, then send the prompt again."
		s.publish()
		return
	}
	if s.request != nil {
		r := s.request.snapshot()
		if !r.joined || r.cleanup == cleanupChecking {
			s.publish()
			return
		}
	}
	ready := false
	if s.state.status != nil {
		for _, w := range s.state.status.Workers {
			if eligible(w, s.options.Config.Model) {
				ready = true
			}
		}
	}
	if !ready {
		s.state.notice = "No Agent is ready. Wait, then send the prompt again."
		s.publish()
		return
	}
	cfg := s.options.Config
	req := &meshv1.InferenceRequest{RequestId: uuid.NewString(), ModelId: cfg.Model, Prompt: prompt, MaxOutputTokens: int32(cfg.MaxOutputTokens)}
	if err := protocol.ValidateRequest(req, cfg.Model); err != nil {
		s.state.notice = err.Error()
		s.publish()
		return
	}
	ctx, cancel := context.WithTimeout(s.roleCtx, cfg.Timeout)
	r := &requestOwner{view: requestView{id: req.RequestId, prompt: prompt, started: now, text: newTextQueue(s.mailbox.wake)}, cancel: cancel, done: make(chan error, 1)}
	s.request = r
	s.state.notice = ""
	s.publish()
	c := s.client
	go func() {
		err := c.generate(ctx, req, func(event *meshv1.InferenceEvent) error {
			switch v := event.Payload.(type) {
			case *meshv1.InferenceEvent_Started:
				r.mu.Lock()
				r.view.workerID = v.Started.WorkerId
				r.view.hostname = v.Started.WorkerHostname
				r.mu.Unlock()
				notifyModel(s.requestWake)
			case *meshv1.InferenceEvent_TextDelta:
				r.mu.Lock()
				if r.view.firstText.IsZero() {
					r.view.firstText = s.effects.now()
				}
				r.mu.Unlock()
				if err := r.view.text.put(ctx, v.TextDelta.Text); err != nil {
					return err
				}
			case *meshv1.InferenceEvent_Completed:
				r.mu.Lock()
				r.view.finish = v.Completed.FinishReason
				r.mu.Unlock()
			}
			return nil
		})
		r.done <- err
	}()
}
