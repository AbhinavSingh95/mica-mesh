package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fileConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestFlagOverridesConfig(t *testing.T) {
	p := fileConfig(t, `{"max_output_tokens":0,"timeout":"0s"}`)
	c, err := parse([]string{"run", "--config", p, "--max-output-tokens", "7", "--timeout", "1s", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.MaxOutputTokens != 7 || c.cfg.Timeout != time.Second {
		t.Fatalf("config=%+v", c.cfg)
	}
}
func TestExplicitZeroIsRejected(t *testing.T) {
	_, err := parse([]string{"run", "--config", fileConfig(t, `{}`), "--max-output-tokens", "0", "hello"})
	if err == nil {
		t.Fatal("explicit zero accepted")
	}
}
func TestControllerOnlyNeedsNoRuntimePaths(t *testing.T) {
	c, err := parse([]string{"start", "--controller", "--config", fileConfig(t, `{}`)})
	if err != nil || c.roles != config.RoleController {
		t.Fatalf("command=%+v error=%v", c, err)
	}
}
func TestCombinedRoleRejectsRemoteController(t *testing.T) {
	_, err := parse([]string{"start", "--controller", "--worker", "--controller-address", "127.0.0.1:1234", "--config", fileConfig(t, `{}`)})
	if err == nil {
		t.Fatal("conflicting address accepted")
	}
}
func TestStrictArguments(t *testing.T) {
	for _, args := range [][]string{{"start"}, {"status", "extra"}, {"run"}, {"status", "--worker"}, {"start", "--controller=false"}, {"run", "a", "b"}} {
		if _, err := parse(append(args, "--config", fileConfig(t, `{}`))); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
func TestRunBudgetIncludesResolution(t *testing.T) {
	cfg := config.Default()
	cfg.Timeout = 20 * time.Millisecond
	var out, diag bytes.Buffer
	err := runInference(context.Background(), cfg, "hello", &out, &diag, func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

type streamController struct {
	meshv1.UnimplementedControllerServiceServer
	events   func(*meshv1.InferenceRequest) []*meshv1.InferenceEvent
	final    error
	canceled chan struct{}
	wait     bool
	cluster  *meshv1.GetClusterStatusResponse
}

func (s *streamController) RunInference(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	for _, e := range s.events(req) {
		if err := out.Send(e); err != nil {
			return err
		}
	}
	if s.wait {
		<-out.Context().Done()
		close(s.canceled)
		return status.FromContextError(out.Context().Err()).Err()
	}
	return s.final
}
func started(req *meshv1.InferenceRequest) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: req.RequestId, WorkerId: uuid.NewString(), WorkerHostname: "test-host", ModelId: req.ModelId}}}
}
func delta(s string) *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: s}}}
}
func completed() *meshv1.InferenceEvent {
	return &meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: &meshv1.Completed{FinishReason: "stop"}}}
}
func serve(t *testing.T, s meshv1.ControllerServiceServer) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer(protocol.GenerationServerOption())
	meshv1.RegisterControllerServiceServer(g, s)
	done := make(chan struct{})
	go func() { defer close(done); _ = g.Serve(l) }()
	t.Cleanup(func() { g.Stop(); <-done })
	return l.Addr().String()
}
func invoke(t *testing.T, s *streamController) (int, string, string) {
	t.Helper()
	var out, diag bytes.Buffer
	code := Main(context.Background(), []string{"run", "--config", fileConfig(t, `{}`), "--controller-address", serve(t, s), "hello"}, nil, &out, &diag)
	return code, out.String(), diag.String()
}
func TestRunSeparatesTextAndDiagnostics(t *testing.T) {
	code, out, diag := invoke(t, &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
		return []*meshv1.InferenceEvent{started(req), delta("hello"), completed()}
	}})
	if code != 0 || out != "hello" || !strings.Contains(diag, "test-host") || !strings.Contains(diag, "finish_reason=stop") || !strings.Contains(diag, "time_to_first_text=") || strings.Contains(diag, "time_to_first_text=unknown") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, diag)
	}
}
func TestRunTimingIncludesResolution(t *testing.T) {
	address := serve(t, &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
		return []*meshv1.InferenceEvent{started(req), delta("text"), completed()}
	}})
	cfg := config.Default()
	cfg.Timeout = time.Second
	var out, diag bytes.Buffer
	var resolutionTime time.Duration
	err := runInference(context.Background(), cfg, "hello", &out, &diag, func(ctx context.Context) (string, error) {
		begin := time.Now()
		// Create a measured discovery cost, rather than timing scheduler work.
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
		resolutionTime = time.Since(begin)
		return address, nil
	})
	if err != nil || out.String() != "text" {
		t.Fatalf("error=%v text=%q", err, out.String())
	}
	for _, key := range []string{"duration=", "time_to_first_text="} {
		found := false
		for _, field := range strings.Fields(diag.String()) {
			if value, ok := strings.CutPrefix(field, key); ok {
				elapsed, err := time.ParseDuration(value)
				if err != nil || elapsed < resolutionTime {
					t.Fatalf("%s%s omits discovery cost %s", key, value, resolutionTime)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %q", key, diag.String())
		}
	}
}

func TestPartialTextSurvivesFailure(t *testing.T) {
	code, out, diag := invoke(t, &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
		return []*meshv1.InferenceEvent{started(req), delta("partial")}
	}, final: status.Error(codes.Unavailable, "failed")})
	if code == 0 || out != "partial" || !strings.Contains(diag, "time_to_first_text=") || strings.Contains(diag, "time_to_first_text=unknown") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}
