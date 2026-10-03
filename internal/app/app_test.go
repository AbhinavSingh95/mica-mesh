package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestShutdownStopsFakeRuntime(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt := fakeruntime.New()
	cfg := config.Default()
	cfg.ControllerAddress = "127.0.0.1:1"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, config.RoleWorker, rt, nil, l) }()
	select {
	case <-rt.Started:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("runtime not started independently")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("shutdown not joined")
	}
	if rt.Counters().Stops != 1 {
		t.Fatalf("counters=%+v", rt.Counters())
	}
}

func TestUnknownHardwareStillJoins(t *testing.T)              { exerciseCombined(t) }
func TestExplicitRequestStreamsBeforeCompletion(t *testing.T) { exerciseCombined(t) }
func exerciseCombined(t *testing.T) {
	exerciseBoundCombined(t, "127.0.0.1:0")
}
func TestCombinedUsesBoundControllerAddress(t *testing.T) {
	got := controllerEndpoint(&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 4321})
	if got != "192.0.2.1:4321" {
		t.Fatalf("controller endpoint=%q", got)
	}
}
func exerciseBoundCombined(t *testing.T, bind string) {
	t.Helper()
	cl, err := net.Listen("tcp4", bind)
	if err != nil {
		t.Fatal(err)
	}
	wl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	rt := fakeruntime.New()
	rt.CleanupGate = release
	rt.Events = []mesh.Event{{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: "incremental"}, {Kind: mesh.EventCompleted, FinishReason: "stop"}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := config.Default()
	done := make(chan error, 1)
	go func() {
		done <- runWithDiscovery(ctx, cfg, config.RoleController|config.RoleWorker, rt, cl, wl, testDiscovery())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("composition did not join")
		}
	})
	conn, err := grpc.NewClient(cl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := meshv1.NewControllerServiceClient(conn)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		row, err := client.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{})
		if err == nil && len(row.Workers) == 1 && row.Workers[0].State == meshv1.WorkerState_WORKER_STATE_READY {
			if row.Workers[0].Hardware == nil || row.Workers[0].Hardware.Gpu != nil {
				t.Fatalf("hardware=%v", row.Workers[0].Hardware)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker did not become ready: %v", err)
		case <-ticker.C:
		}
	}
	stream, err := client.RunInference(ctx, &meshv1.InferenceRequest{RequestId: "fe5161fd-8c94-4a23-b227-0bf7c26b5321", ModelId: cfg.Model, Prompt: "hello", MaxOutputTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetStarted() == nil {
		t.Fatalf("first=%v error=%v", first, err)
	}
	text, err := stream.Recv()
	if err != nil || text.GetTextDelta().GetText() != "incremental" {
		t.Fatalf("text=%v error=%v", text, err)
	}
	if rt.Counters().Active != 1 {
		t.Fatal("text not observed during active cleanup")
	}
	close(release)
	terminal, err := stream.Recv()
	if err != nil || terminal.GetCompleted() == nil {
		t.Fatalf("terminal=%v error=%v", terminal, err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("final status=%v", err)
	}
	cancel()
}
func TestEndpointUsesListenerPort(t *testing.T) {
	address, err := workerEndpointFor(context.Background(), "", "127.0.0.1:1234", &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 4321}, true)
	if err != nil || address != "127.0.0.1:4321" {
		t.Fatalf("endpoint=%q error=%v", address, err)
	}
}

func TestRemoteWorkerRejectsLoopback(t *testing.T) {
	_, err := workerEndpointFor(context.Background(), "", "127.0.0.1:1234", &net.TCPAddr{IP: net.IPv4zero, Port: 4321}, false)
	if err == nil {
		t.Fatal("remote worker registered loopback")
	}
}
func TestWorkerOverrideMustFitListener(t *testing.T) {
	_, err := workerEndpointFor(context.Background(), "192.0.2.1", "192.0.2.2:1234", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4321}, false)
	if err == nil {
		t.Fatal("accepted nonlocal/mismatched override")
	}
}

func testDiscovery() discoveryEffects {
	return discoveryEffects{resolve: discovery.Resolve, advertise: func(context.Context, discovery.ControllerInfo) (func(), error) { return func() {}, nil }, localIPv4: func(string, net.IP) (string, error) { return "192.0.2.1", nil }}
}

func TestAdvertisementMatchesControllerAndStops(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			cl, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			published := make(chan discovery.ControllerInfo, 1)
			stopped := make(chan struct{})
			effects := testDiscovery()
			effects.advertise = func(ctx context.Context, info discovery.ControllerInfo) (func(), error) {
				published <- info
				if unavailable {
					return nil, errors.New("multicast unavailable")
				}
				return func() { close(stopped) }, nil
			}
			done := make(chan error, 1)
			go func() { done <- runWithDiscovery(ctx, config.Default(), config.RoleController, nil, cl, nil, effects) }()
			var info discovery.ControllerInfo
			select {
			case info = <-published:
			case <-time.After(time.Second):
				t.Fatal("advertisement not started")
			}
			if info.Port != cl.Addr().(*net.TCPAddr).Port || info.IPv4 != "192.0.2.1" || info.Hostname == "" || info.ProtocolMajor != 1 {
				t.Fatalf("advertisement=%+v", info)
			}
			conn, err := grpc.NewClient(cl.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			rpc, end := context.WithTimeout(ctx, time.Second)
			defer end()
			state, err := meshv1.NewControllerServiceClient(conn).GetClusterStatus(rpc, &meshv1.GetClusterStatusRequest{})
			if err != nil || state.GetControllerId() != info.InstanceID {
				t.Fatalf("status=%v error=%v advertised=%v", state, err, info)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("app did not join")
			}
			if !unavailable {
				select {
				case <-stopped:
				default:
					t.Fatal("advertisement outlived app")
				}
			}
		})
	}
}
func TestInvalidControllerInterfaceClosesListeners(t *testing.T) {
	cl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	effects := testDiscovery()
	effects.localIPv4 = func(string, net.IP) (string, error) { return "", errors.New("ambiguous --advertise-address") }
	if err := runWithDiscovery(context.Background(), config.Default(), config.RoleController|config.RoleWorker, fakeruntime.New(), cl, wl, effects); err == nil {
		t.Fatal("accepted ambiguous interface")
	}
	for _, listener := range []net.Listener{cl, wl} {
		if err := listener.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("listener leaked: %v", err)
		}
	}
}
