package terminal

import (
	"context"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTwoWorkerSessionNeverUsesIdlePeerAsCleanupProof(t *testing.T) {
	cfg := config.Default()
	reg := controller.NewRegistry(cfg.ModelDescriptor)
	type agent struct {
		id                     string
		svc                    *worker.Service
		rt                     *observedRuntime
		delta, cleanup         chan struct{}
		deltaOnce, cleanupOnce sync.Once
	}
	agents := make(map[string]*agent)
	for range 2 {
		a := &agent{id: uuid.NewString(), delta: make(chan struct{}), cleanup: make(chan struct{})}
		a.rt = &observedRuntime{Runtime: fakeruntime.New(), ready: make(chan struct{}), deltaGate: a.delta}
		a.rt.CleanupGate = a.cleanup
		a.rt.Events = []meshruntime.Event{{Kind: meshruntime.EventStarted}, {Kind: meshruntime.EventTextDelta, Text: "partial"}, {Kind: meshruntime.EventCompleted, FinishReason: "stop"}}
		a.svc = worker.New(a.id, meshruntime.Config{Model: cfg.ModelDescriptor, Backend: "cpu"}, &meshv1.HardwareInfo{Hostname: a.id}, a.rt)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.svc.RunRuntime(ctx) }()
		t.Cleanup(func() {
			a.deltaOnce.Do(func() { close(a.delta) })
			a.cleanupOnce.Do(func() { close(a.cleanup) })
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("Agent runtime did not join")
			}
		})
		awaitSignal(t, a.rt.ready)
		address, _ := serveTest(t, func(g *grpc.Server) { meshv1.RegisterWorkerServiceServer(g, a.svc) }, nil)
		err := reg.Register(&meshv1.RegisterWorkerRequest{WorkerId: a.id, Endpoint: address, ProtocolMajor: protocol.Major, Hardware: &meshv1.HardwareInfo{Hostname: a.id}, RuntimeVersion: "fake", Backend: "cpu", Model: &meshv1.ModelDescriptor{Id: cfg.Model, Sha256: cfg.ModelDescriptor.SHA256, ContextTokens: 2048}, Capacity: 1, Report: a.svc.Report()}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		probe, _ := reg.ProbeTarget(a.id)
		health, err := a.svc.Health(context.Background(), &meshv1.HealthRequest{})
		if err != nil || !reg.ApplyProbe(probe, health, err) {
			t.Fatal("Agent readiness not established")
		}
		agents[a.id] = a
	}
	service := controller.New(uuid.NewString(), reg)
	streamDone := make(chan struct{}, 1)
	address, stop := serveTest(t, func(g *grpc.Server) { meshv1.RegisterControllerServiceServer(g, service) }, streamDone)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		stop()
		if err := service.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Controller did not join")
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
	tick := func() {
		t.Helper()
		select {
		case h.ticks <- e.now():
		case <-time.After(time.Second):
			t.Fatal("session did not accept observation tick")
		}
	}
	h.await(t, func(s sessionSnapshot) bool { return s.status != nil && len(s.status.Workers) == 2 })
	h.act(action{kind: submit, value: "request A"})
	first := h.await(t, func(s sessionSnapshot) bool { return s.request.workerID != "" })
	a := agents[first.request.workerID]
	var b *agent
	for id, candidate := range agents {
		if id != a.id {
			b = candidate
		}
	}
	notifyModel(h.controls.cancelRequest)
	h.await(t, func(s sessionSnapshot) bool { return s.request.joined })
	awaitSignal(t, a.rt.Cleaning)
	awaitSignal(t, streamDone)
	clock.Store(int64(time.Second))
	tick()
	observed := h.await(t, func(s sessionSnapshot) bool { return s.statusAt.Equal(e.now()) })
	if observed.request.cleanup != cleanupChecking || b.rt.Counters().Active != 0 || a.rt.Counters().Active != 1 {
		t.Fatal("idle peer confirmed selected Agent cleanup")
	}
	// The observation budget is UI policy only. Expiry permits a new user
	// request through normal admission; it does not assert remote cleanup.
	clock.Store(int64(10 * time.Second))
	tick()
	observed = h.await(t, func(s sessionSnapshot) bool {
		return s.request.cleanup == cleanupUnconfirmed && s.statusAt.Equal(e.now())
	})
	m := newScreen(Options{Role: config.RoleController})
	m.state = observed
	m.consume()
	h.act(action{kind: submit, value: "request B"})
	second := h.await(t, func(s sessionSnapshot) bool { return s.request.prompt == "request B" && s.request.workerID != "" })
	if second.request.workerID != b.id {
		t.Fatal("new request entered selected Agent's outstanding cleanup")
	}
	other, err := productionEffects().connect(address)
	if err != nil {
		t.Fatal(err)
	}
	defer other.close()
	call, stopCall := context.WithTimeout(context.Background(), time.Second)
	err = other.generate(call, &meshv1.InferenceRequest{RequestId: uuid.NewString(), ModelId: cfg.Model, Prompt: "must not queue", MaxOutputTokens: 16}, func(*meshv1.InferenceEvent) error { return nil })
	stopCall()
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("busy admission=%v", err)
	}
	b.deltaOnce.Do(func() { close(b.delta) })
	b.cleanupOnce.Do(func() { close(b.cleanup) })
	second = h.await(t, func(s sessionSnapshot) bool { return s.request.prompt == "request B" && s.request.joined })
	if second.request.failure != "" {
		t.Fatal(second.request.failure)
	}
	m.state = second
	m.consume()
	if m.cleanupNotice() == "" {
		t.Fatal("later successful request erased prior cleanup uncertainty")
	}
	a.cleanupOnce.Do(func() { close(a.cleanup) })
	awaitSignal(t, a.rt.Released)
	for _, a := range agents {
		counts := a.rt.Counters()
		if counts.Peak != 1 || counts.Active != 0 || counts.Stops != 0 {
			t.Fatalf("bad runtime ownership: %+v", counts)
		}
	}
}
