// Package llamacpp owns the local pinned llama.cpp server.
package llamacpp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

const (
	runtimeVersion    = "0.5.0"
	startupBudget     = 120 * time.Second
	healthInterval    = time.Second
	controlBudget     = time.Second
	terminationBudget = 3 * time.Second
	maxResponseBytes  = 64 * 1024
)

// Runtime owns one child and a reusable loopback client. Callers serialize Start/Stop;
// Health and Capabilities may run concurrently with either operation.
// Task 5 adds generation to this lifecycle implementation.
type Runtime struct {
	mu           sync.Mutex
	health       mesh.Health
	capabilities mesh.Capabilities
	child        *childProcess
	baseURL      string
	client       *http.Client
}
type childProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error // Written before done closes, read only after receiving done.
}

// New constructs an unhealthy runtime with no owned child.
func New() *Runtime {
	return &Runtime{health: mesh.Health{State: mesh.StateUnhealthy}, client: &http.Client{Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}}}
}

// Start validates local artifacts, starts the owned server, and waits through warm-up.
// Failure stops/reaps the child. Successful startup does not attach the child lifetime
// to this request's context; the caller must eventually Stop it.
func (r *Runtime) Start(ctx context.Context, cfg mesh.Config) (err error) {
	r.mu.Lock()
	if r.child != nil {
		select {
		case <-r.child.done:
		default:
			r.mu.Unlock()
			return fmt.Errorf("start: %w: owned child is still running", mesh.ErrUnavailable)
		}
	}
	r.health = mesh.Health{State: mesh.StateStarting}
	r.capabilities = mesh.Capabilities{}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, startupBudget)
	defer cancel()
	defer func() {
		if err != nil {
			cleanupErr := r.Stop(context.Background())
			err = errors.Join(err, cleanupErr)
			r.mu.Lock()
			r.health.State = mesh.StateUnhealthy
			r.health.LastError = err.Error()
			r.mu.Unlock()
		}
	}()
	if err = validateArtifacts(ctx, cfg); err != nil {
		return err
	}
	if _, err = r.runCheck(ctx, cfg.BinaryPath, "--version"); err != nil {
		return err
	}
	device, layers := "none", "0"
	if cfg.Backend == "metal" {
		var output string
		output, err = r.runCheck(ctx, cfg.BinaryPath, "--list-devices")
		if err != nil {
			return err
		}
		if !regexp.MustCompile(`(?m)^\s*MTL0:`).MatchString(output) {
			return fmt.Errorf("metal device MTL0 unavailable: %w", mesh.ErrInvalidInput)
		}
		device, layers = "MTL0", "all"
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	listener, listenErr := net.Listen("tcp", address)
	if listenErr != nil {
		return fmt.Errorf("runtime port unavailable: %w: %v", mesh.ErrInvalidInput, listenErr)
	}
	if err = listener.Close(); err != nil {
		return fmt.Errorf("release runtime port check: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	args := []string{"--model", cfg.ModelPath, "--offline", "--host", "127.0.0.1", "--port", strconv.Itoa(cfg.Port), "--parallel", "1", "--ctx-size", strconv.Itoa(cfg.Model.ContextTokens), "--fit", "off", "--no-context-shift", "--sleep-idle-seconds", "-1", "--slots", "--jinja", "--warmup", "--no-ui", "--device", device, "--gpu-layers", layers}
	cmd := exec.Command(cfg.BinaryPath, args...)
	cmd.Env = runtimeEnvironment()
	cmd.Stdout = io.Discard
	log := &diagnostics{}
	cmd.Stderr = log
	cmd.WaitDelay = time.Second
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("launch runtime: %w: %v", mesh.ErrUnavailable, err)
	}
	child := &childProcess{cmd: cmd, done: make(chan struct{})}
	r.mu.Lock()
	r.child = child
	r.baseURL = "http://" + address
	r.mu.Unlock()
	go func() {
		child.err = cmd.Wait()
		r.mu.Lock()
		if r.child == child {
			r.health.Active = false
			r.health.State = mesh.StateUnhealthy
			r.health.LastError = fmt.Sprintf("runtime exited: %v; stderr: %s", child.err, log.String())
		}
		close(child.done)
		r.mu.Unlock()
	}()
	for {
		select {
		case <-child.done:
			return fmt.Errorf("runtime exited during startup: %w: %v; stderr: %s", mesh.ErrUnavailable, child.err, log.String())
		default:
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		var health struct {
			Status string `json:"status"`
		}
		probeErr := r.controlJSON(ctx, "/health", &health)
		if probeErr == nil && health.Status == "ok" {
			break
		}
		timer := time.NewTimer(healthInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-child.done:
			timer.Stop()
			return fmt.Errorf("runtime exited during startup: %w: %v; stderr: %s", mesh.ErrUnavailable, child.err, log.String())
		case <-timer.C:
		}
	}
	var props struct {
		ModelPath  string `json:"model_path"`
		TotalSlots *int   `json:"total_slots"`
		IsSleeping *bool  `json:"is_sleeping"`
		BuildInfo  string `json:"build_info"`
	}
	if err = r.controlJSON(ctx, "/props", &props); err != nil {
		return fmt.Errorf("verify runtime properties: %w", err)
	}
	if props.ModelPath != cfg.ModelPath || props.TotalSlots == nil || *props.TotalSlots != 1 || props.IsSleeping == nil || *props.IsSleeping || props.BuildInfo == "" {
		return fmt.Errorf("runtime properties differ from configured model/one-slot/awake contract: %w", mesh.ErrUnavailable)
	}
	var slots []slot
	if err = r.controlJSON(ctx, "/slots", &slots); err != nil {
		return fmt.Errorf("verify runtime slot: %w", err)
	}
	if len(slots) != 1 || slots[0].ContextTokens == nil || *slots[0].ContextTokens != cfg.Model.ContextTokens || slots[0].Processing == nil || *slots[0].Processing {
		return fmt.Errorf("runtime slot differs from configured idle context: %w", mesh.ErrUnavailable)
	}
	var warmup struct {
		Stop   bool `json:"stop"`
		Tokens int  `json:"tokens_predicted"`
	}
	if err = r.requestJSON(ctx, http.MethodPost, "/completion", []byte(`{"prompt":"Hello","n_predict":1,"stream":false,"temperature":0.7}`), &warmup); err != nil {
		return fmt.Errorf("runtime warm-up: %w", err)
	}
	if !warmup.Stop || warmup.Tokens != 1 {
		return fmt.Errorf("runtime warm-up did not complete one token: %w", mesh.ErrMalformedResponse)
	}
	if err = r.controlJSON(ctx, "/slots", &slots); err != nil {
		return fmt.Errorf("confirm idle after warm-up: %w", err)
	}
	if len(slots) != 1 || slots[0].Processing == nil || *slots[0].Processing {
		return fmt.Errorf("runtime remains active after warm-up: %w", mesh.ErrUnavailable)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-child.done:
		return fmt.Errorf("runtime exited during warm-up: %w: %v", mesh.ErrUnavailable, child.err)
	default:
	}
	r.capabilities = mesh.Capabilities{Model: cfg.Model, RuntimeVersion: runtimeVersion, Backend: cfg.Backend, Capacity: 1}
	r.health = mesh.Health{State: mesh.StateReady}
	return nil
}

func validateArtifacts(ctx context.Context, cfg mesh.Config) error {
	if !filepath.IsAbs(cfg.BinaryPath) || !filepath.IsAbs(cfg.ModelPath) || cfg.Port < 1 || cfg.Port > 65535 || (cfg.Backend != "cpu" && cfg.Backend != "metal") || cfg.Model.ID == "" || cfg.Model.ContextTokens < 1 || uint64(cfg.Model.ContextTokens) > 1<<32-1 {
		return fmt.Errorf("invalid runtime paths, port, backend, or model/context: %w", mesh.ErrInvalidInput)
	}
	digest, err := hex.DecodeString(cfg.Model.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("invalid model SHA-256: %w", mesh.ErrInvalidInput)
	}
	binary, err := os.Stat(cfg.BinaryPath)
	if err != nil || !binary.Mode().IsRegular() || binary.Mode()&0111 == 0 {
		return fmt.Errorf("runtime binary is not an executable regular file: %w", mesh.ErrInvalidInput)
	}
	// Model paths are operator-controlled and must stay unchanged during startup.
	// Reject FIFOs/devices before open; opening a FIFO can wait past cancellation.
	modelInfo, err := os.Stat(cfg.ModelPath)
	if err != nil || !modelInfo.Mode().IsRegular() {
		return fmt.Errorf("model path is not a regular file: %w", mesh.ErrInvalidInput)
	}
	file, err := os.Open(cfg.ModelPath)
	if err != nil {
		return fmt.Errorf("open model: %w: %v", mesh.ErrInvalidInput, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("model is not a regular file: %w", mesh.ErrInvalidInput)
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		hash.Write(buffer[:n])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read model: %w: %v", mesh.ErrInvalidInput, readErr)
		}
	}
	if !bytes.Equal(hash.Sum(nil), digest) {
		return fmt.Errorf("model checksum mismatch: %w", mesh.ErrInvalidInput)
	}
	return nil
}
func runtimeEnvironment() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "LLAMA_ARG_") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
func (r *Runtime) runCheck(ctx context.Context, binary, flag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, controlBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, flag)
	cmd.Env = runtimeEnvironment()
	cmd.WaitDelay = time.Second
	output := &diagnostics{}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("runtime %s check failed: %w: %v", flag, mesh.ErrInvalidInput, err)
	}
	text := output.String()
	if flag == "--version" && !regexp.MustCompile(`(?m)^version:\s*0\.5\.0\s*\(`).MatchString(text) {
		return "", fmt.Errorf("expected runtime version %s: %w", runtimeVersion, mesh.ErrInvalidInput)
	}
	return text, nil
}

