package controller

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func serviceID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func serviceModel() mesh.Model {
	return mesh.Model{ID: "configured-model", SHA256: strings.Repeat("ab", 32), ContextTokens: 4096}
}
func idleReport() *meshv1.WorkerReport {
	return &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}
}
func inference() *meshv1.InferenceRequest {
	return &meshv1.InferenceRequest{RequestId: serviceID(101), ModelId: "configured-model", Prompt: "hello", MaxOutputTokens: 128}
}
func started(req *meshv1.InferenceRequest) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: req.RequestId, WorkerId: serviceID(1), WorkerHostname: "worker", ModelId: req.ModelId}}}
}
func delta(text string) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: text}}}
}
func completed() *meshv1.InferenceEvent {
	count := int64(7)
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "stop", OutputTokens: &count}}}
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("owned operation did not finish")
		var zero T
		return zero
	}
}
func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code=%v want=%v: %v", got, want, err)
	}
}

type rpcWorker struct {
	meshv1.UnimplementedWorkerServiceServer
	health   func(context.Context) (*meshv1.HealthResponse, error)
	generate func(*meshv1.InferenceRequest, grpc.ServerStreamingServer[meshv1.InferenceEvent]) error
	calls    atomic.Int32
}

func (w *rpcWorker) Health(ctx context.Context, _ *meshv1.HealthRequest) (*meshv1.HealthResponse, error) {
	if w.health != nil {
		return w.health(ctx)
	}
	return &meshv1.HealthResponse{WorkerId: serviceID(1), Report: idleReport()}, nil
}
func (w *rpcWorker) Generate(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	w.calls.Add(1)
	if w.generate != nil {
		return w.generate(req, out)
	}
	for _, e := range []*meshv1.InferenceEvent{started(req), delta("héllo"), completed()} {
		if err := out.Send(e); err != nil {
			return err
		}
	}
	return nil
}
func serverConn(t *testing.T, register func(*grpc.Server), options ...grpc.ServerOption) (*grpc.ClientConn, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(append([]grpc.ServerOption{protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes)}, options...)...)
	register(server)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(65535), grpc.WithInitialConnWindowSize(65535))
	if err != nil {
		server.Stop()
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); await(t, done) })
	return conn, listener.Addr().String()
}
func fixture(t *testing.T, w *rpcWorker) (*Service, *meshv1.RegisterWorkerRequest, meshv1.ControllerServiceClient) {
	t.Helper()
	_, endpoint := serverConn(t, func(s *grpc.Server) { meshv1.RegisterWorkerServiceServer(s, w) })
	m := serviceModel()
	r := NewRegistry(m)
	svc := New(serviceID(50), r)
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, _ := serverConn(t, func(s *grpc.Server) { meshv1.RegisterControllerServiceServer(s, svc) })
	registration := &meshv1.RegisterWorkerRequest{WorkerId: serviceID(1), Endpoint: endpoint, ProtocolMajor: 1, Hardware: &meshv1.HardwareInfo{Hostname: "worker"}, Model: &meshv1.ModelDescriptor{Id: m.ID, Sha256: m.SHA256, ContextTokens: 4096}, Capacity: 1, Report: idleReport()}
	return svc, registration, meshv1.NewControllerServiceClient(conn)
}
func registerReady(t *testing.T, s *Service, r *meshv1.RegisterWorkerRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.RegisterWorker(ctx, r); err != nil {
		t.Fatal(err)
	}
	s.probeWorker(r.WorkerId)
	if got := s.registry.Snapshot(time.Now())[0].State; got != meshv1.WorkerState_WORKER_STATE_READY {
		t.Fatalf("probe did not establish readiness: %v", got)
	}
}
func receive(t *testing.T, call grpc.ServerStreamingClient[meshv1.InferenceEvent]) ([]*meshv1.InferenceEvent, error) {
	t.Helper()
	var events []*meshv1.InferenceEvent
	for {
		e, err := call.Recv()
		if err != nil {
			return events, err
		}
		events = append(events, e)
	}
}
func callInference(t *testing.T, c meshv1.ControllerServiceClient, ctx context.Context) grpc.ServerStreamingClient[meshv1.InferenceEvent] {
	t.Helper()
	call, err := c.RunInference(ctx, inference(), grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes))
	if err != nil {
		t.Fatal(err)
	}
	return call
}
func TestRegisterVisibleUntilReverseProbePasses(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	w := &rpcWorker{health: func(ctx context.Context) (*meshv1.HealthResponse, error) {
		entered <- struct{}{}
		select {
		case <-gate:
			return &meshv1.HealthResponse{WorkerId: serviceID(1), Report: idleReport()}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	s, r, c := fixture(t, w)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.RegisterWorker(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if reply.ControllerId != serviceID(50) || reply.HeartbeatIntervalSeconds != 2 || reply.MembershipExpirySeconds != 10 {
		t.Fatalf("registration policy=%v", reply)
	}
	probeDone := make(chan struct{})
	go func() { s.probeWorker(r.WorkerId); close(probeDone) }()
	await(t, entered)
	statusReply, err := c.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(statusReply.Workers) != 1 || statusReply.Workers[0].State != meshv1.WorkerState_WORKER_STATE_STARTING {
		t.Fatalf("unverified worker=%v", statusReply)
	}
	_, err = s.registry.Reserve(r.Model.Id, serviceID(102), time.Now())
	wantCode(t, err, codes.Unavailable)
	close(gate)
	await(t, probeDone)
	if s.registry.Snapshot(time.Now())[0].State != meshv1.WorkerState_WORKER_STATE_READY {
		t.Fatal("matching identity probe did not establish ready")
	}
}
func TestWrongWorkerIdentityNeverReady(t *testing.T) {
	w := &rpcWorker{health: func(context.Context) (*meshv1.HealthResponse, error) {
		return &meshv1.HealthResponse{WorkerId: serviceID(2), Report: idleReport()}, nil
	}}
	s, r, _ := fixture(t, w)
	if _, err := s.RegisterWorker(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	s.probeWorker(r.WorkerId)
	row := s.registry.Snapshot(time.Now())[0]
	if row.State != meshv1.WorkerState_WORKER_STATE_UNHEALTHY || !strings.Contains(row.LastError, "identity") {
		t.Fatalf("wrong identity=%v", row)
	}
}
func TestProxyPreservesEventsAndDeadline(t *testing.T) {
	seen := make(chan time.Time, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		if !proto.Equal(r, inference()) {
			return status.Error(codes.Internal, "changed request")
		}
		deadline, _ := out.Context().Deadline()
		seen <- deadline
		for _, e := range []*meshv1.InferenceEvent{started(r), delta("héllo"), completed()} {
			if err := out.Send(e); err != nil {
				return err
			}
		}
		return nil
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	incoming, _ := ctx.Deadline()
	events, err := receive(t, callInference(t, c, ctx))
	if err != io.EOF {
		t.Fatal(err)
	}
	// grpc encodes a relative timeout: allow 10ms for loopback transit/rounding.
	// The original-budget cancellation regression separately forbids a timeout reset.
	if forwarded := await(t, seen); forwarded.After(incoming.Add(10 * time.Millisecond)) {
		t.Fatalf("forwarded deadline %v extended %v", forwarded, incoming)
	}
	want := []*meshv1.InferenceEvent{started(inference()), delta("héllo"), completed()}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for i := range want {
		if !proto.Equal(events[i], want[i]) {
			t.Fatalf("event %d=%v want=%v", i, events[i], want[i])
		}
	}
}
func TestNoDeadlineGetsMaximumBudget(t *testing.T) {
	seen := make(chan time.Time, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		deadline, ok := out.Context().Deadline()
		if !ok {
			return status.Error(codes.Internal, "missing deadline")
		}
		seen <- deadline
		out.Send(started(r))
		return out.Send(completed())
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	begin := time.Now()
	_, err := receive(t, callInference(t, c, context.Background()))
	if err != io.EOF {
		t.Fatal(err)
	}
	if deadline := await(t, seen); deadline.Before(begin.Add(299*time.Second)) || deadline.After(time.Now().Add(300*time.Second)) {
		t.Fatalf("fallback deadline=%v", deadline)
	}
}
func TestNoRedispatchAfterGenerate(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
				if partial {
					out.Send(started(r))
					out.Send(delta("partial"))
				}
				return status.Error(codes.Unavailable, "secret runtime response")
			}}
			s, r, c := fixture(t, w)
			registerReady(t, s, r)
			second := proto.Clone(r).(*meshv1.RegisterWorkerRequest)
			second.WorkerId = serviceID(2)
			if err := s.registry.Register(second, time.Now()); err != nil {
				t.Fatal(err)
			}
			p, _ := s.registry.ProbeTarget(second.WorkerId)
			s.registry.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: second.WorkerId, Report: idleReport()}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			events, err := receive(t, callInference(t, c, ctx))
			wantCode(t, err, codes.Unavailable)
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("upstream diagnostics leaked")
			}
			if w.calls.Load() != 1 {
				t.Fatalf("redispatched %d times", w.calls.Load())
			}
			wantEvents := 0
			if partial {
				wantEvents = 2
			}
			if len(events) != wantEvents {
				t.Fatalf("partial output changed: %v", events)
			}
		})
	}
}
func TestCompletedRequiresUpstreamFinalOK(t *testing.T) {
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		out.Send(started(r))
		out.Send(delta("partial"))
		out.Send(completed())
		return status.Error(codes.Internal, "failure after terminal")
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, err := receive(t, callInference(t, c, ctx))
	wantCode(t, err, codes.Internal)
	if len(events) != 2 || events[1].GetTextDelta().Text != "partial" {
		t.Fatalf("failure forwarded Completed: %v", events)
	}
}
func TestProxyCancellationReleasesOnce(t *testing.T) {
	entered := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		entered <- struct{}{}
		out.Send(started(r))
		<-out.Context().Done()
		finished <- struct{}{}
		return out.Context().Err()
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithCancel(context.Background())
	call := callInference(t, c, ctx)
	await(t, entered)
	if _, err := call.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err := call.Recv()
	wantCode(t, err, codes.Canceled)
	await(t, finished)
	s.Close()
	row := s.registry.Snapshot(time.Now())[0]
	if row.ReservedRequestId != nil {
		t.Fatal("reservation survived cancellation")
	}
	if got := s.registry.Expire(time.Now().Add(11 * time.Second)); len(got) != 0 {
		t.Fatalf("released reservation canceled again: %v", got)
	}
}
func TestPostRequestNeedsFreshIdleProbe(t *testing.T) {
	busy := atomic.Bool{}
	w := &rpcWorker{health: func(context.Context) (*meshv1.HealthResponse, error) {
		report := idleReport()
		report.Active = busy.Load()
		return &meshv1.HealthResponse{WorkerId: serviceID(1), Report: report}, nil
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := receive(t, callInference(t, c, ctx))
	if err != io.EOF {
		t.Fatal(err)
	}
	if _, err = s.Heartbeat(ctx, &meshv1.HeartbeatRequest{WorkerId: r.WorkerId, Report: idleReport()}); err != nil {
		t.Fatal(err)
	}
	_, err = s.registry.Reserve(r.Model.Id, serviceID(102), time.Now())
	wantCode(t, err, codes.Unavailable)
	busy.Store(true)
	s.probeWorker(r.WorkerId)
	_, err = s.registry.Reserve(r.Model.Id, serviceID(102), time.Now())
	wantCode(t, err, codes.ResourceExhausted)
	busy.Store(false)
	s.Heartbeat(ctx, &meshv1.HeartbeatRequest{WorkerId: r.WorkerId, Report: idleReport()})
	s.probeWorker(r.WorkerId)
	res, err := s.registry.Reserve(r.Model.Id, serviceID(102), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.registry.Release(res)
}
func TestExpiryCancelsAssignedRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		entered <- struct{}{}
		out.Send(started(r))
		<-out.Context().Done()
		finished <- struct{}{}
		return out.Context().Err()
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call := callInference(t, c, ctx)
	await(t, entered)
	if _, err := call.Recv(); err != nil {
		t.Fatal(err)
	}
	s.sweep(time.Now().Add(11 * time.Second))
	_, err := call.Recv()
	wantCode(t, err, codes.Canceled)
	await(t, finished)
	s.sweep(time.Now().Add(12 * time.Second))
	if w.calls.Load() != 1 {
		t.Fatal("expiry redispatched")
	}
}
func TestStaleProbeDuringConcurrentRequest(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	active := make(chan struct{}, 1)
	w := &rpcWorker{}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	w.health = func(ctx context.Context) (*meshv1.HealthResponse, error) {
		entered <- struct{}{}
		select {
		case <-gate:
			return nil, status.Error(codes.Unavailable, "stale failure")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w.generate = func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		out.Send(started(r))
		active <- struct{}{}
		<-out.Context().Done()
		return out.Context().Err()
	}
	done := make(chan struct{})
	go func() { s.probeWorker(r.WorkerId); close(done) }()
	await(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call := callInference(t, c, ctx)
	await(t, active)
	close(gate)
	await(t, done)
	row := s.registry.Snapshot(time.Now())[0]
	if row.State != meshv1.WorkerState_WORKER_STATE_BUSY {
		t.Fatalf("stale failure changed active state: %v", row)
	}
	cancel()
	_, err := receive(t, call)
	wantCode(t, err, codes.Canceled)
}

func TestProxyRejectsMalformedWorkerStreams(t *testing.T) {
	negative := int64(-1)
	cases := []struct {
		name   string
		events []*meshv1.InferenceEvent
	}{
		{"delta before Started", []*meshv1.InferenceEvent{delta("text")}},
		{"completion before Started", []*meshv1.InferenceEvent{completed()}},
		{"duplicate Started", []*meshv1.InferenceEvent{started(inference()), started(inference())}},
		{"wrong worker identity", []*meshv1.InferenceEvent{{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: inference().RequestId, WorkerId: serviceID(2), ModelId: inference().ModelId}}}}},
		{"wrong request identity", []*meshv1.InferenceEvent{{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: serviceID(102), WorkerId: serviceID(1), ModelId: inference().ModelId}}}}},
		{"wrong model", []*meshv1.InferenceEvent{{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: inference().RequestId, WorkerId: serviceID(1), ModelId: "other"}}}}},
		{"absent payload", []*meshv1.InferenceEvent{{}}},
		{"oversized delta", []*meshv1.InferenceEvent{started(inference()), delta(strings.Repeat("x", 4097))}},
		{"unknown finish reason", []*meshv1.InferenceEvent{started(inference()), {Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "other"}}}}},
		{"negative usage", []*meshv1.InferenceEvent{started(inference()), {Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "stop", InputTokens: &negative}}}}},
		{"duplicate completion", []*meshv1.InferenceEvent{started(inference()), completed(), completed()}},
		{"delta after completion", []*meshv1.InferenceEvent{started(inference()), completed(), delta("after")}},
		{"missing completion", []*meshv1.InferenceEvent{started(inference()), delta("partial")}},
		{"empty stream", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &rpcWorker{generate: func(_ *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
				for _, e := range tc.events {
					if err := out.Send(e); err != nil {
						return err
					}
				}
				return nil
			}}
			s, r, c := fixture(t, w)
			registerReady(t, s, r)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			events, err := receive(t, callInference(t, c, ctx))
			wantCode(t, err, codes.Internal)
			for _, e := range events {
				if e.GetCompleted() != nil {
					t.Fatal("malformed stream produced completion")
				}
			}
		})
	}
}
func TestProxyInputStatuses(t *testing.T) {
	s, r, c := fixture(t, &rpcWorker{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name   string
		change func(*meshv1.InferenceRequest)
		want   codes.Code
	}{
		{"empty cluster", func(*meshv1.InferenceRequest) {}, codes.Unavailable},
		{"unknown model", func(r *meshv1.InferenceRequest) { r.ModelId = "other" }, codes.NotFound},
		{"empty prompt", func(r *meshv1.InferenceRequest) { r.Prompt = "" }, codes.InvalidArgument},
		{"large prompt", func(r *meshv1.InferenceRequest) { r.Prompt = strings.Repeat("x", 16*1024+1) }, codes.InvalidArgument},
		{"UUID", func(r *meshv1.InferenceRequest) { r.RequestId = "bad" }, codes.InvalidArgument},
		{"output limit", func(r *meshv1.InferenceRequest) { r.MaxOutputTokens = 513 }, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := inference()
			tc.change(req)
			call, err := c.RunInference(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			_, err = receive(t, call)
			wantCode(t, err, tc.want)
		})
	}
	registerReady(t, s, r)
	req := inference()
	req.Prompt = strings.Repeat("x", 16*1024)
	call, err := c.RunInference(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = receive(t, call)
	if err != io.EOF {
		t.Fatalf("valid maximum request rejected: %v", err)
	}
}
func TestOriginalBudgetCancelsUpstream(t *testing.T) {
	entered := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	w := &rpcWorker{generate: func(_ *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		entered <- struct{}{}
		<-out.Context().Done()
		finished <- struct{}{}
		return out.Context().Err()
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	call := callInference(t, c, ctx)
	await(t, entered)
	_, err := call.Recv()
	wantCode(t, err, codes.DeadlineExceeded)
	await(t, finished)
}

type observedController struct {
	*Service
	entered, exited chan struct{}
	seen            chan context.Context
	done            chan error
	failSend        bool
}
type proxyStream struct {
	grpc.ServerStreamingServer[meshv1.InferenceEvent]
	owner *observedController
}

func (s *proxyStream) Send(e *meshv1.InferenceEvent) error {
	if s.owner.failSend && e.GetTextDelta() != nil {
		return status.Error(codes.Unavailable, "private write failure")
	}
	s.owner.entered <- struct{}{}
	err := s.ServerStreamingServer.Send(e)
	s.owner.exited <- struct{}{}
	return err
}
func (s *observedController) RunInference(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	s.seen <- out.Context()
	err := s.Service.RunInference(r, &proxyStream{ServerStreamingServer: out, owner: s})
	s.done <- err
	return err
}
func observedClient(t *testing.T, s *Service, fail bool) (meshv1.ControllerServiceClient, *observedController) {
	t.Helper()
	observed := &observedController{Service: s, entered: make(chan struct{}, 1), exited: make(chan struct{}, 1), seen: make(chan context.Context, 1), done: make(chan error, 1), failSend: fail}
	conn, _ := serverConn(t, func(server *grpc.Server) { meshv1.RegisterControllerServiceServer(server, observed) })
	return meshv1.NewControllerServiceClient(conn), observed
}
func TestExpiryCancelsFlowControlStalledProxy(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(fmt.Sprint(long), func(t *testing.T) {
			workerDone := make(chan error, 1)
			w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
				defer func() { workerDone <- out.Context().Err() }()
				if err := out.Send(started(r)); err != nil {
					return err
				}
				for i := 0; i < 4096; i++ {
					if err := out.Send(delta(strings.Repeat("x", 4096))); err != nil {
						return err
					}
				}
				return out.Send(completed())
			}}
			s, r, _ := fixture(t, w)
			registerReady(t, s, r)
			c, observed := observedClient(t, s, false)
			ctx := context.Background()
			if long {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Hour)
				defer cancel()
			}
			call := callInference(t, c, ctx)
			marked := await(t, observed.seen)
			if deadline, ok := marked.Deadline(); !ok || time.Until(deadline) > 300*time.Second {
				t.Fatal("inbound transport is not capped")
			}
			stalled := false
			for i := 0; i < 4097; i++ {
				await(t, observed.entered)
				timer := time.NewTimer(250 * time.Millisecond)
				select {
				case <-observed.exited:
					timer.Stop()
				case <-timer.C:
					stalled = true
				}
				if stalled {
					break
				}
			}
			if !stalled {
				t.Fatal("reader did not stall actual Send")
			}
			s.sweep(time.Now().Add(11 * time.Second))
			await(t, observed.exited)
			wantCode(t, await(t, observed.done), codes.Canceled)
			await(t, workerDone)
			row := s.registry.Snapshot(time.Now())[0]
			if row.ReservedRequestId != nil {
				t.Fatal("stalled request retained reservation")
			}
			if len(s.registry.Expire(time.Now().Add(12*time.Second))) != 0 {
				t.Fatal("stalled request released more than once")
			}
			if w.calls.Load() != 1 {
				t.Fatal("stalled request redispatched")
			}
			events, err := receive(t, call)
			if err == io.EOF {
				t.Fatal("expired stream ended OK")
			}
			for _, e := range events {
				if e.GetCompleted() != nil {
					t.Fatal("expired stream completed")
				}
			}
		})
	}
}
func TestFailedDownstreamSendCancelsUpstream(t *testing.T) {
	workerDone := make(chan struct{}, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		out.Send(started(r))
		out.Send(delta("partial"))
		<-out.Context().Done()
		workerDone <- struct{}{}
		return out.Context().Err()
	}}
	s, r, _ := fixture(t, w)
	registerReady(t, s, r)
	c, observed := observedClient(t, s, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call := callInference(t, c, ctx)
	await(t, observed.entered)
	await(t, observed.exited)
	events, err := receive(t, call)
	wantCode(t, err, codes.Unavailable)
	wantCode(t, await(t, observed.done), codes.Unavailable)
	await(t, workerDone)
	if len(events) != 1 || events[0].GetStarted() == nil {
		t.Fatalf("failure changed partial event stream: %v", events)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("write diagnostic leaked")
	}
	if s.registry.Snapshot(time.Now())[0].ReservedRequestId != nil {
		t.Fatal("Send failure leaked reservation")
	}
}
func TestMissingGenerationTransportFailsBeforeAdmission(t *testing.T) {
	s := New(serviceID(50), NewRegistry(serviceModel()))
	defer s.Close()
	wantCode(t, s.RunInference(inference(), &unmarkedStream{ctx: context.Background()}), codes.Internal)
}

