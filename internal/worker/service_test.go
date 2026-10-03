package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const workerID = "00000000-0000-4000-8000-000000000001"

func config() mesh.Config {
	return mesh.Config{Backend: "cpu", Model: mesh.Model{ID: "test-model", SHA256: strings.Repeat("a", 64), ContextTokens: 4096}}
}
func request() *meshv1.InferenceRequest {
	return &meshv1.InferenceRequest{RequestId: "00000000-0000-4000-8000-000000000002", ModelId: "test-model", Prompt: "hello", MaxOutputTokens: 128}
}

type stream struct {
	grpc.ServerStream
	ctx       context.Context
	events    []*meshv1.InferenceEvent
	sendError error
}

func (s *stream) Context() context.Context { return s.ctx }
func (s *stream) Send(e *meshv1.InferenceEvent) error {
	if s.sendError != nil {
		return s.sendError
	}
	s.events = append(s.events, e)
	return nil
}
func ready(t *testing.T, rt *fakeruntime.Runtime) *Service {
	t.Helper()
	if err := rt.Start(context.Background(), config()); err != nil {
		t.Fatal(err)
	}
	s := New(workerID, config(), &meshv1.HardwareInfo{Hostname: "host"}, rt)
	s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_READY
	s.lifecycle = lifecycleRunning
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := rt.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func signalWait(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("phase did not occur")
	}
}
func resultWait(t *testing.T, c <-chan error) error {
	t.Helper()
	select {
	case e := <-c:
		return e
	case <-time.After(6 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}
func TestGenerateRejectsOverlap(t *testing.T) {
	rt := fakeruntime.New()
	gate := make(chan struct{})
	rt.ReleaseGate = gate
	s := ready(t, rt)
	done := make(chan error, 1)
	go func() { done <- generate(t, s, request(), &stream{ctx: context.Background()}) }()
	signalWait(t, rt.Admitted)
	err := generate(t, s, request(), &stream{ctx: context.Background()})
	close(gate)
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("overlap code=%v", status.Code(err))
	}
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
	if rt.Counters().Peak != 1 {
		t.Errorf("peak=%d", rt.Counters().Peak)
	}
}
func TestStartedAfterValidation(t *testing.T) {
	rt := fakeruntime.New()
	rt.GenerateError = mesh.ErrInvalidInput
	s := ready(t, rt)
	out := &stream{ctx: context.Background()}
	if err := generate(t, s, request(), out); status.Code(err) != codes.InvalidArgument {
		t.Errorf("code=%v", status.Code(err))
	}
	if len(out.events) != 0 {
		t.Fatal("rejected input emitted events")
	}
}
func TestCancelRetainsLocalGuard(t *testing.T) {
	rt := fakeruntime.New()
	release := make(chan struct{})
	cleanup := make(chan struct{})
	rt.ReleaseGate = release
	rt.CleanupGate = cleanup
	s := ready(t, rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- generate(t, s, request(), &stream{ctx: ctx}) }()
	signalWait(t, rt.Admitted)
	cancel()
	signalWait(t, rt.Cleaning)
	r := s.Report()
	if !r.Active || r.ActiveRequestId == nil {
		t.Error("cleanup lost activity")
	}
	r.Active = false
	*r.ActiveRequestId = "tampered"
	if current := s.Report(); !current.Active || current.GetActiveRequestId() != request().RequestId {
		t.Error("caller mutated protected admission ownership through Report")
	}
	err := generate(t, s, request(), &stream{ctx: context.Background()})
	close(cleanup)
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("cleanup overlap=%v", status.Code(err))
	}
	if status.Code(resultWait(t, done)) != codes.Canceled {
		t.Error("lost cancellation")
	}
	if s.Report().Active || rt.Counters().Peak != 1 {
		t.Fatal("incorrect final capacity")
	}
}
func TestRuntimeErrorsMapToStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{{mesh.ErrInvalidInput, codes.InvalidArgument}, {mesh.ErrUnavailable, codes.Unavailable}, {mesh.ErrMalformedResponse, codes.Internal}, {errors.New("private prompt/body"), codes.Internal}, {context.Canceled, codes.Canceled}, {context.DeadlineExceeded, codes.DeadlineExceeded}} {
		t.Run(tc.code.String()+tc.err.Error(), func(t *testing.T) {
			rt := fakeruntime.New()
			rt.GenerateError = tc.err
			s := ready(t, rt)
			err := generate(t, s, request(), &stream{ctx: context.Background()})
			if status.Code(err) != tc.code || strings.Contains(err.Error(), "private") {
				t.Errorf("error=%v", err)
			}
		})
	}
}
func TestWorkerValidatesRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*meshv1.InferenceRequest)
		code   codes.Code
	}{{"id", func(r *meshv1.InferenceRequest) { r.RequestId = "bad" }, codes.InvalidArgument}, {"empty", func(r *meshv1.InferenceRequest) { r.Prompt = "" }, codes.InvalidArgument}, {"utf8", func(r *meshv1.InferenceRequest) { r.Prompt = "\xff" }, codes.InvalidArgument}, {"large", func(r *meshv1.InferenceRequest) { r.Prompt = strings.Repeat("x", 16385) }, codes.InvalidArgument}, {"limit", func(r *meshv1.InferenceRequest) { r.MaxOutputTokens = 513 }, codes.InvalidArgument}, {"model", func(r *meshv1.InferenceRequest) { r.ModelId = "other" }, codes.NotFound}} {
		t.Run(tc.name, func(t *testing.T) {
			rt := fakeruntime.New()
			s := ready(t, rt)
			r := request()
			tc.mutate(r)
			out := &stream{ctx: context.Background()}
			err := generate(t, s, r, out)
			if status.Code(err) != tc.code || rt.Counters().Peak != 0 || len(out.events) != 0 {
				t.Errorf("invalid request error=%v events=%d peak=%d", err, len(out.events), rt.Counters().Peak)
			}
		})
	}
}
func TestCompletionWaitsForCleanup(t *testing.T) {
	rt := fakeruntime.New()
	rt.CleanupError = mesh.ErrUnavailable
	s := ready(t, rt)
	out := &stream{ctx: context.Background()}
	if status.Code(generate(t, s, request(), out)) != codes.Unavailable {
		t.Error("cleanup error discarded")
	}
	for _, e := range out.events {
		if e.GetCompleted() != nil {
			t.Fatal("completed before failed cleanup")
		}
	}
}
func TestRuntimeEventContract(t *testing.T) {
	for _, events := range [][]mesh.Event{{}, {{Kind: mesh.EventTextDelta, Text: "early"}}, {{Kind: mesh.EventStarted}, {Kind: mesh.EventStarted}}, {{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: "\xff"}}, {{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: strings.Repeat("x", 4097)}}, {{Kind: mesh.EventStarted}, {Kind: mesh.EventCompleted, FinishReason: "other"}}, {{Kind: mesh.EventStarted}, {Kind: mesh.EventCompleted, FinishReason: "stop"}, {Kind: mesh.EventTextDelta, Text: "late"}}, {{Kind: mesh.EventKind(99)}}} {
		rt := fakeruntime.New()
		rt.Events = events
		s := ready(t, rt)
		out := &stream{ctx: context.Background()}
		if status.Code(generate(t, s, request(), out)) != codes.Internal {
			t.Errorf("malformed events accepted: %v", events)
		}
		for _, e := range out.events {
			if e.GetCompleted() != nil {
				t.Error("malformed stream completed")
			}
		}
	}
}
func TestSuccessfulStreamIdentity(t *testing.T) {
	rt := fakeruntime.New()
	rt.Events = []mesh.Event{{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: "hello"}, {Kind: mesh.EventCompleted, FinishReason: "length"}}
	s := ready(t, rt)
	out := &stream{ctx: context.Background()}
	if err := generate(t, s, request(), out); err != nil {
		t.Fatal(err)
	}
	if len(out.events) != 3 {
		t.Fatalf("events=%d", len(out.events))
	}
	e := out.events[0].GetStarted()
	if e == nil || e.WorkerId != workerID || e.WorkerHostname != "host" || e.ModelId != "test-model" || e.RequestId != request().RequestId {
		t.Errorf("identity=%v", e)
	}
	if out.events[1].GetTextDelta().Text != "hello" || out.events[2].GetCompleted().FinishReason != "length" {
		t.Fatal("incorrect payload")
	}
}