// Stop reaps only the owned child and is safe after partial startup/repeated calls.
// Cleanup deliberately has a separate bounded lifetime even if ctx is canceled.
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	child := r.child
	r.health.State = mesh.StateUnhealthy
	r.mu.Unlock()
	if child == nil {
		return nil
	}
	// Stop is process cleanup, not request work: cancellation must not abandon ownership.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminationBudget)
	defer cancel()
	select {
	case <-child.done:
		r.client.CloseIdleConnections()
		return nil
	default:
	}
	signalErr := child.cmd.Process.Signal(syscall.SIGTERM)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return fmt.Errorf("terminate owned runtime: %w", signalErr)
	}
	select {
	case <-child.done:
		r.client.CloseIdleConnections()
		return nil
	case <-cleanup.Done():
	}
	if err := child.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill owned runtime: %w", err)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-child.done:
		r.client.CloseIdleConnections()
		return nil
	case <-timer.C:
		return fmt.Errorf("owned runtime was not reaped after kill: %w", mesh.ErrUnavailable)
	}
}

type slot struct {
	ContextTokens *int  `json:"n_ctx"`
	Processing    *bool `json:"is_processing"`
}

// Health probes readiness and slot activity only after successful startup. A stale
// result cannot overwrite process exit or another instance's state.
func (r *Runtime) Health(ctx context.Context) (mesh.Health, error) {
	if err := ctx.Err(); err != nil {
		return mesh.Health{}, err
	}
	r.mu.Lock()
	snapshot := r.health
	child := r.child
	r.mu.Unlock()
	if snapshot.State != mesh.StateReady {
		return snapshot, nil
	}
	var health struct {
		Status string `json:"status"`
	}
	err := r.controlJSON(ctx, "/health", &health)
	if err == nil && health.Status != "ok" {
		err = fmt.Errorf("runtime health is not ready: %w", mesh.ErrUnavailable)
	}
	var slots []slot
	if err == nil {
		err = r.controlJSON(ctx, "/slots", &slots)
		if err == nil && (len(slots) != 1 || slots[0].Processing == nil) {
			err = fmt.Errorf("invalid runtime slots: %w", mesh.ErrMalformedResponse)
		}
	}
	if ctx.Err() != nil {
		return snapshot, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.child == child && r.health.State == mesh.StateReady {
		if err != nil {
			r.health.State = mesh.StateUnhealthy
			r.health.LastError = err.Error()
		} else {
			r.health.Active = *slots[0].Processing
		}
	}
	return r.health, err
}

// Capabilities returns a value snapshot of the verified, warmed configuration.
func (r *Runtime) Capabilities() mesh.Capabilities {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capabilities
}
func (r *Runtime) controlJSON(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, controlBudget)
	defer cancel()
	return r.requestJSON(ctx, http.MethodGet, path, nil, target)
}
func (r *Runtime) requestJSON(ctx context.Context, method, path string, body []byte, target any) error {
	r.mu.Lock()
	baseURL := r.baseURL
	r.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("runtime %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime %s HTTP %d: %w", path, response.StatusCode, mesh.ErrUnavailable)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read runtime %s: %w", path, err)
	}
	if len(payload) > maxResponseBytes {
		return fmt.Errorf("runtime %s response exceeds 64 KiB: %w", path, mesh.ErrMalformedResponse)
	}
	if err = json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode runtime %s: %w: %v", path, mesh.ErrMalformedResponse, err)
	}
	return nil
}

// diagnostics continuously accepts stderr while retaining only the bounded tail.
// exec.Cmd owns draining/copy goroutines and Wait joins them.
type diagnostics struct {
	mu   sync.Mutex
	tail []byte
}

func (d *diagnostics) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if n >= maxResponseBytes {
		d.tail = append(d.tail[:0], p[n-maxResponseBytes:]...)
	} else {
		overflow := len(d.tail) + n - maxResponseBytes
		if overflow > 0 {
			copy(d.tail, d.tail[overflow:])
			d.tail = d.tail[:len(d.tail)-overflow]
		}
		d.tail = append(d.tail, p...)
	}
	return n, nil
}
func (d *diagnostics) String() string { d.mu.Lock(); defer d.mu.Unlock(); return string(d.tail) }