type unmarkedStream struct {
	grpc.ServerStreamingServer[meshv1.InferenceEvent]
	ctx context.Context
}

func (s *unmarkedStream) Context() context.Context { return s.ctx }

func TestRunCoalescesProbesAndCloseJoins(t *testing.T) {
	entered := make(chan struct{}, 2)
	w := &rpcWorker{health: func(ctx context.Context) (*meshv1.HealthResponse, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s, r, c := fixture(t, w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()
	control, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := c.RegisterWorker(control, r); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	for i := 0; i < 32; i++ {
		if _, err := c.Heartbeat(control, &meshv1.HeartbeatRequest{WorkerId: r.WorkerId, Report: idleReport()}); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	queued := len(s.queue)
	s.mu.Unlock()
	if queued > 1 {
		t.Fatalf("heartbeats queued %d probes while one was active", queued)
	}
	begin := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("Close waited for probe timeout: %v", elapsed)
	}
	if err := await(t, runDone); err != nil {
		t.Fatal(err)
	}
	_, err := c.RegisterWorker(control, r)
	wantCode(t, err, codes.Unavailable)
	s.mu.Lock()
	pending := len(s.queue)
	connections := len(s.connections)
	s.mu.Unlock()
	if pending != 0 || connections != 0 {
		t.Fatalf("Close retained queued work/connections: %d/%d", pending, connections)
	}
}
func TestRunSweepsWhileProbeBlocked(t *testing.T) {
	probeEntered := make(chan struct{}, 1)
	w := &rpcWorker{health: func(ctx context.Context) (*meshv1.HealthResponse, error) {
		probeEntered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s, r, _ := fixture(t, w)
	if err := s.registry.Register(r, time.Now().Add(-11*time.Second)); err != nil {
		t.Fatal(err)
	}
	p, _ := s.registry.ProbeTarget(r.WorkerId)
	s.registry.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: r.WorkerId, Report: idleReport()}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	s.mu.Lock()
	s.enqueueLocked(r.WorkerId)
	s.mu.Unlock()
	await(t, probeEntered)
	s.sweep(time.Now().Add(60 * time.Second))
	if len(s.registry.Snapshot(time.Now())) != 0 {
		t.Fatal("blocked probe prevented registry expiry")
	}
	cancel()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestRunAutomaticallyRequiresFreshIdleProof(t *testing.T) {
	active := atomic.Bool{}
	cleaning := make(chan struct{}, 1)
	cleaned := make(chan struct{}, 1)
	cleanup := make(chan struct{})
	var probes atomic.Int32
	w := &rpcWorker{
		health: func(context.Context) (*meshv1.HealthResponse, error) {
			probes.Add(1)
			report := idleReport()
			report.Active = active.Load()
			return &meshv1.HealthResponse{WorkerId: serviceID(1), Report: report}, nil
		},
		generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
			active.Store(true)
			if err := out.Send(started(r)); err != nil {
				return err
			}
			<-out.Context().Done()
			cleaning <- struct{}{}
			<-cleanup
			active.Store(false)
			cleaned <- struct{}{}
			return out.Context().Err()
		},
	}
	s, r, c := fixture(t, w)
	owner, cancelOwner := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(owner) }()
	t.Cleanup(func() {
		cancelOwner()
		select {
		case <-cleanup:
		default:
			close(cleanup)
		}
		await(t, done)
	})
	control, cancelControl := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelControl()
	if _, err := c.RegisterWorker(control, r); err != nil {
		t.Fatal(err)
	}
	awaitClusterState(t, c, meshv1.WorkerState_WORKER_STATE_READY)
	ctx, cancel := context.WithCancel(context.Background())
	call := callInference(t, c, ctx)
	if e, err := call.Recv(); err != nil || e.GetStarted() == nil {
		t.Fatalf("no admission: %v/%v", e, err)
	}
	beforeRelease := probes.Load()
	cancel()
	await(t, cleaning)
	// Observing busy alone could be the old reservation. A fresh reverse probe is
	// required, so wait on the bounded status reader until the reservation is gone.
	row := awaitUnreservedBusy(t, c)
	if row.State != meshv1.WorkerState_WORKER_STATE_BUSY || probes.Load() <= beforeRelease {
		t.Fatalf("release skipped fresh occupied proof: %v", row)
	}
	if _, err := c.Heartbeat(control, &meshv1.HeartbeatRequest{WorkerId: r.WorkerId, Report: idleReport()}); err != nil {
		t.Fatal(err)
	}
	_, err := receive(t, callInference(t, c, control))
	if status.Code(err) != codes.Unavailable && status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("cleanup admitted overlap: %v", err)
	}
	close(cleanup)
	// The worker caller owns cleanup; explicit completion is a real gate, and a
	// heartbeat only queues a reverse proof rather than making the worker ready.
	await(t, cleaned)
	if _, err := c.Heartbeat(control, &meshv1.HeartbeatRequest{WorkerId: r.WorkerId, Report: idleReport()}); err != nil {
		t.Fatal(err)
	}
	awaitClusterState(t, c, meshv1.WorkerState_WORKER_STATE_READY)
}
func awaitClusterState(t *testing.T, c meshv1.ControllerServiceClient, want meshv1.WorkerState) *meshv1.WorkerInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		statusReply, err := c.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(statusReply.Workers) == 1 && statusReply.Workers[0].State == want {
			return statusReply.Workers[0]
		}
		select {
		case <-ctx.Done():
			t.Fatalf("status did not reach %v: %v", want, statusReply)
		case <-tick.C:
		}
	}
}
func awaitUnreservedBusy(t *testing.T, c meshv1.ControllerServiceClient) *meshv1.WorkerInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		statusReply, err := c.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(statusReply.Workers) == 1 && statusReply.Workers[0].ReservedRequestId == nil && statusReply.Workers[0].State == meshv1.WorkerState_WORKER_STATE_BUSY {
			return statusReply.Workers[0]
		}
		select {
		case <-ctx.Done():
			t.Fatal("status retained reservation")
		case <-tick.C:
		}
	}
}
func TestCloseCancelsAssignedRequestAndRejectsAdmission(t *testing.T) {
	workerDone := make(chan struct{}, 1)
	w := &rpcWorker{generate: func(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		out.Send(started(r))
		<-out.Context().Done()
		workerDone <- struct{}{}
		return out.Context().Err()
	}}
	s, r, c := fixture(t, w)
	registerReady(t, s, r)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call := callInference(t, c, ctx)
	if _, err := call.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, workerDone)
	_, err := call.Recv()
	wantCode(t, err, codes.Canceled)
	_, err = receive(t, callInference(t, c, ctx))
	wantCode(t, err, codes.Unavailable)
	if s.registry.Snapshot(time.Now())[0].ReservedRequestId != nil {
		t.Fatal("Close leaked reservation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeletedMembershipPrunesPendingProbes(t *testing.T) {
	s, r, _ := fixture(t, &rpcWorker{})
	for i := 1; i <= 32; i++ {
		registration := proto.Clone(r).(*meshv1.RegisterWorkerRequest)
		registration.WorkerId = serviceID(i)
		if _, err := s.RegisterWorker(context.Background(), registration); err != nil {
			t.Fatal(err)
		}
	}
	s.sweep(time.Now().Add(70 * time.Second))
	if len(s.registry.Snapshot(time.Now())) != 0 {
		t.Fatal("expired rows were retained")
	}
	s.mu.Lock()
	queued, pending := len(s.queue), len(s.pending)
	s.mu.Unlock()
	if queued != 0 || pending != 0 {
		t.Fatalf("deleted workers retained queued probes: %d/%d", queued, pending)
	}
}
