package terminal

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshruntime "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	"github.com/AbhinavSingh95/mica-mesh/internal/worker"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// observedRuntime gates the external runtime only. Real worker/controller RPCs
// still own admission, stream cancellation, reservation release and cleanup.
type observedRuntime struct {
	*fakeruntime.Runtime
	healthCalls atomic.Int32
	ready       chan struct{}
	deltaGate   chan struct{}
}

func (r *observedRuntime) Health(ctx context.Context) (meshruntime.Health, error) {
	if r.healthCalls.Add(1) == 2 {
		close(r.ready)
	}
	return r.Runtime.Health(ctx)
}
func (r *observedRuntime) Generate(ctx context.Context, req meshruntime.Request, emit func(meshruntime.Event) error) error {
	return r.Runtime.Generate(ctx, req, func(event meshruntime.Event) error {
		if err := emit(event); err != nil {
			return err
		}
		if event.Kind == meshruntime.EventTextDelta {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-r.deltaGate:
			}
		}
		return nil
	})
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("effect barrier timed out")
	}
}
func serveTest(t *testing.T, register func(*grpc.Server), doneStream chan struct{}) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	options := []grpc.ServerOption{protocol.GenerationServerOption()}
	if doneStream != nil {
		options = append(options, grpc.ChainStreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			err := handler(srv, stream)
			notifyModel(doneStream)
			return err
		}))
	}
	server := grpc.NewServer(options...)
	register(server)
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			server.Stop()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("RPC owner not joined")
			}
		})
	}
	t.Cleanup(stop)
	return l.Addr().String(), stop
}
func TestRequestCancelKeepsBothRolesRunning(t *testing.T) {
	cfg := config.Default()
	cfg.ControllerListen = "127.0.0.1:0"
	cfg.WorkerListen = "127.0.0.1:0"
	cleanup := make(chan struct{})
	delta := make(chan struct{})
	rt := &observedRuntime{Runtime: fakeruntime.New(), ready: make(chan struct{}), deltaGate: delta}
	rt.CleanupGate = cleanup
	rt.Events = []meshruntime.Event{{Kind: meshruntime.EventStarted}, {Kind: meshruntime.EventTextDelta, Text: "partial 🌏"}, {Kind: meshruntime.EventCompleted, FinishReason: "stop"}}
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	id := uuid.NewString()
	svc := worker.New(id, meshruntime.Config{Model: cfg.ModelDescriptor, Backend: "cpu"}, &meshv1.HardwareInfo{Hostname: "Test Agent"}, rt)
	go func() { workerDone <- svc.RunRuntime(ctx) }()
	var cleanupOnce, deltaOnce sync.Once
	t.Cleanup(func() {
		cleanupOnce.Do(func() { close(cleanup) })
		deltaOnce.Do(func() { close(delta) })
		cancel()
		select {
		case err := <-workerDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime owner not joined")
		}
	})
	awaitSignal(t, rt.ready)
	workerStreamDone := make(chan struct{}, 1)
	workerAddress, _ := serveTest(t, func(g *grpc.Server) { meshv1.RegisterWorkerServiceServer(g, svc) }, workerStreamDone)
	reg := controller.NewRegistry(cfg.ModelDescriptor)
	if err := reg.Register(&meshv1.RegisterWorkerRequest{WorkerId: id, Endpoint: workerAddress, ProtocolMajor: protocol.Major, Hardware: &meshv1.HardwareInfo{Hostname: "Test Agent"}, RuntimeVersion: "fake", Backend: "cpu", Model: &meshv1.ModelDescriptor{Id: cfg.Model, Sha256: cfg.ModelDescriptor.SHA256, ContextTokens: 2048}, Capacity: 1, Report: svc.Report()}, time.Now()); err != nil {
		t.Fatal(err)
	}
	probe, _ := reg.ProbeTarget(id)
	health, err := svc.Health(context.Background(), &meshv1.HealthRequest{})
	if err != nil || !reg.ApplyProbe(probe, health, err) {
		t.Fatal("initial readiness proof failed")
	}
	controllerSvc := controller.New(uuid.NewString(), reg)
	controllerStreamDone := make(chan struct{}, 1)
	address, stopController := serveTest(t, func(g *grpc.Server) { meshv1.RegisterControllerServiceServer(g, controllerSvc) }, controllerStreamDone)
	controllerCtx, cancelController := context.WithCancel(context.Background())
	controllerDone := make(chan error, 1)
	go func() { controllerDone <- controllerSvc.Run(controllerCtx) }()
	t.Cleanup(func() {
		cancelController()
		stopController()
		if err := controllerSvc.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-controllerDone:
		case <-time.After(3 * time.Second):
			t.Error("Controller not joined")
		}
	})
	e := testEffects()
	e.connect = productionEffects().connect
	e.start = func(ctx context.Context, c config.Config, r config.Role, o app.Options) (*roleHandle, error) {
		child, stop := context.WithCancel(ctx)
		return &roleHandle{endpoint: address, wait: func() error { <-child.Done(); return nil }, close: func() error { stop(); return nil }}, nil
	}
	base := time.Now()
	var clock atomic.Int64
	e.now = func() time.Time { return base.Add(time.Duration(clock.Load())) }
	h := startSession(t, Options{Config: cfg, Role: config.RoleController, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool {
		return s.status != nil && len(s.status.Workers) > 0 && eligible(s.status.Workers[0], cfg.Model)
	})
	h.act(action{kind: submit, value: "first prompt"})
	s := h.await(t, func(s sessionSnapshot) bool { return s.request.workerID == id })
	q := s.request.text
	// The queue wake carries accepted text, not just Started metadata.
	var partial string
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for partial == "" {
		partial += q.drain()
		if partial != "" {
			break
		}
		select {
		case <-h.box.wake:
		case <-timer.C:
			t.Fatal("no partial text")
		}
	}
	if partial != "partial 🌏" {
		t.Fatalf("partial text %q", partial)
	}
	notifyModel(h.controls.interrupt)
	h.await(t, func(s sessionSnapshot) bool { return s.request.joined && s.request.cleanup == cleanupChecking })
	awaitSignal(t, rt.Cleaning)
	awaitSignal(t, controllerStreamDone)
	if rt.Counters().Stops != 0 {
		t.Fatal("request cancellation stopped Agent runtime")
	}
	clock.Store(int64(time.Second))
	h.ticks <- e.now()
	s = h.await(t, func(s sessionSnapshot) bool { return s.statusAt.Equal(e.now()) })
	if s.request.cleanup != cleanupChecking {
		t.Fatal("busy worker confirmed cleanup")
	}
	cleanupOnce.Do(func() { close(cleanup) })
	awaitSignal(t, workerStreamDone)
	if err := reg.Heartbeat(id, svc.Report(), time.Now()); err != nil {
		t.Fatal(err)
	}
	probe, _ = reg.ProbeTarget(id)
	health, err = svc.Health(context.Background(), &meshv1.HealthRequest{})
	if err != nil || !reg.ApplyProbe(probe, health, err) {
		t.Fatal("post-cleanup readiness proof failed")
	}
	clock.Store(int64(2 * time.Second))
	h.ticks <- e.now()
	h.await(t, func(s sessionSnapshot) bool { return s.request.cleanup == cleanupConfirmed })
	deltaOnce.Do(func() { close(delta) })
	h.act(action{kind: submit, value: "second prompt"})
	s = h.await(t, func(s sessionSnapshot) bool { return s.request.prompt == "second prompt" && s.request.joined })
	if s.request.failure != "" || s.request.finish != "stop" {
		t.Fatalf("next manual request failed: %+v", s.request)
	}
	if rt.Counters().Stops != 0 || rt.Counters().Active != 0 {
		t.Fatalf("role lifetime changed: %+v", rt.Counters())
	}
}
func TestPollStartsAtMostOncePerSecondAndKeepsOneOwner(t *testing.T) {
	base := time.Now()
	now := base
	e := testEffects()
	e.now = func() time.Time { return now }
	s := newSession(Options{}, e, newControls(), newMailbox())
	s.state.phase = running
	s.roleCtx = context.Background()
	gate := make(chan struct{})
	calls := make(chan time.Time, 4)
	s.client = &controllerConnection{status: func(ctx context.Context) (*meshv1.GetClusterStatusResponse, error) {
		deadline, _ := ctx.Deadline()
		calls <- deadline
		<-gate
		return nil, errors.New("offline")
	}}
	s.startPoll()
	deadline := <-calls
	if !deadline.Equal(base.Add(2 * time.Second)) {
		t.Fatal("wrong status budget")
	}
	now = base.Add(2 * time.Second)
	s.startPoll()
	select {
	case <-calls:
		t.Fatal("overlapping status calls")
	default:
	}
	close(gate)
	<-s.poll
	s.poll = nil
	now = base.Add(500 * time.Millisecond)
	s.startPoll()
	if s.poll != nil {
		t.Fatal("early second poll")
	}
	now = base.Add(2 * time.Second)
	s.startPoll()
	<-calls
	<-s.poll
}
func TestRequestQueueCompletionWaitsForFinalOK(t *testing.T) {
	e := testEffects()
	final := make(chan struct{})
	started := make(chan struct{})
	e.connect = func(string) (*controllerConnection, error) {
		return &controllerConnection{close: func() error { return nil }, status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
			w := idleWorker()
			w.Model.Id = config.Default().Model
			return &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}, nil
		}, generate: func(ctx context.Context, r *meshv1.InferenceRequest, emit func(*meshv1.InferenceEvent) error) error {
			emit(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "stop"}}})
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-final:
				return errors.New("final transport failure")
			}
		}}, nil
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleController, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.status != nil })
	h.act(action{kind: submit, value: "prompt"})
	awaitSignal(t, started)
	if h.box.snapshot().request.joined {
		t.Fatal("Completed claimed success before final OK")
	}
	close(final)
	s := h.await(t, func(s sessionSnapshot) bool { return s.request.joined })
	if !strings.Contains(s.request.failure, "transport failure") {
		t.Fatal("final failure lost")
	}
}
