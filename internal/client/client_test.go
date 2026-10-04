package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The fixtures use the real wire transport; only the remote Controller is controlled.
type controller struct {
	meshv1.UnimplementedControllerServiceServer
	generate func(*meshv1.InferenceRequest, grpc.ServerStreamingServer[meshv1.InferenceEvent]) error
	status   func(context.Context) (*meshv1.GetClusterStatusResponse, error)
}

func (s *controller) RunInference(req *meshv1.InferenceRequest, stream grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	return s.generate(req, stream)
}
func (s *controller) GetClusterStatus(ctx context.Context, _ *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	if s.status != nil {
		return s.status(ctx)
	}
	return &meshv1.GetClusterStatusResponse{ControllerId: "550e8400-e29b-41d4-a716-446655440000"}, nil
}

type countedListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *countedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return conn, err
}
func serve(t *testing.T, s *controller) (*Client, *countedListener) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countedListener{Listener: listener}
	server := grpc.NewServer(protocol.GenerationServerOption())
	meshv1.RegisterControllerServiceServer(server, s)
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(counted) }()
	t.Cleanup(func() { server.Stop(); <-done })
	client, err := New(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, counted
}
func request() *meshv1.InferenceRequest {
	return &meshv1.InferenceRequest{RequestId: "550e8400-e29b-41d4-a716-446655440001", ModelId: "test-model", Prompt: "hello", MaxOutputTokens: 128}
}
func started(req *meshv1.InferenceRequest) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: req.RequestId, ModelId: req.ModelId, WorkerId: "550e8400-e29b-41d4-a716-446655440002", WorkerHostname: "test-host"}}}
}
func delta(text string) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: text}}}
}
func completed() *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "stop"}}}
}
func send(stream grpc.ServerStreamingServer[meshv1.InferenceEvent], events ...*meshv1.InferenceEvent) error {
	for _, event := range events {
		if err := stream.Send(event); err != nil {
			return err
		}
	}
	return nil
}
func budget(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEventOrderRequiresFinalOK(t *testing.T) {
	// Omitting any validator or treating Completed as final success must fail a case.
	for _, name := range []string{"valid", "empty-answer", "length", "missing-start", "early-complete", "empty", "unknown", "duplicate-start", "duplicate-complete", "late-text", "missing-complete", "empty-text", "oversized-text", "wrong-request", "wrong-model", "bad-worker", "empty-host", "oversized-host", "bad-reason", "negative-input", "negative-output", "completed-with-error"} {
		t.Run(name, func(t *testing.T) {
			req := request()
			first, last := started(req), completed()
			events := []*meshv1.InferenceEvent{first, delta("héllo"), last}
			var final error
			switch name {
			case "empty-answer":
				events = []*meshv1.InferenceEvent{first, last}
			case "length":
				last.GetCompleted().FinishReason = "length"
			case "missing-start":
				events = events[1:]
			case "early-complete":
				events = []*meshv1.InferenceEvent{last}
			case "empty":
				events = nil
			case "unknown":
				events = []*meshv1.InferenceEvent{{}}
			case "duplicate-start":
				events = []*meshv1.InferenceEvent{first, first, last}
			case "duplicate-complete":
				events = append(events, last)
			case "late-text":
				events = append(events, delta("late"))
			case "missing-complete":
				events = events[:2]
			case "empty-text":
				events[1] = delta("")
			case "oversized-text":
				events[1] = delta(strings.Repeat("x", 4097))
			case "wrong-request":
				first.GetStarted().RequestId = uuid.NewString()
			case "wrong-model":
				first.GetStarted().ModelId = "other"
			case "bad-worker":
				first.GetStarted().WorkerId = "bad"
			case "empty-host":
				first.GetStarted().WorkerHostname = ""
			case "oversized-host":
				first.GetStarted().WorkerHostname = strings.Repeat("x", 1025)
			case "bad-reason":
				last.GetCompleted().FinishReason = "other"
			case "negative-input":
				n := int64(-1)
				last.GetCompleted().InputTokens = &n
			case "negative-output":
				n := int64(-1)
				last.GetCompleted().OutputTokens = &n
			case "completed-with-error":
				final = status.Error(codes.Unavailable, "transport failed")
			}
			c, _ := serve(t, &controller{generate: func(_ *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
				if err := send(out, events...); err != nil {
					return err
				}
				return final
			}})
			var text strings.Builder
			err := c.Generate(budget(t), req, func(event *meshv1.InferenceEvent) error { text.WriteString(event.GetTextDelta().GetText()); return nil })
			success := name == "valid" || name == "empty-answer" || name == "length"
			if (err == nil) != success {
				t.Fatalf("Generate error=%v, want success=%t", err, success)
			}
			if success && name != "empty-answer" && text.String() != "héllo" {
				t.Fatalf("text=%q", text.String())
			}
			if name == "completed-with-error" && status.Code(err) != codes.Unavailable {
				t.Fatalf("final status=%v", err)
			}
		})
	}
}
func TestPartialOutputSurvivesTransportFailure(t *testing.T) {
	c, _ := serve(t, &controller{generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		if err := send(out, started(req), delta("partial 世界")); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "lost transport")
	}})
	var text strings.Builder
	err := c.Generate(budget(t), request(), func(event *meshv1.InferenceEvent) error { text.WriteString(event.GetTextDelta().GetText()); return nil })
	if text.String() != "partial 世界" || status.Code(err) != codes.Unavailable {
		t.Fatalf("text=%q error=%v", text.String(), err)
	}
}
func TestConsumerFailureCancelsStream(t *testing.T) {
	for _, stage := range []struct {
		name  string
		calls int
	}{{"Started", 1}, {"TextDelta", 2}, {"Completed", 3}} {
		t.Run(stage.name, func(t *testing.T) {
			canceled := make(chan struct{})
			c, _ := serve(t, &controller{generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
				// Cancellation can reach Send before all buffered events are sent.
				sendErr := send(out, started(req), delta("partial"), completed())
				<-out.Context().Done()
				close(canceled)
				return errors.Join(sendErr, out.Context().Err())
			}})
			consumerErr := errors.New("consumer failed")
			calls := 0
			err := c.Generate(budget(t), request(), func(event *meshv1.InferenceEvent) error {
				calls++
				if calls == stage.calls {
					return consumerErr
				}
				return nil
			})
			if !errors.Is(err, consumerErr) || calls != stage.calls {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("consumer failure did not cancel the RPC")
			}
			// Reuse proves that a canceled request does not close the client.
			if _, err := c.Status(budget(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestEarlierDeadlinePreserved(t *testing.T) {
	observed := make(chan time.Time, 2)
	c, _ := serve(t, &controller{
		generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
			deadline, ok := out.Context().Deadline()
			if !ok {
				return status.Error(codes.Internal, "no deadline")
			}
			observed <- deadline
			return send(out, started(req), completed())
		},
		status: func(ctx context.Context) (*meshv1.GetClusterStatusResponse, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, status.Error(codes.Internal, "no deadline")
			}
			observed <- deadline
			return &meshv1.GetClusterStatusResponse{ControllerId: "550e8400-e29b-41d4-a716-446655440000"}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := c.Generate(ctx, request(), func(*meshv1.InferenceEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := <-observed; got.After(deadline.Add(10 * time.Millisecond)) {
			t.Fatalf("deadline extended: got %v want <= %v", got, deadline)
		}
	}
}
func TestStatusHasLargerReceiveBound(t *testing.T) {
	for _, test := range []struct {
		name  string
		bytes int
		code  codes.Code
	}{{"above-generation-limit", 100 * 1024, codes.OK}, {"above-status-limit", 1024 * 1024, codes.ResourceExhausted}} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := serve(t, &controller{status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
				return &meshv1.GetClusterStatusResponse{ControllerId: "550e8400-e29b-41d4-a716-446655440000", Workers: []*meshv1.WorkerInfo{{LastError: strings.Repeat("x", test.bytes)}}}, nil
			}})
			response, err := c.Status(budget(t))
			if status.Code(err) != test.code {
				t.Fatalf("error=%v want code=%v", err, test.code)
			}
			if err == nil && len(response.Workers[0].LastError) != test.bytes {
				t.Fatal("status data truncated")
			}
		})
	}
	c, _ := serve(t, &controller{generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		return send(out, started(req), delta(strings.Repeat("x", 100*1024)))
	}})
	if err := c.Generate(budget(t), request(), func(*meshv1.InferenceEvent) error { return nil }); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("generation receive limit error=%v", err)
	}
}
func TestConnectionReuseAndClose(t *testing.T) {
	c, listener := serve(t, &controller{generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		return send(out, started(req), completed())
	}})
	for range 3 {
		if _, err := c.Status(budget(t)); err != nil {
			t.Fatal(err)
		}
		if err := c.Generate(budget(t), request(), func(*meshv1.InferenceEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got := listener.accepted.Load(); got != 1 {
		t.Fatalf("opened %d connections, want 1", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(budget(t)); err == nil {
		t.Fatal("closed client accepted status")
	}
	if err := c.Generate(budget(t), request(), func(*meshv1.InferenceEvent) error { return nil }); err == nil {
		t.Fatal("closed client accepted generation")
	}
}
func TestRequestValidationBeforeDispatch(t *testing.T) {
	var dispatched atomic.Int32
	c, _ := serve(t, &controller{generate: func(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
		dispatched.Add(1)
		return send(out, started(req), completed())
	}})
	for _, name := range []string{"nil", "bad-id", "empty-prompt", "oversized-prompt", "invalid-utf8", "zero-output", "oversized-output"} {
		t.Run(name, func(t *testing.T) {
			req := request()
			switch name {
			case "nil":
				req = nil
			case "bad-id":
				req.RequestId = "bad"
			case "empty-prompt":
				req.Prompt = ""
			case "oversized-prompt":
				req.Prompt = strings.Repeat("x", 16385)
			case "invalid-utf8":
				req.Prompt = string([]byte{255})
			case "zero-output":
				req.MaxOutputTokens = 0
			case "oversized-output":
				req.MaxOutputTokens = 513
			}
			if err := c.Generate(budget(t), req, func(*meshv1.InferenceEvent) error { return nil }); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("validation error=%v", err)
			}
		})
	}
	if dispatched.Load() != 0 {
		t.Fatal("invalid request dispatched")
	}
}
func TestStatusRejectsMalformedIdentity(t *testing.T) {
	c, _ := serve(t, &controller{status: func(context.Context) (*meshv1.GetClusterStatusResponse, error) {
		return &meshv1.GetClusterStatusResponse{ControllerId: "bad"}, nil
	}})
	if _, err := c.Status(budget(t)); err == nil {
		t.Fatal("malformed status identity accepted")
	}
}
