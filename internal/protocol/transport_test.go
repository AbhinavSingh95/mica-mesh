package protocol_test

import (
	"context"
	"io"
	"net"
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
