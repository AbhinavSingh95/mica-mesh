package protocol

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type timedSender struct {
	meshv1.UnimplementedWorkerServiceServer
	done chan error
}

func (s *timedSender) Generate(_ *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	cancel, ok := GenerationCancel(out.Context())
	if ok {
		defer cancel()
	}
	for i := 0; i < 4096; i++ {
		err := out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: strings.Repeat("x", 4096)}}})
		if err != nil {
			s.done <- err
			return err
		}
	}
	s.done <- nil
	return nil
}
func TestTransportDeadlineUnblocksRealSend(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &timedSender{done: make(chan error, 1)}
	server := grpc.NewServer(generationServerOption(100 * time.Millisecond))
	meshv1.RegisterWorkerServiceServer(server, fixture)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithInitialWindowSize(65535), grpc.WithInitialConnWindowSize(65535))
	if err != nil {
		server.Stop()
		<-served
		t.Fatal(err)
	}
	defer func() { conn.Close(); server.Stop(); <-served }()
	_, err = meshv1.NewWorkerServiceClient(conn).Generate(context.Background(), &meshv1.InferenceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fixture.done:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Errorf("stalled transport timer error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport timer did not unblock Send")
	}
}
