// Package worker owns local inference capacity and worker lifecycle.
package worker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type lifecycle int

const (
	lifecycleStarting lifecycle = iota
	lifecycleRunning
	lifecycleRecovering
	lifecycleStopping
)

// Service owns one local slot through validation, generation and runtime cleanup.
// RunRuntime is called once by the process owner, independently of membership.
type Service struct {
	meshv1.UnimplementedWorkerServiceServer
	mu            sync.Mutex
	id            string
	cfg           mesh.Config
	hardware      *meshv1.HardwareInfo
	rt            mesh.Runtime
	report        *meshv1.WorkerReport
	revision      uint64
	lifecycle     lifecycle
	activeCancel  context.CancelFunc
	activeDone    chan struct{}
	runtimeActive bool // Includes activity whose idle/reap confirmation is still missing.
}

// New creates a starting worker. Configuration has already been structurally validated.
// Model verification and readiness remain runtime-owned. Hardware must be non-nil and is copied.
func New(id string, cfg mesh.Config, hardware *meshv1.HardwareInfo, rt mesh.Runtime) *Service {
	return &Service{id: id, cfg: cfg, hardware: proto.Clone(hardware).(*meshv1.HardwareInfo), rt: rt, report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_STARTING}}
}

// Report returns a detached snapshot without making network calls.
func (s *Service) Report() *meshv1.WorkerReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return proto.Clone(s.report).(*meshv1.WorkerReport)
}

// Health returns the process identity and current local ownership/readiness.
func (s *Service) Health(ctx context.Context, _ *meshv1.HealthRequest) (*meshv1.HealthResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return &meshv1.HealthResponse{WorkerId: s.id, Report: s.Report()}, nil
}

// Generate admits immediately or rejects overlap; callbacks preserve downstream backpressure.
func (s *Service) Generate(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	ctx := out.Context()
	cancel, ok := protocol.GenerationCancel(ctx)
	if !ok {
		return status.Error(codes.Internal, "generation transport lifetime is not configured")
	}
	defer cancel()
	if err := protocol.ValidateRequest(req, s.cfg.Model.ID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	s.mu.Lock()
	if s.report.Active {
		s.mu.Unlock()
		return status.Error(codes.ResourceExhausted, "worker is busy")
	}
	if s.lifecycle != lifecycleRunning || s.report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_READY {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "runtime is not ready")
	}
	s.activeCancel = cancel
	done := make(chan struct{})
	s.activeDone = done
	s.report.Active = true
	id := req.RequestId
	s.report.ActiveRequestId = &id
	s.revision++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.activeCancel = nil
		s.activeDone = nil
		s.updateActivityLocked()
		s.revision++
		close(done)
		s.mu.Unlock()
	}()
	started := false
	var terminal *meshv1.Completed
	err := s.rt.Generate(ctx, mesh.Request{ID: req.RequestId, ModelID: req.ModelId, Prompt: req.Prompt, MaxOutputTokens: int(req.MaxOutputTokens)}, func(e mesh.Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if terminal != nil {
			return mesh.ErrMalformedResponse
		}
		switch e.Kind {
		case mesh.EventStarted:
			if started || e.Text != "" || e.FinishReason != "" || e.InputTokens != nil || e.OutputTokens != nil {
				return mesh.ErrMalformedResponse
			}
			started = true
			return out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: req.RequestId, WorkerId: s.id, WorkerHostname: s.hardware.Hostname, ModelId: req.ModelId}}})
		case mesh.EventTextDelta:
			if !started || !utf8.ValidString(e.Text) || len(e.Text) > 4096 || e.FinishReason != "" || e.InputTokens != nil || e.OutputTokens != nil {
				return mesh.ErrMalformedResponse
			}
			return out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: e.Text}}})
		case mesh.EventCompleted:
			if !started || e.Text != "" || (e.FinishReason != "stop" && e.FinishReason != "length") || (e.InputTokens != nil && *e.InputTokens < 0) || (e.OutputTokens != nil && *e.OutputTokens < 0) {
				return mesh.ErrMalformedResponse
			}
			terminal = &meshv1.Completed{FinishReason: e.FinishReason, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens}
			return nil
		default:
			return mesh.ErrMalformedResponse
		}
	})
	if err == nil && (!started || terminal == nil) {
		err = mesh.ErrMalformedResponse
	}
	// A returned handler does not prove runtime idle when cleanup failed. Invalidate
	// probes from before this return and retain ownership until a fresh idle result.
	s.mu.Lock()
	s.runtimeActive = true
	s.revision++
	s.mu.Unlock()
	s.refreshHealth(context.WithoutCancel(ctx))
	if errors.Is(err, mesh.ErrUnavailable) || errors.Is(err, mesh.ErrMalformedResponse) {
		s.setState(meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, "runtime generation failed")
	}
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if err != nil {
		return runtimeStatus(err)
	}
	if err := out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: terminal}}); err != nil {
		return runtimeStatus(err)
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return nil
}
func runtimeStatus(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "generation deadline exceeded")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "generation canceled")
	case errors.Is(err, mesh.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, "runtime rejected input or context budget")
	case errors.Is(err, mesh.ErrUnavailable):
		return status.Error(codes.Unavailable, "runtime unavailable")
	default:
		return status.Error(codes.Internal, "runtime generation failed")
	}
}
func (s *Service) setState(state meshv1.RuntimeState, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifecycle == lifecycleStopping {
		return
	}
	s.report.RuntimeState = state
	s.report.LastError = diagnostic(lastError)
	s.revision++
}
func (s *Service) refreshHealth(ctx context.Context) (mesh.Health, error) {
	s.mu.Lock()
	revision := s.revision
	s.mu.Unlock()
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	health, err := s.rt.Health(probe)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != revision {
		return health, err
	}
	if err == nil {
		s.runtimeActive = health.Active
		s.updateActivityLocked()
	}
	// Shutdown health may confirm activity ended, but cannot restore readiness.
	if s.lifecycle == lifecycleStopping {
		s.revision++
		return health, err
	}
	if err != nil {
		s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY
		s.report.LastError = "runtime health check failed"
	} else {
		switch health.State {
		case mesh.StateReady:
			if s.lifecycle == lifecycleRunning {
				s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_READY
			}
		case mesh.StateStarting:
			s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_STARTING
		default:
			s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY
		}
		s.report.LastError = diagnostic(health.LastError)
	}
	s.revision++
	return health, err
}

// Handler ownership and runtime activity independently retain the local slot.
// Call with mu held; discard the request identity only when both are idle.
func (s *Service) updateActivityLocked() {
	s.report.Active = s.activeDone != nil || s.runtimeActive
	if !s.report.Active {
		s.report.ActiveRequestId = nil
	}
}

// Runtime stderr can be non-UTF-8 and larger than a control RPC. Keep reports
// serializable and leave room for identity/hardware within the 64KiB message cap.
func diagnostic(text string) string {
	text = strings.ToValidUTF8(text, "�")
	const maxBytes = 4096
	const suffix = "… [truncated]"
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes - len(suffix)
	for !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + suffix
}
