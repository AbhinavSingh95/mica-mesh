package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"
	"time"
	"unicode/utf8"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

const (
	generationBudget = 300 * time.Second
	idleBudget       = 2 * time.Second
	idlePollInterval = 25 * time.Millisecond
	maxPromptBytes   = 16 * 1024
	maxDeltaBytes    = 4096
)

var _ mesh.Runtime = (*Runtime)(nil)

// Generate owns activity through preflight, streaming and bounded idle/process
// cleanup. emit runs synchronously and must respect its caller's cancellation.
func (r *Runtime) Generate(ctx context.Context, req mesh.Request, emit func(mesh.Event) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, generationBudget)
	defer cancel()
	if err = ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.generating || r.health.State != mesh.StateReady || r.health.Active {
		r.mu.Unlock()
		return fmt.Errorf("generate: %w", mesh.ErrUnavailable)
	}
	child := r.child
	select {
	case <-child.done:
		r.mu.Unlock()
		return fmt.Errorf("generate: %w", errors.Join(mesh.ErrUnavailable, child.err))
	default:
	}
	r.generating = true
	r.health.Active = true
	model := r.capabilities.Model
	r.mu.Unlock()
	dispatched, idleConfirmed := false, false
	// Start cannot replace the child while this owner is present, even after exit.
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		select {
		case <-child.done:
			err = errors.Join(err, fmt.Errorf("owned runtime exited: %w", errors.Join(mesh.ErrUnavailable, child.err)))
		default:
		}
		r.mu.Lock()
		r.generating = false
		r.health.Active = dispatched && !idleConfirmed
		select {
		case <-child.done:
			r.health.Active = false
		default:
		}
		r.mu.Unlock()
	}()
	if len(req.Prompt) == 0 || len(req.Prompt) > maxPromptBytes || !utf8.ValidString(req.Prompt) || req.MaxOutputTokens < 1 || req.MaxOutputTokens > 512 || req.ModelID != model.ID {
		return fmt.Errorf("invalid prompt, output limit or model: %w", mesh.ErrInvalidInput)
	}
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var template struct {
		Prompt *string `json:"prompt"`
	}
	body, _ := json.Marshal(struct {
		Messages            []message `json:"messages"`
		AddGenerationPrompt bool      `json:"add_generation_prompt"`
	}{[]message{{"system", "You are a helpful assistant."}, {"user", req.Prompt}}, true})
	control, cancelControl := context.WithTimeout(ctx, controlBudget)
	err = r.requestJSON(control, http.MethodPost, "/apply-template", body, &template)
	cancelControl()
	if err != nil {
		return fmt.Errorf("apply chat template: %w", err)
	}
	if template.Prompt == nil || *template.Prompt == "" || !utf8.ValidString(*template.Prompt) {
		return fmt.Errorf("invalid chat template: %w", mesh.ErrMalformedResponse)
	}
	inputTokens, err := r.checkTokenBudget(ctx, *template.Prompt, model.ContextTokens-req.MaxOutputTokens)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = emit(mesh.Event{Kind: mesh.EventStarted}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	body, _ = json.Marshal(struct {
		Prompt      string  `json:"prompt"`
		NPredict    int     `json:"n_predict"`
		Temperature float64 `json:"temperature"`
		Stream      bool    `json:"stream"`
	}{*template.Prompt, req.MaxOutputTokens, 0.7, true})
	// Any attempt to dispatch is ambiguous on error. Cancel and close the HTTP
	// operation before polling with a separate cleanup lifetime.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	dispatched = true
	response, streamErr := r.requestHTTP(streamCtx, http.MethodPost, "/completion", body)
	var completed mesh.Event
	if streamErr == nil {
		completed, streamErr = readCompletion(response.Body, model.ContextTokens, inputTokens, req.MaxOutputTokens, func(e mesh.Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return emit(e)
		})
	}
	cancelStream()
	if response != nil {
		streamErr = errors.Join(streamErr, response.Body.Close())
	}
	// Preserve the incoming deadline/cancellation, not our own stream cancellation.
	// readCompletion has already checked the request context at emission/read errors.
	cleanupErr := r.finishGeneration(context.WithoutCancel(ctx), child)
	idleConfirmed = cleanupErr == nil
	if streamErr != nil || cleanupErr != nil {
		return errors.Join(streamErr, cleanupErr)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	select {
	case <-child.done:
		return fmt.Errorf("runtime exited before completion: %w", errors.Join(mesh.ErrUnavailable, child.err))
	default:
	}
	return emit(completed)
}

// Token IDs are counted as they arrive; overflow can reject a large valid prompt
// without allocating an unbounded token slice or waiting for the entire response.
func (r *Runtime) checkTokenBudget(ctx context.Context, prompt string, limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("output exceeds context: %w", mesh.ErrInvalidInput)
	}
	body, _ := json.Marshal(struct {
		Content      string `json:"content"`
		AddSpecial   bool   `json:"add_special"`
		ParseSpecial bool   `json:"parse_special"`
	}{prompt, true, true})
	ctx, cancel := context.WithTimeout(ctx, controlBudget)
	defer cancel()
	response, err := r.requestHTTP(ctx, http.MethodPost, "/tokenize", body)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	limited := &io.LimitedReader{R: response.Body, N: maxResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	malformed := func() (int, error) {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("invalid tokenization response: %w", mesh.ErrMalformedResponse)
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return malformed()
	}
	found := false
	count := 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return malformed()
		}
		if key != "tokens" {
			var ignored json.RawMessage
			if decoder.Decode(&ignored) != nil {
				return malformed()
			}
			continue
		}
		if found {
			return malformed()
		}
		found = true
		token, err = decoder.Token()
		if err != nil || token != json.Delim('[') {
			return malformed()
		}
		for decoder.More() {
			var id int64
			if decoder.Decode(&id) != nil || id < 0 {
				return malformed()
			}
			count++
			if count > limit {
				return 0, fmt.Errorf("templated input plus output exceeds context: %w", mesh.ErrInvalidInput)
			}
		}
		token, err = decoder.Token()
		if err != nil || token != json.Delim(']') {
			return malformed()
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || !found || count == 0 || decoder.InputOffset() > maxResponseBytes {
		return malformed()
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF || limited.N == 0 {
		return malformed()
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return count, nil
}

type completionFrame struct {
	Content      *string `json:"content"`
	Stop         *bool   `json:"stop"`
	StopType     string  `json:"stop_type"`
	InputTokens  *int64  `json:"tokens_evaluated"`
	OutputTokens *int64  `json:"tokens_predicted"`
	// Native truncated can be true at exact budget even without input truncation.
	// Validate its wire type, and use measured/reported token accounting instead.
	Truncated *bool `json:"truncated"`
}

// A bounded frame and the Reader's fixed buffer are the only stream storage.
// Completion is retained until EOF confirms no trailing event or transport error.
func readCompletion(body io.Reader, contextTokens, inputTokens, maxOutput int, emit func(mesh.Event) error) (mesh.Event, error) {
	reader := bufio.NewReaderSize(body, 4096)
	frameBytes := 0
	data := make([]byte, 0, 4096)
	terminal := false
	var completed mesh.Event
	malformed := func(reason string) (mesh.Event, error) {
		return mesh.Event{}, fmt.Errorf("%s: %w", reason, mesh.ErrMalformedResponse)
	}
	for {
		line, err := reader.ReadSlice('\n')
		frameBytes += len(line)
		if frameBytes > maxResponseBytes {
			return malformed("runtime event exceeds 64 KiB")
		}
		if err != nil {
			if err == bufio.ErrBufferFull {
				// ReadSlice's fragments must be joined only within the same bounded line.
				rest, readErr := readBoundedLine(reader, line, &frameBytes)
				if readErr != nil {
					return mesh.Event{}, readErr
				}
				line = rest
			} else if err == io.EOF {
				if len(line) != 0 || frameBytes != 0 || !terminal {
					return malformed("runtime stream lacks a clean terminal event")
				}
				return completed, nil
			} else {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return mesh.Event{}, err
				}
				return mesh.Event{}, fmt.Errorf("read runtime stream: %w: %w", mesh.ErrMalformedResponse, err)
			}
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > 0 {
			if line[0] == ':' {
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				return malformed("invalid runtime SSE field")
			}
			payload := line[len("data:"):]
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, payload...)
			continue
		}
		frameBytes = 0
		if len(data) == 0 {
			continue
		}
		if terminal {
			return malformed("runtime event follows terminal")
		}
		if !utf8.Valid(data) {
			return malformed("invalid runtime event UTF-8")
		}
		var frame completionFrame
		if json.Unmarshal(data, &frame) != nil || frame.Content == nil || frame.Stop == nil || !utf8.ValidString(*frame.Content) {
			return malformed("invalid runtime completion event")
		}
		data = data[:0]
		if frame.InputTokens != nil && (*frame.InputTokens != int64(inputTokens)) || frame.OutputTokens != nil && (*frame.OutputTokens < 0 || *frame.OutputTokens > int64(maxOutput)) {
			return malformed("invalid runtime usage")
		}
		if frame.InputTokens != nil && frame.OutputTokens != nil && *frame.InputTokens+*frame.OutputTokens > int64(contextTokens) {
			return malformed("runtime usage exceeds context")
		}
		if *frame.Stop {
			reason := "stop"
			switch frame.StopType {
			case "eos", "word":
			case "limit":
				reason = "length"
			default:
				return malformed("unknown runtime finish reason")
			}
			terminal = true
			completed = mesh.Event{Kind: mesh.EventCompleted, FinishReason: reason, InputTokens: frame.InputTokens, OutputTokens: frame.OutputTokens}
		} else if frame.StopType != "" {
			return malformed("nonterminal event has finish reason")
		}
		text := *frame.Content
		for len(text) > 0 {
			size := min(len(text), maxDeltaBytes)
			if size < len(text) {
				for !utf8.RuneStart(text[size]) {
					size--
				}
			}
			if err := emit(mesh.Event{Kind: mesh.EventTextDelta, Text: text[:size]}); err != nil {
				return mesh.Event{}, err
			}
			text = text[size:]
		}
	}
}
func readBoundedLine(reader *bufio.Reader, first []byte, frameBytes *int) ([]byte, error) {
	line := append([]byte(nil), first...)
	for {
		part, err := reader.ReadSlice('\n')
		*frameBytes += len(part)
		if *frameBytes > maxResponseBytes {
			return nil, fmt.Errorf("runtime event exceeds 64 KiB: %w", mesh.ErrMalformedResponse)
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("unterminated runtime event: %w: %w", mesh.ErrMalformedResponse, err)
		}
		return line, nil
	}
}

func (r *Runtime) cleanupContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if r.cleanupTimeout != nil {
		return r.cleanupTimeout(ctx, d)
	}
	return context.WithTimeout(ctx, d)
}
func (r *Runtime) finishGeneration(ctx context.Context, child *childProcess) error {
	idle, cancel := r.cleanupContext(ctx, idleBudget)
	var idleErr error
	for {
		select {
		case <-child.done:
			cancel()
			return fmt.Errorf("runtime exited during generation: %w", errors.Join(mesh.ErrUnavailable, child.err))
		default:
		}
		var slots []slot
		idleErr = r.controlJSON(idle, "/slots", &slots)
		if idleErr == nil && len(slots) == 1 && slots[0].Processing != nil && !*slots[0].Processing {
			cancel()
			return nil
		}
		if idleErr == nil {
			idleErr = fmt.Errorf("runtime idle not confirmed: %w", mesh.ErrUnavailable)
		}
		timer := time.NewTimer(idlePollInterval)
		select {
		case <-idle.Done():
			timer.Stop()
			cancel()
			return r.terminateGeneration(ctx, child, idleErr)
		case <-child.done:
			timer.Stop()
			cancel()
			return fmt.Errorf("runtime exited during cleanup: %w", errors.Join(mesh.ErrUnavailable, child.err))
		case <-timer.C:
		}
	}
}
func (r *Runtime) terminateGeneration(ctx context.Context, child *childProcess, idleErr error) error {
	r.mu.Lock()
	r.health.State = mesh.StateUnhealthy
	r.health.LastError = idleErr.Error()
	r.mu.Unlock()
	// One total termination/reap budget, reserving its last second for SIGKILL.
	cleanup, cancel := r.cleanupContext(ctx, terminationBudget)
	defer cancel()
	grace, cancelGrace := r.cleanupContext(cleanup, terminationBudget-time.Second)
	defer cancelGrace()
	signalErr := child.cmd.Process.Signal(syscall.SIGTERM)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		idleErr = errors.Join(idleErr, fmt.Errorf("terminate owned runtime: %w", signalErr))
	}
	select {
	case <-child.done:
		r.client.CloseIdleConnections()
		return idleErr
	case <-grace.Done():
	}
	if err := child.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.Join(idleErr, fmt.Errorf("kill owned runtime: %w", err))
	}
	select {
	case <-child.done:
		r.client.CloseIdleConnections()
		return idleErr
	case <-cleanup.Done():
		return errors.Join(idleErr, fmt.Errorf("owned runtime not reaped: %w", mesh.ErrUnavailable))
	}
}
