package worker

import (
	"context"
	"io"
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

func workerClient(t *testing.T, svc meshv1.WorkerServiceServer) meshv1.WorkerServiceClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(64*1024), grpc.MaxSendMsgSize(64*1024))
	meshv1.RegisterWorkerServiceServer(server, svc)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(65535), grpc.WithInitialConnWindowSize(65535))
	if err != nil {
		server.Stop()
		<-done
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); server.Stop(); <-done })
	return meshv1.NewWorkerServiceClient(conn)
}
func TestGenerateRequiresTransportLifetime(t *testing.T) {
	rt := fakeruntime.New()
	s := ready(t, rt)
	if code := status.Code(s.Generate(request(), &stream{ctx: context.Background()})); code != codes.Internal {
		t.Errorf("missing option code=%v", code)
	}
	if rt.Counters().Peak != 0 {
		t.Fatal("unbounded transport admitted runtime")
	}
}
func TestRealGenerateFinalOK(t *testing.T) {
	rt := fakeruntime.New()
	rt.Events = []mesh.Event{{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: "hello"}, {Kind: mesh.EventCompleted, FinishReason: "stop"}}
	s := ready(t, rt)
	client := workerClient(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	call, err := client.Generate(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	var events []*meshv1.InferenceEvent
	for {
		e, err := call.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 3 || events[0].GetStarted().WorkerId != workerID || events[1].GetTextDelta().Text != "hello" || events[2].GetCompleted().FinishReason != "stop" {
		t.Errorf("stream=%v", events)
	}
}

type observedStream struct {
	grpc.ServerStreamingServer[meshv1.InferenceEvent]
	entered, exited chan struct{}
}

func (s *observedStream) Send(e *meshv1.InferenceEvent) error {
	s.entered <- struct{}{}
	err := s.ServerStreamingServer.Send(e)
	s.exited <- struct{}{}
	return err
}

type observedWorker struct {
	*Service
	entered, exited chan struct{}
	done            chan error
	seen            chan context.Context
	once            sync.Once
}

func (s *observedWorker) Generate(r *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	first := false
	s.once.Do(func() { first = true })
	if !first {
		return s.Service.Generate(r, out)
	}
	s.seen <- out.Context()
	err := s.Service.Generate(r, &observedStream{ServerStreamingServer: out, entered: s.entered, exited: s.exited})
	s.done <- err
	return err
}
func TestSupervisorCancelsFlowControlStalledSend(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-client-deadline", true: "long-client-deadline"}[long], func(t *testing.T) {
			rt := fakeruntime.New()
			cleanup := make(chan struct{})
			rt.CleanupGate = cleanup
			rt.Events = append(rt.Events, mesh.Event{Kind: mesh.EventStarted})
			for i := 0; i < 2048; i++ {
				rt.Events = append(rt.Events, mesh.Event{Kind: mesh.EventTextDelta, Text: strings.Repeat("x", 4096)})
			}
			rt.Events = append(rt.Events, mesh.Event{Kind: mesh.EventCompleted, FinishReason: "stop"})
			s := New(workerID, config(), &meshv1.HardwareInfo{}, rt)
			owner, cancelOwner := context.WithCancel(context.Background())
			park := make(chan struct{}, 1)
			supervised := make(chan error, 1)
			go func() {
				supervised <- s.runRuntime(owner, supervisorTiming{now: time.Now, wait: func(ctx context.Context, _ time.Duration) error { park <- struct{}{}; <-ctx.Done(); return ctx.Err() }})
			}()
			joined := false
			t.Cleanup(func() {
				cancelOwner()
				select {
				case <-cleanup:
				default:
					close(cleanup)
				}
				if !joined {
					resultWait(t, supervised)
				}
			})
			signalWait(t, park)
			observed := &observedWorker{Service: s, entered: make(chan struct{}, 1), exited: make(chan struct{}, 1), done: make(chan error, 1), seen: make(chan context.Context, 1)}
			client := workerClient(t, observed)
			ctx := context.Background()
			if long {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Hour)
				defer cancel()
			}
			call, err := client.Generate(ctx, request())
			if err != nil {
				t.Fatal(err)
			}
			var marked context.Context
			select {
			case marked = <-observed.seen:
			case <-time.After(2 * time.Second):
				t.Fatal("no handler")
			}
			if deadline, ok := marked.Deadline(); !ok || time.Until(deadline) > 300*time.Second {
				t.Fatal("uncapped inbound transport")
			}
			// Entry/exit bracket the REAL Send. A bounded no-progress observation identifies
			// a blocked flow-control write; it is not a scheduling sleep or a retry loop.
			stalled := false
			for sends := 0; sends < 2049; sends++ {
				signalWait(t, observed.entered)
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
				t.Fatal("client without reads did not backpressure transport")
			}
			cancelOwner()
			signalWait(t, rt.Cleaning)
			if !s.Report().Active {
				t.Fatal("cleanup lost ownership")
			}
			overlap, err := client.Generate(context.Background(), request())
			if err != nil {
				t.Fatal(err)
			}
			_, err = overlap.Recv()
			if status.Code(err) != codes.ResourceExhausted {
				t.Errorf("cleanup overlap=%v", err)
			}
			close(cleanup)
			if code := status.Code(resultWait(t, observed.done)); code != codes.Canceled {
				t.Errorf("stalled request ended with %v", code)
			}
			if err := resultWait(t, supervised); err != nil {
				t.Error(err)
			}
			joined = true
			if rt.Counters().Active != 0 || rt.Counters().Peak != 1 {
				t.Fatal("runtime cleanup incomplete")
			}
			for {
				e, err := call.Recv()
				if err != nil {
					if err == io.EOF {
						t.Error("canceled stalled stream ended OK")
					}
					break
				}
				if e.GetCompleted() != nil {
					t.Error("canceled stream completed")
				}
			}
		})
	}
}