// invokeServer obtains the actual transport context, but retains a controllable
// synchronous sender for event validation tests. Transport stalls are tested separately.
type invokeServer struct {
	meshv1.UnimplementedWorkerServiceServer
	s      *Service
	out    *stream
	req    *meshv1.InferenceRequest
	result chan error
}

func (i *invokeServer) Generate(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	i.out.ctx = out.Context()
	err := i.s.Generate(i.req, i.out)
	i.result <- err
	return err
}
func generate(t *testing.T, s *Service, req *meshv1.InferenceRequest, out *stream) error {
	t.Helper()
	clientctx := out.ctx
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	result := make(chan error, 1)
	server := grpc.NewServer(protocol.GenerationServerOption())
	meshv1.RegisterWorkerServiceServer(server, &invokeServer{s: s, out: out, req: req, result: result})
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	defer func() { server.Stop(); <-served }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client, err := meshv1.NewWorkerServiceClient(conn).Generate(clientctx, request())
	if err != nil {
		return err
	}
	_, _ = client.Recv() // the controlled sender emits locally; final status wakes Recv
	return resultWait(t, result)
}

// cleanupRuntime models the real adapter's exceptional boundary: Generate has
// returned, but idle/reap was not confirmed. The ordinary fake still supplies
// synchronous events, admission and bounded cleanup for the generation itself.
type cleanupRuntime struct {
	*fakeruntime.Runtime
	mu            sync.Mutex
	cleanupHealth *mesh.Health
	probeError    error
	stopError     error
	entered       chan struct{}
	release       chan struct{}
}

