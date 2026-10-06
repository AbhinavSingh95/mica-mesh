package protocol_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type transportServer struct {
	meshv1.UnimplementedWorkerServiceServer
	t       *testing.T
	seen    chan context.Context
	release chan struct{}
}

func (s *transportServer) Generate(_ *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	ctx := out.Context()
	s.seen <- ctx
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}
func TestGenerationTransportContext(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-client-deadline", true: "short-client-deadline"}[short], func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			s := &transportServer{t: t, seen: make(chan context.Context, 1), release: make(chan struct{})}
			server := grpc.NewServer(protocol.GenerationServerOption())
			meshv1.RegisterWorkerServiceServer(server, s)
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() { server.Stop(); <-done }()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx := context.Background()
			if short {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
			}
			call, err := meshv1.NewWorkerServiceClient(conn).Generate(ctx, &meshv1.InferenceRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var captured context.Context
			select {
			case captured = <-s.seen:
			case <-time.After(time.Second):
				t.Fatal("handler did not start")
			}
			deadline, ok := captured.Deadline()
			if !ok {
				t.Error("generation has no transport deadline")
			} else {
				remaining := time.Until(deadline)
				if remaining > 300*time.Second || (!short && remaining < 299*time.Second) || (short && remaining > 20*time.Second) {
					t.Errorf("remaining=%v", remaining)
				}
			}
			cancel, ok := protocol.GenerationCancel(captured)
			if !ok {
				t.Error("generation handler cannot cancel transport")
				close(s.release)
			} else {
				cancel()
				select {
				case <-captured.Done():
				case <-time.After(time.Second):
					t.Error("transport cancellation did not complete")
				}
			}
			_, err = call.Recv()
			if err != io.EOF && status.Code(err) != codes.Canceled {
				t.Errorf("canceled transport ended with %v", err)
			}
		})
	}
}
func TestGenerationCancelRejectsUninstalledTransport(t *testing.T) {
	if _, ok := protocol.GenerationCancel(context.Background()); ok {
		t.Fatal("uninstalled context accepted")
	}
}

// This server deliberately sends oversized generation/control data to exercise
// the composition limits even when a remote service violates its event contract.
type limitServer struct {
	meshv1.UnimplementedControllerServiceServer
	meshv1.UnimplementedWorkerServiceServer
	rows int
}

func (s *limitServer) GetClusterStatus(context.Context, *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	reply := &meshv1.GetClusterStatusResponse{}
	for i := 0; i < s.rows; i++ {
		reply.Workers = append(reply.Workers, &meshv1.WorkerInfo{LastError: strings.Repeat("x", 4096)})
	}
	return reply, nil
}
func (s *limitServer) Health(context.Context, *meshv1.HealthRequest) (*meshv1.HealthResponse, error) {
	return &meshv1.HealthResponse{Report: &meshv1.WorkerReport{LastError: strings.Repeat("x", 70*1024)}}, nil
}
func (s *limitServer) Generate(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	text := "ok"
	if req.Prompt == "oversized response" {
		text = strings.Repeat("x", 70*1024)
	}
	return out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: text}}})
}
func TestGenerationAndClusterStatusHaveSeparateMessageLimits(t *testing.T) {
	for _, rows := range []int{20, 300} {
		t.Run(fmt.Sprint(rows), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
			svc := &limitServer{rows: rows}
			meshv1.RegisterControllerServiceServer(server, svc)
			meshv1.RegisterWorkerServiceServer(server, svc)
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			defer func() { server.Stop(); <-done }()
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			reply, err := meshv1.NewControllerServiceClient(conn).GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
			if rows == 20 {
				if err != nil || len(reply.Workers) != 20 {
					t.Fatalf("bounded multi-row status rejected: %v/%v", reply, err)
				}
			} else if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("unbounded cluster status accepted: %v", err)
			}
			worker := meshv1.NewWorkerServiceClient(conn)
			call, err := worker.Generate(ctx, &meshv1.InferenceRequest{Prompt: strings.Repeat("x", 16*1024)}, grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := call.Recv(); err != nil {
				t.Fatalf("valid maximum prompt rejected: %v", err)
			}
			if _, err := call.Recv(); err != io.EOF {
				t.Fatal(err)
			}
			call, err = worker.Generate(ctx, &meshv1.InferenceRequest{Prompt: "oversized response"}, grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := call.Recv(); status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("generation receive cap ineffective: %v", err)
			}
			_, err = worker.Health(ctx, &meshv1.HealthRequest{}, grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes))
			if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("ordinary control response cap ineffective: %v", err)
			}
			call, err = worker.Generate(ctx, &meshv1.InferenceRequest{Prompt: strings.Repeat("x", 70*1024)}, grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes))
			if err == nil {
				_, err = call.Recv()
			}
			if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("generation send cap ineffective: %v", err)
			}
		})
	}
}