func TestCompletedRequiresFinalOK(t *testing.T) {
	code, _, diag := invoke(t, &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
		return []*meshv1.InferenceEvent{started(req), completed()}
	}, final: status.Error(codes.Unavailable, "failed")})
	if code == 0 || !strings.Contains(diag, "time_to_first_text=unknown") || strings.Contains(diag, "finish_reason=") {
		t.Fatalf("non-OK completion result=%d diagnostic=%q", code, diag)
	}
}
func TestMalformedEventOrder(t *testing.T) {
	for _, name := range []string{"early-text", "early-complete", "absent", "duplicate-start", "late-text", "missing-complete", "oversized", "invalid-utf8", "wrong-request", "wrong-model", "bad-worker", "bad-reason", "negative-usage"} {
		t.Run(name, func(t *testing.T) {
			code, _, _ := invoke(t, &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
				first := started(req)
				last := completed()
				switch name {
				case "early-text":
					return []*meshv1.InferenceEvent{delta("early")}
				case "early-complete":
					return []*meshv1.InferenceEvent{last}
				case "absent":
					return []*meshv1.InferenceEvent{{}}
				case "duplicate-start":
					return []*meshv1.InferenceEvent{first, first, last}
				case "late-text":
					return []*meshv1.InferenceEvent{first, last, delta("late")}
				case "missing-complete":
					return []*meshv1.InferenceEvent{first}
				case "oversized":
					return []*meshv1.InferenceEvent{first, delta(strings.Repeat("x", 4097)), last}
				case "invalid-utf8":
					return []*meshv1.InferenceEvent{first, delta(string([]byte{0xff})), last}
				case "wrong-request":
					first.GetStarted().RequestId = uuid.NewString()
				case "wrong-model":
					first.GetStarted().ModelId = "other"
				case "bad-worker":
					first.GetStarted().WorkerId = "invalid"
				case "bad-reason":
					last.GetCompleted().FinishReason = "other"
				case "negative-usage":
					n := int64(-1)
					last.GetCompleted().OutputTokens = &n
				}
				return []*meshv1.InferenceEvent{first, last}
			}})
			if code == 0 {
				t.Fatal("malformed stream accepted")
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestWriteFailureCancelsRPC(t *testing.T) {
	s := &streamController{events: func(req *meshv1.InferenceRequest) []*meshv1.InferenceEvent {
		return []*meshv1.InferenceEvent{started(req), delta("text")}
	}, wait: true, canceled: make(chan struct{})}
	cfg := config.Default()
	cfg.ControllerAddress = serve(t, s)
	cfg.Timeout = time.Second
	var diag bytes.Buffer
	if err := runInference(context.Background(), cfg, "hello", failingWriter{}, &diag, resolveController(cfg.ControllerAddress)); err == nil {
		t.Fatal("write failure succeeded")
	}
	select {
	case <-s.canceled:
	case <-time.After(time.Second):
		t.Fatal("RPC not canceled")
	}
}

func TestOldConfigPathPreserved(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "mica-mesh")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "config.json"), []byte(`{"max_output_tokens":9}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	c, err := parse([]string{"run", "hello"})
	if err != nil || c.cfg.MaxOutputTokens != 9 {
		t.Fatalf("config=%+v error=%v", c.cfg, err)
	}
}

func (s *streamController) GetClusterStatus(context.Context, *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	if s.cluster != nil {
		return s.cluster, nil
	}
	return &meshv1.GetClusterStatusResponse{ControllerId: uuid.NewString(), Workers: []*meshv1.WorkerInfo{{WorkerId: uuid.NewString(), State: meshv1.WorkerState(99), Hardware: &meshv1.HardwareInfo{Hostname: "unknown-hardware"}, Report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState(99)}}}}, nil
}
func TestStatusUnknownStateIsUnavailable(t *testing.T) {
	var out, diag bytes.Buffer
	code := Main(context.Background(), []string{"status", "--config", fileConfig(t, `{}`), "--controller-address", serve(t, &streamController{})}, nil, &out, &diag)
	if code != 0 || !strings.Contains(out.String(), "state=WORKER_STATE_UNAVAILABLE") {
		t.Fatalf("code=%d output=%q diagnostic=%q", code, out.String(), diag.String())
	}
}

func TestStatusHardwareAndActiveIdentity(t *testing.T) {
	cpu, gpu := "CPU\nquoted", "GPU\tquoted"
	cores := uint32(8)
	ram := uint64(16384)
	vram := uint64(8192)
	unified := true
	request := uuid.NewString()
	s := &streamController{cluster: &meshv1.GetClusterStatusResponse{ControllerId: uuid.NewString(), Workers: []*meshv1.WorkerInfo{{WorkerId: uuid.NewString(), State: meshv1.WorkerState_WORKER_STATE_BUSY, Hardware: &meshv1.HardwareInfo{Cpu: &cpu, CpuCores: &cores, RamBytes: &ram, Gpu: &gpu, GpuMemoryBytes: &vram, UnifiedMemory: &unified}, Report: &meshv1.WorkerReport{Active: true, ActiveRequestId: &request}}, {WorkerId: uuid.NewString(), Hardware: &meshv1.HardwareInfo{}, Report: &meshv1.WorkerReport{}}}}}
	var out, diag bytes.Buffer
	code := Main(context.Background(), []string{"status", "--config", fileConfig(t, `{}`), "--controller-address", serve(t, s)}, nil, &out, &diag)
	for _, want := range []string{`cpu="CPU\nquoted"`, `gpu="GPU\tquoted"`, "cpu_cores=8", "ram_bytes=16384", "dedicated_gpu_memory_bytes=8192", "unified_memory=true", "active_request_id=" + strconv.Quote(request), `cpu="unknown"`, "ram_bytes=unknown", "dedicated_gpu_memory_bytes=unknown", "unified_memory=unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q from %q", want, out.String())
		}
	}
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, diag.String())
	}
}
func TestNullDeviceBinding(t *testing.T) {
	f, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	release, err := bindOutput(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestNullFileNameDoesNotOverrideIdentity(t *testing.T) {
	zero, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer zero.Close()
	fd, err := syscall.Dup(int(zero.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	spoof := os.NewFile(uintptr(fd), "/dev/null")
	defer spoof.Close()
	if release, err := bindOutput(context.Background(), spoof); err == nil {
		release()
		t.Fatal("unsupported device accepted by caller-provided file name")
	}
}

type budgetController struct {
	meshv1.UnimplementedControllerServiceServer
	remaining chan time.Duration
}

func (s *budgetController) GetClusterStatus(ctx context.Context, _ *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("missing control deadline")
	}
	s.remaining <- time.Until(deadline)
	return &meshv1.GetClusterStatusResponse{ControllerId: uuid.NewString()}, nil
}
func TestStatusHasRPCBudgetAfterDiscovery(t *testing.T) {
	server := &budgetController{remaining: make(chan time.Duration, 1)}
	address := serve(t, server)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := clusterStatusWithResolver(ctx, &out, func(ctx context.Context) (string, error) {
		browse, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		<-browse.Done()
		return address, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if remaining := <-server.remaining; remaining < time.Second || remaining > 2*time.Second {
		t.Fatalf("RPC remaining=%v", remaining)
	}
}
func TestStatusPreservesEarlierCallerDeadline(t *testing.T) {
	server := &budgetController{remaining: make(chan time.Duration, 1)}
	address := serve(t, server)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := clusterStatusWithResolver(ctx, &out, func(context.Context) (string, error) { return address, nil }); err != nil {
		t.Fatal(err)
	}
	if remaining := <-server.remaining; remaining > 500*time.Millisecond {
		t.Fatalf("extended caller deadline: %v", remaining)
	}
}

func TestControllerNeedsNoManagedFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{{"start", "--controller"}, {"run", "hello"}, {"status"}, {"start", "--controller=false", "--controller"}} {
		c, err := parse(args)
		if err != nil || c.cfg.RuntimeBinary != "" || c.cfg.ModelPath != "" {
			t.Fatalf("command = %+v, %v", c, err)
		}
	}
}

func TestVisitedAssetFlagsPreservePresence(t *testing.T) {
	for _, test := range []struct {
		flag string
		want config.AssetFields
	}{
		{"--runtime-binary", config.AssetFields{RuntimeBinary: true}},
		{"--model-path", config.AssetFields{ModelPath: true}},
	} {
		c, err := parse([]string{"start", "--worker", "--config", fileConfig(t, `{}`), test.flag, ""})
		if err != nil {
			t.Fatal(err)
		}
		if c.assets != test.want {
			t.Fatalf("%s presence = %+v", test.flag, c.assets)
		}
		_, err = resolveWorkerConfig(context.Background(), c)
		var unavailable *setup.ErrNotPrepared
		if err == nil || errors.As(err, &unavailable) {
			t.Fatalf("empty flag error = %v", err)
		}
	}
	c, err := parse([]string{"start", "--worker", "--config", fileConfig(t, `{"backend":"metal","runtime_binary":"","model_path":""}`), "--backend", "cpu", "--runtime-binary", "/manual/server", "--model-path", "/manual/model"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveWorkerConfig(context.Background(), c)
	if err != nil || c.assets != (config.AssetFields{RuntimeBinary: true, ModelPath: true, Backend: true}) || cfg.Backend != "cpu" || cfg.RuntimeBinary != "/manual/server" || cfg.ModelPath != "/manual/model" {
		t.Fatalf("flag overrides = %+v, %v", c, err)
	}
}

func TestWorkerMissingManagedFilesIsNotPrepared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := parse([]string{"start", "--worker"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveWorkerConfig(context.Background(), c)
	var unavailable *setup.ErrNotPrepared
	if !errors.As(err, &unavailable) {
		t.Fatalf("missing managed files error = %v", err)
	}
}

func TestManagedLayoutFollowsExecutableLink(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bundle", "bin", "mica-mesh")
	if err := os.MkdirAll(filepath.Dir(executable), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "mica-mesh")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	layout, err := managedLayout(link, root)
	wantRoot, wantErr := filepath.EvalSymlinks(filepath.Join(root, "bundle"))
	if wantErr != nil {
		t.Fatal(wantErr)
	}
	if err != nil || layout.ReleaseRoot != wantRoot || layout.DataRoot != filepath.Join(root, "Library", "Application Support", "Mica Mesh") {
		t.Fatalf("layout = %+v, %v", layout, err)
	}
	if _, err := managedLayout(filepath.Join(root, "missing"), root); err == nil {
		t.Fatal("accepted missing executable")
	}
}

func TestRoleCommandsMapToOneRole(t *testing.T) {
	for name, want := range map[string]config.Role{"controller": config.RoleController, "agent": config.RoleWorker} {
		c, err := parse([]string{name, "--config", fileConfig(t, `{}`), "--plain"})
		if err != nil || c.roles != want {
			t.Errorf("%s: role=%v error=%v", name, c.roles, err)
		}
	}
}
func TestControllerNeedsNoAssets(t *testing.T) {
	c, err := parse([]string{"controller", "--config", fileConfig(t, `{}`)})
	if err != nil || c.roles != config.RoleController {
		t.Fatalf("role=%v error=%v", c.roles, err)
	}
}
func TestLocalDefaultsUseDistinctFixedPorts(t *testing.T) {
	for _, role := range []string{"controller", "agent"} {
		c, err := parse([]string{role, "--local", "--config", fileConfig(t, `{}`)})
		if err != nil {
			t.Fatal(err)
		}
		target := ""
		if role == "agent" {
			target = "127.0.0.1:50051"
		}
		if c.cfg.ControllerListen != "127.0.0.1:50051" || c.cfg.WorkerListen != "127.0.0.1:50052" || c.cfg.ControllerAddress != target || c.cfg.RuntimePort != 8080 {
			t.Fatalf("%s config=%+v", role, c.cfg)
		}
	}
}
func TestLocalIgnoresStoredRemoteTarget(t *testing.T) {
	for _, role := range []string{"controller", "agent"} {
		c, err := parse([]string{role, "--local", "--config", fileConfig(t, `{"controller_address":"192.0.2.2:8000","controller_listen":"192.0.2.3:8001","worker_listen":"192.0.2.3:8002","advertise_address":"192.0.2.3","runtime_port":8081}`), "--worker-listen", "127.0.0.1:0", "--controller-listen", "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		if c.cfg.WorkerListen != "127.0.0.1:0" || c.cfg.ControllerListen != "127.0.0.1:0" || c.cfg.RuntimePort != 8081 || strings.Contains(c.cfg.ControllerAddress, "192.") || strings.Contains(c.cfg.AdvertiseAddress, "192.") {
			t.Fatalf("config=%+v", c.cfg)
		}
	}
}
func TestLocalRejectsExplicitNonLoopbackFlags(t *testing.T) {
	for _, flag := range []string{"controller-address", "controller-listen", "worker-listen", "advertise-address"} {
		value := "192.0.2.1:50051"
		if flag == "advertise-address" {
			value = "192.0.2.1"
		}
		if _, err := parse([]string{"agent", "--local", "--config", fileConfig(t, `{}`), "--" + flag, value}); err == nil {
			t.Errorf("accepted %s", flag)
		}
	}
	for _, args := range [][]string{{"controller", "--controller-address", ""}, {"agent", "--controller-address", ""}, {"agent", "--controller-address", "127.0.0.1:0"}, {"agent", "--worker-listen", "0.0.0.0:0"}} {
		if _, err := parse(append(args, "--local", "--config", fileConfig(t, `{}`))); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}
func TestRoleHelpShowsAgentTerminology(t *testing.T) {
	for _, role := range []string{"controller", "agent"} {
		help := commandHelp(role)
		if !strings.Contains(help, "--local") || !strings.Contains(help, "Agent") || strings.Contains(help, "Worker listener") {
			t.Errorf("%s help=%s", role, help)
		}
	}
}

type forbiddenRoleInput struct{ t *testing.T }

func (r forbiddenRoleInput) Read([]byte) (int, error) {
	r.t.Error("plain role read consent input")
	return 0, io.EOF
}
func TestPlainRolesNeverPromptOrDownload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, role := range []string{"controller", "agent"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var out, diag bytes.Buffer
		args := []string{role, "--plain", "--local", "--config", fileConfig(t, `{}`), "--controller-listen", "127.0.0.1:0", "--worker-listen", "127.0.0.1:0"}
		code := Main(ctx, args, forbiddenRoleInput{t}, &out, &diag)
		if role == "controller" && code != 0 {
			t.Fatalf("Controller requires assets: %s", diag.String())
		}
		if role == "agent" && (code == 0 || !strings.Contains(diag.String(), "setup")) {
			t.Fatalf("unprepared Agent result=%d diagnostics=%s", code, diag.String())
		}
		if out.Len() != 0 || strings.Contains(diag.String(), "[y/N]") || strings.Contains(diag.String(), "Downloading") {
			t.Fatalf("plain role prompted/downloaded: %s %s", out.String(), diag.String())
		}
	}
}
