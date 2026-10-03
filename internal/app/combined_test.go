package app

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	"github.com/AbhinavSingh95/mica-mesh/internal/worker"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// This scenario belongs beside the private composition function: the combined
// worker must join through precisely the same real admission path as a peer.
func TestCombinedModeUsesSameAdmissionPath(t *testing.T) {
	listen := func() net.Listener {
		t.Helper()
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	cl, wl, external := listen(), listen(), listen()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	release := make(chan struct{})
	var releaseOnce sync.Once
	open := func() { releaseOnce.Do(func() { close(release) }) }
	localRT, peerRT := fakeruntime.New(), fakeruntime.New()
	localRT.ReleaseGate, peerRT.ReleaseGate = release, release
	cfg := config.Default()
	appDone := make(chan error, 1)
	peerDone := make(chan error, 1)
	memberDone := make(chan error, 1)
	serveDone := make(chan error, 1)
	peerID := "00000000-0000-4000-8000-000000000011"
	peer := worker.New(peerID, mesh.Config{Backend: cfg.Backend, Model: cfg.ModelDescriptor}, &meshv1.HardwareInfo{Hostname: "external"}, peerRT)
	server := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
	meshv1.RegisterWorkerServiceServer(server, peer)
	go func() {
		appDone <- runWithDiscovery(ctx, cfg, config.RoleController|config.RoleWorker, localRT, cl, wl, testDiscovery())
	}()
	go func() { serveDone <- server.Serve(external) }()
	go func() { peerDone <- peer.RunRuntime(ctx) }()
	go func() {
		memberDone <- worker.RunMembership(ctx, peer, external.Addr().String(), func(context.Context) (string, error) { return cl.Addr().String(), nil })
	}()
	t.Cleanup(func() {
		open()
		cancel()
		server.Stop()
		for _, done := range []<-chan error{appDone, memberDone, peerDone, serveDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(8 * time.Second):
				t.Error("combined or peer owner did not join")
			}
		}
		if localRT.Counters().Active != 0 || peerRT.Counters().Active != 0 {
			t.Error("shutdown retained runtime ownership")
		}
	})
	conn, err := grpc.NewClient(cl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := meshv1.NewControllerServiceClient(conn)
	ready := func() {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			rpc, end := context.WithTimeout(ctx, time.Second)
			rows, err := client.GetClusterStatus(rpc, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
			end()
			if err == nil && len(rows.Workers) == 2 && rows.Workers[0].State == meshv1.WorkerState_WORKER_STATE_READY && rows.Workers[1].State == meshv1.WorkerState_WORKER_STATE_READY {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("combined and external workers did not become ready")
			case <-ticker.C:
			}
		}
	}
	ready()
	infer := func(id string) grpc.ServerStreamingClient[meshv1.InferenceEvent] {
		t.Helper()
		s, err := client.RunInference(ctx, &meshv1.InferenceRequest{RequestId: id, ModelId: cfg.Model, Prompt: "hello", MaxOutputTokens: 5})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := infer("00000000-0000-4000-8000-000000000012")
	second := infer("00000000-0000-4000-8000-000000000013")
	selection := func(s grpc.ServerStreamingClient[meshv1.InferenceEvent]) string {
		t.Helper()
		event, err := s.Recv()
		if err != nil || event.GetStarted() == nil {
			t.Fatalf("Started=%v error=%v", event, err)
		}
		return event.GetStarted().WorkerId
	}
	a, b := selection(first), selection(second)
	if a == b || (a != peerID && b != peerID) {
		t.Fatalf("combined and peer selections=%q,%q", a, b)
	}
	third := infer("00000000-0000-4000-8000-000000000014")
	if _, err := third.Recv(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("combined third request=%v", err)
	}
	if localRT.Counters().Active != 1 || peerRT.Counters().Active != 1 {
		t.Fatal("roles bypassed shared capacity admission")
	}
	open()
	finish := func(s grpc.ServerStreamingClient[meshv1.InferenceEvent]) {
		t.Helper()
		event, err := s.Recv()
		if err != nil || event.GetCompleted() == nil {
			t.Fatalf("Completed=%v error=%v", event, err)
		}
		if _, err := s.Recv(); err != io.EOF {
			t.Fatalf("final stream status=%v", err)
		}
	}
	finish(first)
	finish(second)
	ready()
	fresh := infer("00000000-0000-4000-8000-000000000015")
	selection(fresh)
	finish(fresh)
	if localRT.Counters().Peak != 1 || peerRT.Counters().Peak != 1 {
		t.Fatal("combined capacity exceeded one")
	}
}
