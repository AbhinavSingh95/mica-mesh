package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime/llamacpp"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestProcessCloseReapsOwnedRuntime(t *testing.T) {
	pid := make(chan int, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/launch" {
			var launch struct{ PID int }
			if err := json.NewDecoder(r.Body).Decode(&launch); err != nil {
				t.Error(err)
				return
			}
			pid <- launch.PID
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer control.Close()
	t.Setenv("MICA_TEST_CONTROL", control.URL)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cfg := config.Default()
	cfg.Backend = "cpu"
	cfg.RuntimeBinary = filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("MICA_TEST_SERVER"))
	cfg.ModelPath = filepath.Join(t.TempDir(), "model.gguf")
	data := []byte("test gguf")
	if err := os.WriteFile(cfg.ModelPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	cfg.ModelDescriptor.SHA256 = hex.EncodeToString(digest[:])
	cfg.ControllerAddress = "127.0.0.1:1"
	runtimePort, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimePort = runtimePort.Addr().(*net.TCPAddr).Port
	_ = runtimePort.Close()
	wl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.ControllerListen = "127.0.0.1:0"
	cfg.WorkerListen = "127.0.0.1:0"
	p, err := startWithDiscovery(ctx, cfg, config.RoleWorker, llamacpp.New(), nil, wl, Options{Network: Local}, testDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	var ownedPID int
	select {
	case ownedPID = <-pid:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("runtime child not launched")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runtime shutdown not joined")
	}
	process, err := os.FindProcess(ownedPID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("owned runtime child %d still alive or unreaped", ownedPID)
	}
}

func localConfig() config.Config {
	cfg := config.Default()
	cfg.ControllerListen = "127.0.0.1:0"
	cfg.WorkerListen = "127.0.0.1:0"
	cfg.RuntimeBinary = "/prepared/llama-server"
	cfg.ModelPath = "/prepared/model.gguf"
	return cfg
}
func TestSeparateLocalRolesNeverUseDiscovery(t *testing.T) {
	effects := discoveryEffects{
		resolve: func(context.Context, string) (string, error) {
			t.Error("local resolver called")
			return "", errors.New("unexpected")
		},
		advertise: func(context.Context, discovery.ControllerInfo) (func(), error) {
			t.Error("local advertisement called")
			return nil, errors.New("unexpected")
		},
		localIPv4: func(string, net.IP) (string, error) {
			t.Error("local interface selection called")
			return "", errors.New("unexpected")
		},
	}
	for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		cfg := localConfig()
		var cl, wl net.Listener
		if role == config.RoleController {
			cl = l
		} else {
			wl = l
			cfg.ControllerAddress = "127.0.0.1:1"
		}
		p, err := startWithDiscovery(context.Background(), cfg, role, fakeruntime.New(), cl, wl, Options{Network: Local, ResolveController: effects.resolve}, effects)
		if err != nil {
			t.Fatal(err)
		}
		if role == config.RoleController && p.ControllerAddress() != l.Addr().String() {
			t.Errorf("bound controller=%q", p.ControllerAddress())
		}
		if role == config.RoleWorker {
			s, ok := p.AgentStatus()
			if !ok || s.Endpoint != l.Addr().String() {
				t.Errorf("Agent status=%+v present=%v", s, ok)
			}
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestProcessStartupFailureClosesListeners(t *testing.T) {
	cl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	wl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer wl.Close()
	effects := testDiscovery()
	effects.localIPv4 = func(string, net.IP) (string, error) { return "", errors.New("no interface") }
	if _, err := startWithDiscovery(context.Background(), config.Default(), config.RoleController|config.RoleWorker, fakeruntime.New(), cl, wl, Options{}, effects); err == nil {
		t.Fatal("startup succeeded")
	}
	for _, l := range []net.Listener{cl, wl} {
		if err := l.Close(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("listener not closed: %v", err)
		}
	}
}
func TestAgentStatusCopiesReport(t *testing.T) {
	cfg := localConfig()
	cfg.ControllerAddress = "127.0.0.1:1"
	wl, err := net.Listen("tcp4", cfg.WorkerListen)
	if err != nil {
		t.Fatal(err)
	}
	defer wl.Close()
	rt := fakeruntime.New()
	rt.StartGate = make(chan struct{})
	p, err := startWithDiscovery(context.Background(), cfg, config.RoleWorker, rt, nil, wl, Options{Network: Local}, testDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case <-rt.Started:
	case <-time.After(time.Second):
		t.Fatal("runtime not started")
	}
	first, ok := p.AgentStatus()
	if !ok || first.Report == nil || first.WorkerID == "" {
		t.Fatalf("snapshot=%+v", first)
	}
	first.Report.LastError = "mutated"
	first.Membership.Registered = true
	second, _ := p.AgentStatus()
	if second.Report.LastError == "mutated" || second.Membership.Registered {
		t.Fatal("snapshot mutated owned state")
	}
	if second.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_STARTING {
		t.Fatalf("state=%v", second.Report)
	}
}

func startLocalTest(t *testing.T, role config.Role, target string, rt mesh.Runtime, listener net.Listener) *Process {
	t.Helper()
	cfg := localConfig()
	cfg.ControllerAddress = target
	var cl, wl net.Listener
	if role == config.RoleController {
		cl = listener
	} else {
		wl = listener
	}
	p, err := startWithDiscovery(context.Background(), cfg, role, rt, cl, wl, Options{Network: Local}, testDiscovery())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}
func localListener(t *testing.T, address string) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func waitAgent(t *testing.T, p *Process, predicate func(AgentStatus) bool) AgentStatus {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		s, ok := p.AgentStatus()
		if ok && predicate(s) {
			return s
		}
		select {
		case <-timer.C:
			t.Fatalf("Agent state=%+v", s)
		case <-tick.C:
		}
	}
}
func TestAgentStartsBeforeController(t *testing.T) { separateOrder(t, true) }
func TestControllerStartsBeforeAgent(t *testing.T) { separateOrder(t, false) }
func separateOrder(t *testing.T, agentFirst bool) {
	cl := localListener(t, "127.0.0.1:0")
	wl := localListener(t, "127.0.0.1:0")
	rt := fakeruntime.New()
	var controller, agent *Process
	if !agentFirst {
		controller = startLocalTest(t, config.RoleController, "", nil, cl)
	}
	agent = startLocalTest(t, config.RoleWorker, cl.Addr().String(), rt, wl)
	if agentFirst {
		s := waitAgent(t, agent, func(s AgentStatus) bool { return s.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY })
		if s.Membership.Registered {
			t.Fatal("connected without a Controller")
		}
		controller = startLocalTest(t, config.RoleController, "", nil, cl)
	}
	waitAgent(t, agent, func(s AgentStatus) bool { return s.Membership.Registered })
	if _, ok := controller.AgentStatus(); ok {
		t.Fatal("Controller acquired an Agent")
	}
}
func TestAgentRetainsRuntimeAcrossControllerRestart(t *testing.T) {
	cl := localListener(t, "127.0.0.1:0")
	address := cl.Addr().String()
	controller := startLocalTest(t, config.RoleController, "", nil, cl)
	rt := fakeruntime.New()
	agent := startLocalTest(t, config.RoleWorker, address, rt, localListener(t, "127.0.0.1:0"))
	first := waitAgent(t, agent, func(s AgentStatus) bool {
		return s.Membership.Registered && s.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY
	})
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	waitAgent(t, agent, func(s AgentStatus) bool { return !s.Membership.Registered })
	startLocalTest(t, config.RoleController, "", nil, localListener(t, address))
	last := waitAgent(t, agent, func(s AgentStatus) bool { return s.Membership.Registered })
	if first.WorkerID != last.WorkerID || first.Endpoint != last.Endpoint || rt.Counters().Starts != 1 || rt.Counters().Stops != 0 {
		t.Fatalf("identity/runtime changed: first=%+v last=%+v counts=%+v", first, last, rt.Counters())
	}
}
func TestClosingOneRolePreservesOther(t *testing.T) {
	controller := startLocalTest(t, config.RoleController, "", nil, localListener(t, "127.0.0.1:0"))
	agent := startLocalTest(t, config.RoleWorker, controller.ControllerAddress(), fakeruntime.New(), localListener(t, "127.0.0.1:0"))
	waitAgent(t, agent, func(s AgentStatus) bool { return s.Membership.Registered })
	if err := agent.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(controller.ControllerAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := meshv1.NewControllerServiceClient(conn).GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{}); err != nil {
		t.Fatalf("Controller stopped with Agent: %v", err)
	}
}

type stopRuntime struct {
	*fakeruntime.Runtime
	entered, release chan struct{}
	stopErr          error
}

func (r *stopRuntime) Stop(ctx context.Context) error {
	close(r.entered)
	<-r.release
	return errors.Join(r.Runtime.Stop(ctx), r.stopErr)
}
func TestProcessCloseJoinsAndReaps(t *testing.T) {
	rt := &stopRuntime{Runtime: fakeruntime.New(), entered: make(chan struct{}), release: make(chan struct{}), stopErr: errors.New("stop failure")}
	wl := localListener(t, "127.0.0.1:0")
	cfg := localConfig()
	cfg.ControllerAddress = "127.0.0.1:1"
	p, err := startWithDiscovery(context.Background(), cfg, config.RoleWorker, rt, nil, wl, Options{Network: Local}, testDiscovery())
	if err != nil {
		wl.Close()
		t.Fatal(err)
	}
	waitAgent(t, p, func(s AgentStatus) bool { return s.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY })
	done := make(chan error, 8)
	for range 4 {
		go func() { done <- p.Close() }()
		go func() { done <- p.Wait() }()
	}
	select {
	case <-rt.entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup not started")
	}
	select {
	case <-done:
		t.Fatal("returned before cleanup")
	default:
	}
	close(rt.release)
	for range 8 {
		select {
		case err := <-done:
			if !errors.Is(err, rt.stopErr) {
				t.Errorf("cleanup failure lost: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("caller not joined")
		}
	}
	if rt.Counters().Stops != 1 {
		t.Fatalf("counts=%+v", rt.Counters())
	}
}

type closeErrorListener struct {
	net.Listener
	closeErr error
}

func (l *closeErrorListener) Close() error {
	err := l.Listener.Close()
	if !errors.Is(err, net.ErrClosed) {
		return errors.Join(err, l.closeErr)
	}
	return err
}
func TestProcessPropagatesListenerCleanupFailure(t *testing.T) {
	want := errors.New("listener cleanup failed")
	l := &closeErrorListener{Listener: localListener(t, "127.0.0.1:0"), closeErr: want}
	p, err := startWithDiscovery(context.Background(), localConfig(), config.RoleController, nil, l, nil, Options{Network: Local}, testDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); !errors.Is(err, want) {
		t.Fatalf("cleanup failure lost: %v", err)
	}
}
func TestProcessParentCancellationBeforeOwnersStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := Start(ctx, localConfig(), config.RoleController, Options{Network: Local})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled process did not join")
	}
	if conn, err := net.DialTimeout("tcp4", p.ControllerAddress(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener survived cancellation")
	}
}
func TestLocalStartPreservesExplicitEndpointsAndRejectsRemote(t *testing.T) {
	cfg := localConfig()
	p, err := Start(context.Background(), cfg, config.RoleController, Options{Network: Local})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.ControllerAddress() == "127.0.0.1:0" || p.ControllerAddress() == "127.0.0.1:50051" {
		t.Fatalf("listener override reset: %s", p.ControllerAddress())
	}
	cfg.ControllerListen = "192.0.2.1:0"
	if _, err := Start(context.Background(), cfg, config.RoleController, Options{Network: Local}); err == nil {
		t.Fatal("remote local listener accepted")
	}
	cfg = localConfig()
	cfg.ControllerAddress = "127.0.0.1:0"
	if _, err := Start(context.Background(), cfg, config.RoleWorker, Options{Network: Local}); err == nil {
		t.Fatal("zero target accepted")
	}
}

func TestLocalStillUsesControllerAdmission(t *testing.T) {
	controller := startLocalTest(t, config.RoleController, "", nil, localListener(t, "127.0.0.1:0"))
	release := make(chan struct{})
	rt := fakeruntime.New()
	rt.ReleaseGate = release
	agent := startLocalTest(t, config.RoleWorker, controller.ControllerAddress(), rt, localListener(t, "127.0.0.1:0"))
	waitAgent(t, agent, func(s AgentStatus) bool { return s.Membership.Registered })
	conn, err := grpc.NewClient(controller.ControllerAddress(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := meshv1.NewControllerServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		rows, err := client.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{})
		if err == nil && len(rows.Workers) == 1 && rows.Workers[0].State == meshv1.WorkerState_WORKER_STATE_READY {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("reverse probe never confirmed readiness")
		case <-ticker.C:
		}
	}
	first, err := client.RunInference(ctx, &meshv1.InferenceRequest{RequestId: "00000000-0000-4000-8000-000000000011", ModelId: localConfig().Model, Prompt: "hello", MaxOutputTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	started, err := first.Recv()
	if err != nil || started.GetStarted() == nil {
		t.Fatalf("Started=%v error=%v", started, err)
	}
	snapshot, _ := agent.AgentStatus()
	if started.GetStarted().WorkerId != snapshot.WorkerID {
		t.Fatal("controller selected a different identity")
	}
	second, err := client.RunInference(ctx, &meshv1.InferenceRequest{RequestId: "00000000-0000-4000-8000-000000000012", ModelId: localConfig().Model, Prompt: "hello", MaxOutputTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Recv(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("local capacity bypassed: %v", err)
	}
	close(release)
	if event, err := first.Recv(); err != nil || event.GetCompleted() == nil {
		t.Fatalf("completion=%v error=%v", event, err)
	}
	if _, err := first.Recv(); err != io.EOF {
		t.Fatalf("final=%v", err)
	}
	if rt.Counters().Starts != 1 || rt.Counters().Peak != 1 {
		t.Fatalf("runtime counts=%+v", rt.Counters())
	}
}

func TestResolverOverrideLeavesRuntimeIndependent(t *testing.T) {
	cfg := localConfig()
	entered := make(chan struct{})
	options := Options{ResolveController: func(ctx context.Context, _ string) (string, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 4*time.Second {
			t.Error("resolver missing lookup budget")
		}
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}}
	effects := testDiscovery()
	effects.resolve = func(context.Context, string) (string, error) {
		t.Error("default resolver used with override")
		return "", errors.New("unexpected")
	}
	wl := localListener(t, "127.0.0.1:0")
	rt := fakeruntime.New()
	p, err := startWithDiscovery(context.Background(), cfg, config.RoleWorker, rt, nil, wl, options, effects)
	if err != nil {
		wl.Close()
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("membership did not own resolver")
	}
	waitAgent(t, p, func(s AgentStatus) bool {
		return s.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY && !s.Membership.Registered
	})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if rt.Counters().Starts != 1 || rt.Counters().Stops != 1 {
		t.Fatalf("runtime not owned independently: %+v", rt.Counters())
	}
}

func TestProcessPreservesRuntimeNetworkCleanupError(t *testing.T) {
	release := make(chan struct{})
	close(release)
	rt := &stopRuntime{Runtime: fakeruntime.New(), entered: make(chan struct{}), release: release, stopErr: net.ErrClosed}
	cfg := localConfig()
	cfg.ControllerAddress = "127.0.0.1:1"
	p, err := startWithDiscovery(context.Background(), cfg, config.RoleWorker, rt, nil, localListener(t, "127.0.0.1:0"), Options{Network: Local}, testDiscovery())
	if err != nil {
		t.Fatal(err)
	}
	waitAgent(t, p, func(s AgentStatus) bool { return s.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY })
	if err := p.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("runtime cleanup error suppressed: %v", err)
	}
}

type fatalAcceptListener struct {
	*closeErrorListener
	acceptErr error
}

func (l *fatalAcceptListener) Accept() (net.Conn, error) { return nil, l.acceptErr }

func TestProcessFatalAcceptPreservesFirstCloseFailure(t *testing.T) {
	for name, role := range map[string]config.Role{"Controller": config.RoleController, "Agent": config.RoleWorker} {
		t.Run(name, func(t *testing.T) {
			acceptErr := errors.New("fatal Accept failure")
			closeErr := errors.New("first Close failure")
			listener := &fatalAcceptListener{
				closeErrorListener: &closeErrorListener{Listener: localListener(t, "127.0.0.1:0"), closeErr: closeErr},
				acceptErr:          acceptErr,
			}
			cfg := localConfig()
			var cl, wl net.Listener
			if role == config.RoleController {
				cl = listener
			} else {
				wl = listener
				cfg.ControllerAddress = "127.0.0.1:1"
			}
			p, err := startWithDiscovery(context.Background(), cfg, role, fakeruntime.New(), cl, wl, Options{Network: Local}, testDiscovery())
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- p.Wait() }()
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("fatal Accept did not stop and join process owners")
			}
			if !errors.Is(err, acceptErr) || !errors.Is(err, closeErr) {
				t.Errorf("Wait lost primary or cleanup failure: %v", err)
			}
			closed := p.Close()
			if !errors.Is(closed, acceptErr) || !errors.Is(closed, closeErr) {
				t.Errorf("Close lost primary or cleanup failure: %v", closed)
			}
			if closed != err {
				t.Error("Wait and Close observed different final results")
			}
		})
	}
}
