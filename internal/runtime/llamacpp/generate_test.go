package llamacpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func request() mesh.Request {
	return mesh.Request{ID: "request-id", ModelID: "test-model", Prompt: "Hello 🌏", MaxOutputTokens: 512}
}
func collect(r *Runtime, ctx context.Context, req mesh.Request) ([]mesh.Event, error) {
	var events []mesh.Event
	err := r.Generate(ctx, req, func(e mesh.Event) error { events = append(events, e); return nil })
	return events, err
}
func frame(w io.Writer, body string) { fmt.Fprintf(w, "data: %s\n\n", body) }
func TestGenerateEventOrder(t *testing.T) {
	f := setup(t)
	f.templateHandler = func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Messages            []struct{ Role, Content string }
			AddGenerationPrompt bool `json:"add_generation_prompt"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[0].Content != "You are a helpful assistant." || body.Messages[1].Role != "user" || body.Messages[1].Content != request().Prompt || !body.AddGenerationPrompt {
			t.Errorf("template body = %+v", body)
		}
		io.WriteString(w, `{"prompt":"formatted conversation"}`)
	}
	f.generateHandler = func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Prompt      string
			NPredict    int `json:"n_predict"`
			Temperature float64
			Stream      bool
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Prompt != "formatted conversation" || body.NPredict != 512 || body.Temperature != 0.7 || !body.Stream {
			t.Errorf("completion body = %+v", body)
		}
		frame(w, `{"content":"Hello ","stop":false}`)
		frame(w, `{"content":"🌏","stop":false}`)
		frame(w, `{"content":"!","stop":true,"stop_type":"eos","tokens_evaluated":10,"tokens_predicted":3}`)
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[0].Kind != mesh.EventStarted || events[1].Text != "Hello " || events[2].Text != "🌏" || events[3].Text != "!" || events[4].Kind != mesh.EventCompleted || events[4].FinishReason != "stop" || events[4].InputTokens == nil || *events[4].InputTokens != 10 || events[4].OutputTokens == nil || *events[4].OutputTokens != 3 {
		t.Fatalf("events = %+v", events)
	}
	ready(t, r)
}
func TestContextBudgetIncludesTemplateAndOutput(t *testing.T) {
	for _, n := range []int{1536, 1537} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := setup(t)
			f.tokenCount = n
			f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
				frame(w, `{"content":"","stop":true,"stop_type":"limit","tokens_evaluated":1536,"tokens_predicted":512,"truncated":true}`)
			}
			r := running(t, f)
			events, err := collect(r, bound(t), request())
			if n == 1536 {
				if err != nil || len(events) != 2 || events[1].FinishReason != "length" {
					t.Fatalf("exact budget = %+v %v", events, err)
				}
			} else if !errors.Is(err, mesh.ErrInvalidInput) || len(events) != 0 || f.completionCalls.Load() != 0 {
				t.Fatalf("overflow = %+v %v dispatches=%d", events, err, f.completionCalls.Load())
			}
		})
	}
}
func TestSSEFragmentedUTF8(t *testing.T) {
	f := setup(t)
	text := strings.Repeat("🌏éabc", 1000)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		b, _ := json.Marshal(struct {
			Content string `json:"content"`
			Stop    bool   `json:"stop"`
		}{text, false})
		payload := "data: " + string(b) + "\n\ndata: {\"content\":\"\",\"stop\":true,\"stop_type\":\"word\"}\n\n"
		for i := range len(payload) {
			io.WriteString(w, payload[i:i+1])
			w.(http.Flusher).Flush()
		}
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, e := range events {
		if e.Kind == mesh.EventTextDelta {
			if !utf8.ValidString(e.Text) || len(e.Text) > 4096 {
				t.Fatalf("invalid delta len=%d", len(e.Text))
			}
			got.WriteString(e.Text)
		}
	}
	if got.String() != text {
		t.Fatal("fragmented output changed")
	}
}
func assertNoCompleted(t *testing.T, events []mesh.Event) {
	t.Helper()
	for _, e := range events {
		if e.Kind == mesh.EventCompleted {
			t.Fatal("completed after detected error")
		}
	}
}
func TestOversizedEvent(t *testing.T) {
	f := setup(t)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		frame(w, `{"content":"`+strings.Repeat("x", 65536)+`","stop":false}`)
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, mesh.ErrMalformedResponse) {
		t.Fatalf("oversized event = %v", err)
	}
	assertNoCompleted(t, events)
}
func TestMissingTerminalEvent(t *testing.T) {
	f := setup(t)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) { frame(w, `{"content":"partial","stop":false}`) }
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, mesh.ErrMalformedResponse) || len(events) != 2 || events[1].Text != "partial" {
		t.Fatalf("missing terminal = %+v %v", events, err)
	}
	assertNoCompleted(t, events)
}
func TestCallbackFailureCancelsHTTP(t *testing.T) {
	f := setup(t)
	canceled := make(chan struct{})
	f.generateHandler = func(w http.ResponseWriter, req *http.Request) {
		f.processing.Store(true)
		frame(w, `{"content":"text","stop":false}`)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
		f.processing.Store(false)
		close(canceled)
	}
	r := running(t, f)
	failure := errors.New("downstream failed")
	err := r.Generate(bound(t), request(), func(e mesh.Event) error {
		if e.Kind == mesh.EventTextDelta {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("callback failure = %v", err)
	}
	receive(t, canceled)
	ready(t, r)
}
func TestCancelWaitsForIdle(t *testing.T) {
	f := setup(t)
	dispatched := make(chan struct{})
	canceled := make(chan struct{})
	cleanupProbe := make(chan struct{}, 1)
	idle := make(chan struct{})
	f.generateHandler = func(w http.ResponseWriter, req *http.Request) {
		f.processing.Store(true)
		frame(w, `{"content":"text","stop":false}`)
		w.(http.Flusher).Flush()
		close(dispatched)
		<-req.Context().Done()
		close(canceled)
	}
	f.slotsHandler = func(w http.ResponseWriter, req *http.Request) bool {
		if !f.processing.Load() {
			return false
		}
		select {
		case cleanupProbe <- struct{}{}:
		default:
		}
		select {
		case <-idle:
			f.processing.Store(false)
			io.WriteString(w, `[{"n_ctx":2048,"is_processing":false}]`)
		case <-req.Context().Done():
		}
		return true
	}
	r := running(t, f)
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Generate(ctx, request(), func(mesh.Event) error { return nil }) }()
	select {
	case <-dispatched:
	case err := <-done:
		t.Fatalf("Generate returned before dispatch: %v", err)
	case <-bound(t).Done():
		t.Fatal("no dispatch")
	}
	cancel()
	receive(t, canceled)
	receive(t, cleanupProbe)
	r.mu.Lock()
	h := r.health
	r.mu.Unlock()
	if !h.Active {
		t.Error("released activity before idle")
	}
	select {
	case err := <-done:
		t.Fatalf("returned before idle: %v", err)
	default:
	}
	close(idle)
	err := receive(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	ready(t, r)
}
func TestCancelKillsUnresponsiveRuntime(t *testing.T) {
	f := setup(t)
	t.Setenv("MICA_TEST_IGNORE_TERM", "1")
	f.terminated = make(chan struct{}, 1)
	dispatched := make(chan struct{})
	canceled := make(chan struct{})
	probe := make(chan struct{}, 1)
	f.generateHandler = func(w http.ResponseWriter, req *http.Request) {
		f.processing.Store(true)
		frame(w, `{"content":"text","stop":false}`)
		w.(http.Flusher).Flush()
		close(dispatched)
		<-req.Context().Done()
		close(canceled)
	}
	f.slotsHandler = func(w http.ResponseWriter, req *http.Request) bool {
		if !f.processing.Load() {
			return false
		}
		select {
		case probe <- struct{}{}:
		default:
		}
		<-req.Context().Done()
		return true
	}
	r := running(t, f)
	l := receive(t, f.launched)
	type phase struct {
		duration time.Duration
		cancel   context.CancelFunc
	}
	phases := make(chan phase, 3)
	r.cleanupTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		phases <- phase{d, cancel}
		return ctx, cancel
	}
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Generate(ctx, request(), func(mesh.Event) error { return nil }) }()
	select {
	case <-dispatched:
	case err := <-done:
		t.Fatalf("Generate returned before dispatch: %v", err)
	case <-bound(t).Done():
		t.Fatal("no dispatch")
	}
	cancel()
	receive(t, canceled)
	idlePhase := receive(t, phases)
	defer idlePhase.cancel()
	if idlePhase.duration != 2*time.Second {
		t.Errorf("idle budget = %s", idlePhase.duration)
	}
	receive(t, probe)
	idlePhase.cancel()
	termination := receive(t, phases)
	defer termination.cancel()
	if termination.duration != 3*time.Second {
		t.Errorf("termination/reap budget = %s", termination.duration)
	}
	grace := receive(t, phases)
	defer grace.cancel()
	if grace.duration > termination.duration {
		t.Error("graceful phase exceeds termination budget")
	}
	receive(t, f.terminated)
	h, err := r.Health(bound(t))
	if err != nil || h.State != mesh.StateUnhealthy || !h.Active {
		t.Errorf("cleanup health = %+v %v", h, err)
	}
	if err := r.Generate(bound(t), request(), func(mesh.Event) error { return nil }); !errors.Is(err, mesh.ErrUnavailable) {
		t.Errorf("overlap during cleanup = %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("returned while child ignores termination: %v", err)
	default:
	}
	grace.cancel()
	err = receive(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel identity = %v", err)
	}
	assertExited(t, l.PID)
	h, _ = r.Health(bound(t))
	if h.State != mesh.StateUnhealthy || h.Active {
		t.Errorf("post-kill health = %+v", h)
	}
	t.Setenv("MICA_TEST_IGNORE_TERM", "0")
	f.processing.Store(false)
	if err := r.Start(bound(t), f.cfg); err != nil {
		t.Fatal(err)
	}
	ready(t, r)
}
func TestMalformedStreams(t *testing.T) {
	cases := map[string]string{
		"bad json": "data: {bad}\n\n", "missing stop": "data: {\"content\":\"text\"}\n\n", "unknown reason": "data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"mystery\"}\n\n",
		"negative usage":             "data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\",\"tokens_predicted\":-1}\n\n",
		"extra event":                "data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\"}\n\ndata: {\"content\":\"extra\",\"stop\":false}\n\n",
		"terminal without delimiter": "data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\"}\n", "error event": "data: {\"error\":{\"message\":\"private prompt\"}}\n\n", "invalid utf8": "data: {\"content\":\"\xff\",\"stop\":false}\n\n",
		"overflow usage": "data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"limit\",\"tokens_evaluated\":2040,\"tokens_predicted\":512}\n\n",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.generateHandler = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, payload) }
			r := running(t, f)
			events, err := collect(r, bound(t), request())
			if !errors.Is(err, mesh.ErrMalformedResponse) {
				t.Fatalf("stream = %v", err)
			}
			if strings.Contains(err.Error(), "private prompt") {
				t.Fatal("leaked runtime payload")
			}
			assertNoCompleted(t, events)
		})
	}
}
func TestSSEHeartbeatAndOptionalUsage(t *testing.T) {
	f := setup(t)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, ":\n\ndata: {\"content\":\"text\",\"stop\":false}\r\n\r\ndata: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\",\"generation_settings\":{}}\n\n:\n\n")
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if err != nil || len(events) != 3 || events[2].InputTokens != nil || events[2].OutputTokens != nil {
		t.Fatalf("heartbeat/usage = %+v %v", events, err)
	}
}
func TestGenerationPreflightOwnsActivity(t *testing.T) {
	f := setup(t)
	called := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f.templateHandler = func(w http.ResponseWriter, req *http.Request) {
		close(called)
		select {
		case <-release:
			io.WriteString(w, `{"prompt":"formatted conversation"}`)
		case <-req.Context().Done():
		}
	}
	r := running(t, f)
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Generate(ctx, request(), func(mesh.Event) error { return nil }) }()
	select {
	case <-called:
	case err := <-done:
		t.Fatalf("preflight never called: %v", err)
	}
	h, err := r.Health(bound(t))
	if err != nil || !h.Active {
		t.Errorf("idle probe lost preflight activity: %+v %v", h, err)
	}
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("preflight cancel = %v", err)
	}
	ready(t, r)
}
func TestGenerateRejectsInvalidRequests(t *testing.T) {
	for name, mutate := range map[string]func(*mesh.Request){"empty": func(q *mesh.Request) { q.Prompt = "" }, "oversized": func(q *mesh.Request) { q.Prompt = strings.Repeat("x", 16385) }, "utf8": func(q *mesh.Request) { q.Prompt = "\xff" }, "output zero": func(q *mesh.Request) { q.MaxOutputTokens = 0 }, "output large": func(q *mesh.Request) { q.MaxOutputTokens = 513 }, "model": func(q *mesh.Request) { q.ModelID = "other" }} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			r := running(t, f)
			q := request()
			mutate(&q)
			events, err := collect(r, bound(t), q)
			if !errors.Is(err, mesh.ErrInvalidInput) || len(events) != 0 || f.completionCalls.Load() != 0 {
				t.Fatalf("invalid request = %+v %v", events, err)
			}
		})
	}
}
func TestGenerationCrashPreservesOwnership(t *testing.T) {
	f := setup(t)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		frame(w, `{"content":"text","stop":false}`)
		w.(http.Flusher).Flush()
	}
	r := running(t, f)
	l := receive(t, f.launched)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.Generate(bound(t), request(), func(e mesh.Event) error {
			if e.Kind == mesh.EventTextDelta {
				close(entered)
				<-release
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("no delta: %v", err)
	}
	response, _ := http.Get(fmt.Sprintf("http://127.0.0.1:%d/crash", f.cfg.Port))
	if response != nil {
		response.Body.Close()
	}
	r.mu.Lock()
	child := r.child
	r.mu.Unlock()
	receive(t, child.done)
	h, _ := r.Health(bound(t))
	if h.State != mesh.StateUnhealthy || !h.Active {
		t.Errorf("crash released generation ownership: %+v", h)
	}
	if err := r.Start(bound(t), f.cfg); !errors.Is(err, mesh.ErrUnavailable) {
		t.Errorf("Start replaced still-owned generation: %v", err)
	}
	close(release)
	err := receive(t, done)
	if !errors.Is(err, mesh.ErrUnavailable) || !errors.Is(err, child.err) {
		t.Errorf("crash identity = %v; child=%v", err, child.err)
	}
	assertExited(t, l.PID)
}
func TestCanceledBeforeCompletionEmitsNoCompleted(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		frame(w, `{"content":"text","stop":false}`)
		frame(w, `{"content":"","stop":true,"stop_type":"eos"}`)
	}
	r := running(t, f)
	var events []mesh.Event
	err := r.Generate(ctx, request(), func(e mesh.Event) error {
		events = append(events, e)
		if e.Kind == mesh.EventTextDelta {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel = %v", err)
	}
	assertNoCompleted(t, events)
}
func TestTemplateRejectsInvalidUTF8(t *testing.T) {
	f := setup(t)
	f.templateHandler = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "{\"prompt\":\"\xff\"}") }
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, mesh.ErrMalformedResponse) || len(events) != 0 {
		t.Fatalf("invalid template = %+v %v", events, err)
	}
}
func TestTokenizationAndTemplateFailures(t *testing.T) {
	for name, payload := range map[string]string{"missing": "{}", "bad id": "{\"tokens\":[\"x\"]}", "negative": "{\"tokens\":[-1]}", "empty": "{\"tokens\":[]}", "trailing": "{\"tokens\":[1]}{}", "oversized": strings.Repeat(" ", 65537) + `{"tokens":[1]}`} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.tokenHandler = func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, payload) }
			r := running(t, f)
			events, err := collect(r, bound(t), request())
			if !errors.Is(err, mesh.ErrMalformedResponse) || len(events) != 0 {
				t.Fatalf("tokenization = %+v %v", events, err)
			}
		})
	}
}
func TestTokenOverflowStopsReadingEarly(t *testing.T) {
	f := setup(t)
	canceled := make(chan struct{})
	f.tokenHandler = func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"tokens":[`+strings.Repeat("1,", 1537))
		w.(http.Flusher).Flush()
		<-req.Context().Done()
		close(canceled)
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, mesh.ErrInvalidInput) || len(events) != 0 {
		t.Fatalf("overflow = %+v %v", events, err)
	}
	receive(t, canceled)
}
func TestReportedInputMustMatchPreflight(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			f := setup(t)
			f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
				frame(w, fmt.Sprintf(`{"content":"","stop":true,"stop_type":"eos","tokens_evaluated":9,"tokens_predicted":1,"truncated":%t}`, truncated))
			}
			r := running(t, f)
			events, err := collect(r, bound(t), request())
			if !errors.Is(err, mesh.ErrMalformedResponse) {
				t.Fatalf("changed templated input = %+v %v", events, err)
			}
			assertNoCompleted(t, events)
		})
	}
}
func TestTokenizeControlDeadline(t *testing.T) {
	f := setup(t)
	canceled := make(chan struct{})
	f.tokenHandler = func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"tokens":[`)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
		close(canceled)
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, mesh.ErrMalformedResponse) || len(events) != 0 {
		t.Fatalf("control deadline = %+v %v", events, err)
	}
	receive(t, canceled)
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }
func TestBrokenStreamPreservesCause(t *testing.T) {
	cause := errors.New("broken connection")
	reader := io.MultiReader(strings.NewReader("data: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\"}\n\n"), failedReader{cause})
	_, err := readCompletion(reader, 2048, 10, 512, func(mesh.Event) error { return nil })
	if !errors.Is(err, cause) || !errors.Is(err, mesh.ErrMalformedResponse) {
		t.Fatalf("broken terminal transport = %v", err)
	}
}

func (t observingTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type observingTransport struct {
	base    http.RoundTripper
	observe func(*http.Request)
}

func (t observingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.observe(req)
	return t.base.RoundTrip(req)
}
func TestGenerationPreservesDeadlineBudget(t *testing.T) {
	for name, budget := range map[string]time.Duration{"missing": 0, "long": 600 * time.Second, "short": 4 * time.Second} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			r := running(t, f)
			ctx := context.Background()
			if budget != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, budget)
				defer cancel()
			}
			start := time.Now()
			r.client.Transport = observingTransport{r.client.Transport, func(req *http.Request) {
				deadline, ok := req.Context().Deadline()
				if !ok {
					t.Errorf("%s missing deadline", req.URL.Path)
					return
				}
				allowed := time.Second
				if req.URL.Path == "/completion" {
					allowed = 300 * time.Second
					if budget > 0 && budget < allowed {
						allowed = budget
					}
				}
				if deadline.After(start.Add(allowed + 100*time.Millisecond)) {
					t.Errorf("%s resets deadline beyond %s", req.URL.Path, allowed)
				}
			}}
			if _, err := collect(r, ctx, request()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCallbackBackpressureKeepsCapacity(t *testing.T) {
	f := setup(t)
	r := running(t, f)
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.Generate(ctx, request(), func(e mesh.Event) error {
			if e.Kind == mesh.EventTextDelta {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("no delta: %v", err)
	}
	h, err := r.Health(bound(t))
	if err != nil || !h.Active {
		t.Errorf("blocked consumer released activity: %+v %v", h, err)
	}
	if err := r.Generate(bound(t), request(), func(mesh.Event) error { return nil }); !errors.Is(err, mesh.ErrUnavailable) {
		t.Errorf("concurrent generation = %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("returned during emit: %v", err)
	default:
	}
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	ready(t, r)
}
func TestCancellationDuringIdleConfirmation(t *testing.T) {
	f := setup(t)
	probe := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	defer close(release)
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		f.processing.Store(true)
		frame(w, `{"content":"","stop":true,"stop_type":"eos"}`)
	}
	f.slotsHandler = func(w http.ResponseWriter, req *http.Request) bool {
		if !f.processing.Load() {
			return false
		}
		select {
		case probe <- struct{}{}:
		default:
		}
		select {
		case <-release:
			io.WriteString(w, `[{"is_processing":false}]`)
		case <-req.Context().Done():
		}
		return true
	}
	r := running(t, f)
	ctx, cancel := context.WithCancel(bound(t))
	defer cancel()
	done := make(chan error, 1)
	var events []mesh.Event
	go func() {
		err := r.Generate(ctx, request(), func(e mesh.Event) error { events = append(events, e); return nil })
		done <- err
	}()
	receive(t, probe)
	cancel()
	release <- struct{}{}
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("idle cleanup cancellation = %v", err)
	}
	assertNoCompleted(t, events)
}
func TestGenerationUsesConfiguredContext(t *testing.T) {
	f := setup(t)
	f.cfg.Model.ContextTokens = 4096
	f.tokenCount = 3584
	f.generateHandler = func(w http.ResponseWriter, _ *http.Request) {
		frame(w, `{"content":"","stop":true,"stop_type":"limit","tokens_evaluated":3584,"tokens_predicted":512,"truncated":true}`)
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if err != nil || len(events) != 2 || events[1].FinishReason != "length" {
		t.Fatalf("configured budget = %+v %v", events, err)
	}
}
func TestTokenizeBoundsTrailingWhitespace(t *testing.T) {
	f := setup(t)
	f.tokenHandler = func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"tokens":[1]}`+strings.Repeat(" ", 65537))
	}
	r := running(t, f)
	events, err := collect(r, bound(t), request())
	if !errors.Is(err, mesh.ErrMalformedResponse) || len(events) != 0 {
		t.Fatalf("oversized tokenizer suffix = %+v %v", events, err)
	}
}
func TestCanceledStreamKeepsContextClassification(t *testing.T) {
	for name, prefix := range map[string]string{"between frames": "data: {\"content\":\"text\",\"stop\":false}\n\n", "inside long frame": "data: {\"content\":\"" + strings.Repeat("x", 5000)} {
		t.Run(name, func(t *testing.T) {
			reader := io.MultiReader(strings.NewReader(prefix), failedReader{context.Canceled})
			_, err := readCompletion(reader, 2048, 10, 512, func(mesh.Event) error { return nil })
			if !errors.Is(err, context.Canceled) || errors.Is(err, mesh.ErrMalformedResponse) {
				t.Fatalf("canceled stream classification = %v", err)
			}
		})
	}
}
