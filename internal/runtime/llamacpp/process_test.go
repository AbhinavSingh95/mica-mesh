package llamacpp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

type launch struct {
	Args map[string]string
	Env  []string
	PID  int
}
type fixture struct {
	cfg                        mesh.Config
	launched                   chan launch
	healthCalled, warmupCalled chan struct{}
	processing                 atomic.Bool
	warmupBusy                 bool
	propsOverride              string
	loading                    atomic.Bool
	warmupGate                 chan struct{}
	warmupStatus               int
	server                     *httptest.Server
	generateHandler            func(http.ResponseWriter, *http.Request)
	tokenHandler               func(http.ResponseWriter, *http.Request)
	templateHandler            func(http.ResponseWriter, *http.Request)
	slotsHandler               func(http.ResponseWriter, *http.Request) bool
	tokenCount                 int
	completionCalls            atomic.Int32
	terminated                 chan struct{}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	// Both target Macs use Bazel's runfiles directory. The executable path
	// comes from a declared data dependency, never PATH or a host installation.
	path := filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("MICA_TEST_SERVER"))
	dir := t.TempDir()
	binary := filepath.Join(dir, "runtime with spaces")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(binary, b, 0700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "tiny model.gguf")
	b = []byte("tiny fake gguf artifact")
	if err = os.WriteFile(model, b, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	f := &fixture{cfg: mesh.Config{BinaryPath: binary, ModelPath: model, Backend: "cpu", Port: port, Model: mesh.Model{ID: "test-model", SHA256: hex.EncodeToString(sum[:]), ContextTokens: 2048}}, launched: make(chan launch, 1), healthCalled: make(chan struct{}, 32), warmupCalled: make(chan struct{}, 1), warmupStatus: 200, tokenCount: 10}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/termination":
			if f.terminated != nil {
				select {
				case f.terminated <- struct{}{}:
				default:
				}
			}
		case "/launch":
			var l launch
			if err := json.NewDecoder(req.Body).Decode(&l); err != nil {
				t.Error(err)
			}
			f.launched <- l
		case "/health":
			select {
			case f.healthCalled <- struct{}{}:
			default:
			}
			if f.loading.Load() {
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"message":"Loading model"}}`)
			} else {
				io.WriteString(w, `{"status":"ok"}`)
			}
		case "/apply-template":
			if f.templateHandler != nil {
				f.templateHandler(w, req)
				return
			}
			io.WriteString(w, `{"prompt":"formatted conversation"}`)
		case "/tokenize":
			if f.tokenHandler != nil {
				f.tokenHandler(w, req)
				return
			}
			var body struct {
				Content      string
				AddSpecial   bool `json:"add_special"`
				ParseSpecial bool `json:"parse_special"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Content != "formatted conversation" || !body.AddSpecial || !body.ParseSpecial {
				t.Errorf("tokenization body = %+v", body)
			}
			tokens := make([]int, f.tokenCount)
			_ = json.NewEncoder(w).Encode(struct {
				Tokens []int `json:"tokens"`
			}{tokens})
		case "/slots":
			if f.slotsHandler != nil && f.slotsHandler(w, req) {
				return
			}
			fmt.Fprintf(w, `[{"id":0,"n_ctx":%d,"is_processing":%t}]`, f.cfg.Model.ContextTokens, f.processing.Load())
		case "/props":
			if f.propsOverride != "" {
				io.WriteString(w, f.propsOverride)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model_path": f.cfg.ModelPath, "total_slots": 1, "build_info": "version: 0.5.0 (build 1, commit 7fe450e)", "is_sleeping": false})
		case "/completion":
			payload, _ := io.ReadAll(req.Body)
			var body struct {
				Prompt   string
				NPredict int `json:"n_predict"`
				Stream   bool
			}
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Error(err)
			}
			if body.Stream {
				req.Body = io.NopCloser(strings.NewReader(string(payload)))
				f.completionCalls.Add(1)
				if f.generateHandler != nil {
					f.generateHandler(w, req)
				} else {
					io.WriteString(w, "data: {\"content\":\"Hi\",\"stop\":false}\n\ndata: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\"}\n\n")
				}
				return
			}
			if body.Prompt == "" || body.NPredict != 1 {
				t.Errorf("wrong warmup: %+v", body)
			}
			select {
			case f.warmupCalled <- struct{}{}:
			default:
			}
			if f.warmupGate != nil {
				select {
				case <-f.warmupGate:
				case <-req.Context().Done():
					return
				}
			}
			if f.warmupBusy {
				f.processing.Store(true)
			}
			w.WriteHeader(f.warmupStatus)
			io.WriteString(w, `{"content":"Hi","stop":true,"tokens_predicted":1}`)
		default:
			http.NotFound(w, req)
		}
	}))
	// Keep race instrumentation; remove its artificial one-second exit delay
	// only in child processes so the version check has the same timing contract.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	t.Setenv("MICA_TEST_CONTROL", f.server.URL)
	t.Cleanup(f.server.Close)
	return f
}
func bound(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for test coordination")
		var zero T
		return zero
	}
}
func running(t *testing.T, f *fixture) *Runtime {
	t.Helper()
	r := New()
	t.Cleanup(func() {
		if err := r.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err := r.Start(bound(t), f.cfg); err != nil {
		t.Fatal(err)
	}
	return r
}
func ready(t *testing.T, r *Runtime) {
	t.Helper()
	h, err := r.Health(bound(t))
	if err != nil || h.State != mesh.StateReady || h.Active {
		t.Fatalf("health = %+v, %v; want ready and idle", h, err)
	}
}
func TestStartChecksDigest(t *testing.T) {
	f := setup(t)
	f.cfg.Model.SHA256 = strings.Repeat("0", 64)
	r := New()
	err := r.Start(bound(t), f.cfg)
	if !errors.Is(err, mesh.ErrInvalidInput) {
		t.Fatalf("Start = %v; want invalid artifact", err)
	}
	select {
	case <-f.launched:
		t.Fatal("launched child with unverified model")
	default:
	}
}
func TestLoadingIsNotReady(t *testing.T) {
	f := setup(t)
	f.loading.Store(true)
	f.warmupGate = make(chan struct{})
	defer close(f.warmupGate)
	r := New()
	defer r.Stop(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(bound(t), f.cfg) }()
	select {
	case err := <-done:
		t.Fatalf("Start returned before readiness: %v", err)
	case <-f.healthCalled:
	case <-time.After(10 * time.Second):
		t.Fatal("health not polled")
	}
	h, _ := r.Health(bound(t))
	if h.State == mesh.StateReady {
		t.Fatal("loading reported ready")
	}
	f.loading.Store(false)
	receive(t, f.warmupCalled)
	h, _ = r.Health(bound(t))
	if h.State == mesh.StateReady {
		t.Fatal("reported ready before warmup finished")
	}
	f.warmupGate <- struct{}{}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	ready(t, r)
}
func TestStartupTimeoutReapsChild(t *testing.T) {
	f := setup(t)
	f.loading.Store(true)
	r := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := r.Start(ctx, f.cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start = %v; want deadline", err)
	}
	l := receive(t, f.launched)
	assertExited(t, l.PID)
	h, _ := r.Health(bound(t))
	if h.State != mesh.StateUnhealthy {
		t.Fatalf("timeout health = %+v", h)
	}
}
func assertExited(t *testing.T, pid int) {
	t.Helper()
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if err = p.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("owned child %d still alive or unreaped", pid)
	}
}
func TestPortConflictPreservesExistingServer(t *testing.T) {
	f := setup(t)
	existing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, "existing") }))
	defer existing.Close()
	f.cfg.Port = existing.Listener.Addr().(*net.TCPAddr).Port
	r := New()
	err := r.Start(bound(t), f.cfg)
	if err == nil {
		t.Fatal("Start accepted an occupied port")
	}
	resp, err := http.Get(existing.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "existing" {
		t.Fatalf("existing server changed: %s", b)
	}
}
func TestPathsWithSpaces(t *testing.T) {
	f := setup(t)
	t.Setenv("LLAMA_ARG_HOST", "0.0.0.0")
	t.Setenv("LLAMA_ARG_PARALLEL", "9")
	r := running(t, f)
	ready(t, r)
	l := receive(t, f.launched)
	for k, v := range map[string]string{"--model": f.cfg.ModelPath, "--host": "127.0.0.1", "--parallel": "1", "--ctx-size": "2048", "--fit": "off", "--sleep-idle-seconds": "-1", "--device": "none", "--gpu-layers": "0", "--offline": "true", "--no-context-shift": "true", "--slots": "true", "--jinja": "true", "--warmup": "true", "--no-ui": "true"} {
		if l.Args[k] != v {
			t.Errorf("%s=%q; want %q", k, l.Args[k], v)
		}
	}
	for _, e := range l.Env {
		if strings.HasPrefix(e, "LLAMA_ARG_") {
			t.Errorf("runtime override inherited: %s", e)
		}
	}
	c := r.Capabilities()
	if c.Model != f.cfg.Model || c.Capacity != 1 || c.RuntimeVersion != "0.5.0" || c.Backend != "cpu" {
		t.Fatalf("capabilities = %+v", c)
	}
}
func TestNoisyStderrCannotBlockStart(t *testing.T) {
	f := setup(t)
	t.Setenv("MICA_TEST_NOISE", "1")
	r := running(t, f)
	ready(t, r)
	l := receive(t, f.launched)
	if err := r.Stop(bound(t)); err != nil {
		t.Fatal(err)
	}
	assertExited(t, l.PID)
	h, _ := r.Health(bound(t))
	if len(h.LastError) > 66000 {
		t.Fatalf("diagnostics retained %d bytes", len(h.LastError))
	}
}
func TestStopAfterPartialStart(t *testing.T) {
	f := setup(t)
	f.warmupStatus = 500
	r := New()
	err := r.Start(bound(t), f.cfg)
	if err == nil {
		t.Fatal("failed warmup accepted")
	}
	l := receive(t, f.launched)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err = r.Stop(bound(t)); err != nil {
		t.Fatal(err)
	}
	assertExited(t, l.PID)
	h, _ := r.Health(bound(t))
	if h.State != mesh.StateUnhealthy {
		t.Fatalf("failed startup health = %+v", h)
	}
}
func TestWrongVersionRejected(t *testing.T) {
	f := setup(t)
	t.Setenv("MICA_TEST_VERSION", "version: 0.6.0 (build 1, commit abc)")
	err := New().Start(bound(t), f.cfg)
	if !errors.Is(err, mesh.ErrInvalidInput) {
		t.Fatalf("wrong version accepted: %v", err)
	}
}
func TestStartedChildOutlivesStartupContext(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(bound(t))
	r := New()
	defer r.Stop(context.Background())
	if err := r.Start(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	cancel()
	ready(t, r)
}

func TestRuntimeCrashMarksUnhealthyAndReaps(t *testing.T) {
	f := setup(t)
	r := running(t, f)
	l := receive(t, f.launched)
	req, err := http.NewRequestWithContext(bound(t), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/crash", f.cfg.Port), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := http.DefaultClient.Do(req)
	if response != nil {
		response.Body.Close()
	}
	r.mu.Lock()
	done := r.child.done
	r.mu.Unlock()
	receive(t, done)
	assertExited(t, l.PID)
	h, _ := r.Health(bound(t))
	if h.State != mesh.StateUnhealthy || h.LastError == "" {
		t.Fatalf("crash health = %+v", h)
	}
}
func TestRepeatedStartDoesNotOrphanChild(t *testing.T) {
	f := setup(t)
	r := running(t, f)
	l := receive(t, f.launched)
	if err := r.Start(bound(t), f.cfg); !errors.Is(err, mesh.ErrUnavailable) {
		t.Fatalf("second Start = %v", err)
	}
	ready(t, r)
	r.Stop(bound(t))
	assertExited(t, l.PID)
	if err := r.Start(bound(t), f.cfg); err != nil {
		t.Fatalf("Start after Stop = %v", err)
	}
	ready(t, r)
}
func TestMetalSelectsExplicitDevice(t *testing.T) {
	f := setup(t)
	f.cfg.Backend = "metal"
	r := running(t, f)
	ready(t, r)
	l := receive(t, f.launched)
	if l.Args["--device"] != "MTL0" || l.Args["--gpu-layers"] != "all" {
		t.Fatalf("metal configuration = %+v", l.Args)
	}
}
func TestHealthReportsLiveSlotActivity(t *testing.T) {
	f := setup(t)
	r := running(t, f)
	f.processing.Store(true)
	h, err := r.Health(bound(t))
	if err != nil || h.State != mesh.StateReady || !h.Active {
		t.Fatalf("busy health = %+v, %v", h, err)
	}
	f.processing.Store(false)
	ready(t, r)
}
func TestOversizedPropertiesFailStartup(t *testing.T) {
	f := setup(t)
	f.propsOverride = strings.Repeat("x", 65537)
	r := New()
	defer r.Stop(context.Background())
	if err := r.Start(bound(t), f.cfg); !errors.Is(err, mesh.ErrMalformedResponse) {
		t.Fatalf("oversized properties = %v", err)
	}
	l := receive(t, f.launched)
	assertExited(t, l.PID)
}
func TestCanceledStartDoesNotLaunch(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := New()
	if err := r.Start(ctx, f.cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v", err)
	}
	select {
	case <-f.launched:
		t.Fatal("launched after canceled startup")
	default:
	}
}
func TestInvalidConfigurationNeverLaunches(t *testing.T) {
	f := setup(t)
	original := f.cfg
	for name, mutate := range map[string]func(*mesh.Config){"relative binary": func(c *mesh.Config) { c.BinaryPath = "relative" }, "relative model": func(c *mesh.Config) { c.ModelPath = "relative" }, "backend": func(c *mesh.Config) { c.Backend = "auto" }, "port": func(c *mesh.Config) { c.Port = 0 }, "digest": func(c *mesh.Config) { c.Model.SHA256 = "xx" }, "context": func(c *mesh.Config) { c.Model.ContextTokens = 0 }} {
		t.Run(name, func(t *testing.T) {
			cfg := original
			mutate(&cfg)
			r := New()
			defer r.Stop(context.Background())
			err := r.Start(bound(t), cfg)
			if !errors.Is(err, mesh.ErrInvalidInput) {
				t.Fatalf("invalid config = %v", err)
			}
		})
	}
	select {
	case <-f.launched:
		t.Fatal("invalid config launched")
	default:
	}
}
func TestConfiguredContextIsAuthoritative(t *testing.T) {
	f := setup(t)
	f.cfg.Model.ContextTokens = 4096
	r := running(t, f)
	ready(t, r)
	l := receive(t, f.launched)
	if l.Args["--ctx-size"] != "4096" || r.Capabilities().Model.ContextTokens != 4096 {
		t.Fatalf("configured context changed: args=%+v capabilities=%+v", l.Args, r.Capabilities())
	}
}
func TestWarmupMustLeaveSlotIdle(t *testing.T) {
	f := setup(t)
	f.warmupBusy = true
	r := New()
	defer r.Stop(context.Background())
	err := r.Start(bound(t), f.cfg)
	if !errors.Is(err, mesh.ErrUnavailable) {
		t.Fatalf("busy warmup accepted: %v", err)
	}
	l := receive(t, f.launched)
	assertExited(t, l.PID)
}
func TestStopClearsActivityAfterReaping(t *testing.T) {
	f := setup(t)
	r := running(t, f)
	f.processing.Store(true)
	h, err := r.Health(bound(t))
	if err != nil || !h.Active {
		t.Fatalf("expected active slot: %+v %v", h, err)
	}
	if err := r.Stop(bound(t)); err != nil {
		t.Fatal(err)
	}
	h, err = r.Health(bound(t))
	if err != nil || h.Active || h.State != mesh.StateUnhealthy {
		t.Fatalf("stopped runtime activity = %+v %v", h, err)
	}
}

func TestNonregularModelRejectedBeforeOpen(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "short deadline"
		if canceled {
			name = "already canceled"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			fifo := filepath.Join(t.TempDir(), "model.fifo")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			f.cfg.ModelPath = fifo
			// This reader opens without waiting for a writer. It guarantees that the
			// rescue writer below can also open nonblocking, even before Start runs.
			reader, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Close(reader)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if canceled {
				cancel()
			}
			r := New()
			defer r.Stop(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx, f.cfg) }()
			timer := time.NewTimer(500 * time.Millisecond)
			defer timer.Stop()
			var startErr error
			select {
			case startErr = <-done:
			case <-timer.C:
				// Release a pre-fix blocking os.Open and join the owned Start goroutine
				// before reporting the failure. No writer was present before this bound.
				writer, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer syscall.Close(writer)
				startErr = receive(t, done)
				t.Error("Start blocked opening a FIFO beyond its canceled/deadline context")
			}
			if !errors.Is(startErr, mesh.ErrInvalidInput) {
				t.Errorf("nonregular model Start = %v; want invalid input", startErr)
			}
			select {
			case <-f.launched:
				t.Error("nonregular model launched a runtime child")
			default:
			}
		})
	}
}