func (r *cleanupRuntime) Generate(ctx context.Context, req mesh.Request, emit func(mesh.Event) error) error {
	r.entered <- struct{}{}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := r.Runtime.Generate(ctx, req, emit); err != nil {
		return err
	}
	r.mu.Lock()
	r.cleanupHealth = &mesh.Health{State: mesh.StateUnhealthy, Active: true, LastError: "idle/reap not confirmed"}
	r.mu.Unlock()
	return mesh.ErrUnavailable
}
func (r *cleanupRuntime) Health(ctx context.Context) (mesh.Health, error) {
	r.mu.Lock()
	if r.cleanupHealth != nil {
		health := *r.cleanupHealth
		err := r.probeError
		r.mu.Unlock()
		if err != nil {
			return mesh.Health{}, err
		}
		return health, nil
	}
	r.mu.Unlock()
	return r.Runtime.Health(ctx)
}
func (r *cleanupRuntime) Stop(ctx context.Context) error {
	r.mu.Lock()
	err := r.stopError
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := r.Runtime.Stop(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.cleanupHealth = &mesh.Health{State: mesh.StateUnhealthy}
	r.mu.Unlock()
	return nil
}
func TestUnconfirmedRuntimeCleanupRetainsActivity(t *testing.T) {
	for _, probeFails := range []bool{false, true} {
		for _, cleanupByStop := range []bool{false, true} {
			name := fmt.Sprintf("health_error=%v/cleanup_by_stop=%v", probeFails, cleanupByStop)
			t.Run(name, func(t *testing.T) {
				rt := fakeruntime.New()
				s := ready(t, rt)
				r := &cleanupRuntime{Runtime: rt, stopError: mesh.ErrUnavailable, entered: make(chan struct{}, 1), release: make(chan struct{})}
				if probeFails {
					r.probeError = mesh.ErrUnavailable
				}
				s.rt = r
				out := &stream{ctx: context.Background()}
				done := make(chan error, 1)
				go func() { done <- generate(t, s, request(), out) }()
				signalWait(t, r.entered)
				if _, err := s.refreshHealth(context.Background()); err != nil {
					t.Fatal(err)
				}
				close(r.release)
				if code := status.Code(resultWait(t, done)); code != codes.Unavailable {
					t.Errorf("generation code=%v", code)
				}
				for _, event := range out.events {
					if event.GetCompleted() != nil {
						t.Error("unconfirmed cleanup sent Completed")
					}
				}
				assertRetained := func(phase string) {
					t.Helper()
					health, err := s.Health(context.Background(), &meshv1.HealthRequest{})
					if err != nil {
						t.Fatal(err)
					}
					if health.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY || !health.Report.Active || health.Report.GetActiveRequestId() != request().RequestId {
						t.Errorf("%s lost runtime ownership: %v", phase, health.Report)
					}
				}
				assertRetained("handler return")
				if code := status.Code(generate(t, s, request(), &stream{ctx: context.Background()})); code != codes.ResourceExhausted {
					t.Errorf("unconfirmed runtime activity freed local slot: %v", code)
				}
				if err := s.stopRuntime(); !errors.Is(err, mesh.ErrUnavailable) {
					t.Errorf("failed Stop=%v", err)
				}
				assertRetained("failed Stop")
				if cleanupByStop {
					r.mu.Lock()
					r.stopError = nil
					r.mu.Unlock()
					if err := s.stopRuntime(); err != nil {
						t.Fatal(err)
					}
				} else {
					r.mu.Lock()
					r.probeError = nil
					r.cleanupHealth.Active = false
					r.mu.Unlock()
					if _, err := s.refreshHealth(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if report := s.Report(); report.Active || report.ActiveRequestId != nil {
					t.Errorf("confirmed cleanup retained ownership: %v", report)
				}
			})
		}
	}
}

type validationRuntime struct {
	*fakeruntime.Runtime
	entered chan struct{}
	release chan struct{}
}

func (r *validationRuntime) Generate(ctx context.Context, req mesh.Request, emit func(mesh.Event) error) error {
	r.entered <- struct{}{}
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Runtime.Generate(ctx, req, emit)
}
func TestIdleHealthCannotClearActiveHandler(t *testing.T) {
	rt := fakeruntime.New()
	s := ready(t, rt)
	r := &validationRuntime{Runtime: rt, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.rt = r
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- generate(t, s, request(), &stream{ctx: ctx}) }()
	signalWait(t, r.entered)
	health, err := s.refreshHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Active {
		t.Fatal("fixture runtime should still be idle during handler-owned validation")
	}
	if report := s.Report(); !report.Active || report.GetActiveRequestId() != request().RequestId {
		t.Errorf("idle health released active handler: %v", report)
	}
	overlap, cancelOverlap := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelOverlap()
	if code := status.Code(generate(t, s, request(), &stream{ctx: overlap})); code != codes.ResourceExhausted {
		t.Errorf("overlap during validation=%v", code)
	}
	close(r.release)
	if err := resultWait(t, done); err != nil {
		t.Fatal(err)
	}
	if report := s.Report(); report.Active || report.ActiveRequestId != nil {
		t.Errorf("completed handler remained active: %v", report)
	}
}
