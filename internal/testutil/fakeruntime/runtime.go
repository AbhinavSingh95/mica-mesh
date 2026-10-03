// Package fakeruntime supplies controllable runtime behavior for mesh tests.
package fakeruntime

import (
	"context"
	"errors"
	"sync"
	"time"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

// Counters is an immutable observation of work, including cancellation cleanup.
type Counters struct{ Starts, Active, Peak, Stops int }

// Runtime is a test-only runtime. Configure gates, errors, and events before the
// affected call starts; do not mutate them while that call runs. Nil gates pass
// immediately. Tests close or send to gates; the fake never closes them.
// Observation channels carry coalesced signals (one pending signal per phase).
// Tests receive them; the fake never closes them. Counters retain exact totals.
// The fake permits overlapping calls so worker tests can detect a missing guard.
type Runtime struct {
	Started, Admitted, FirstEvent, Cleaning, Released                  chan struct{}
	StartGate, AdmissionGate, FirstEventGate, ReleaseGate, CleanupGate <-chan struct{}
	StartError, GenerateError, CleanupError                            error
	Events                                                             []mesh.Event // Nil selects Started then Completed; empty emits nothing.

	mu           sync.Mutex
	counters     Counters
	health       mesh.Health
	capabilities mesh.Capabilities
	stopCancel   context.CancelFunc
	session      context.Context
	idle         chan struct{} // Closed only by the last departing generation, under mu.
	stopped      bool
}

// New constructs an unhealthy fake with bounded observation channels.
func New() *Runtime {
	idle := make(chan struct{})
	close(idle)
	return &Runtime{Started: make(chan struct{}, 1), Admitted: make(chan struct{}, 1), FirstEvent: make(chan struct{}, 1), Cleaning: make(chan struct{}, 1), Released: make(chan struct{}, 1), health: mesh.Health{State: mesh.StateUnhealthy}, idle: idle, stopped: true}
}

// Start exposes the starting transition before waiting on StartGate.
func (r *Runtime) Start(ctx context.Context, cfg mesh.Config) error {
	r.mu.Lock()
	if !r.stopped || r.counters.Active != 0 {
		r.mu.Unlock()
		return mesh.ErrUnavailable
	}
	r.counters.Starts++
	r.health = mesh.Health{State: mesh.StateStarting}
	r.stopped = false
	r.session, r.stopCancel = context.WithCancel(context.Background())
	r.mu.Unlock()
	signal(r.Started)
	err := gate(ctx, r.StartGate)
	if err == nil {
		err = r.StartError
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.health.State = mesh.StateUnhealthy
		r.health.LastError = err.Error()
		r.stopCancel()
		r.stopped = true
		return err
	}
	r.health.State = mesh.StateReady
	r.capabilities = mesh.Capabilities{Model: cfg.Model, Backend: cfg.Backend, RuntimeVersion: "fake", Capacity: 1}
	return nil
}

// Stop cancels admitted work, waits for bounded cleanup, and is idempotent.
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		r.counters.Stops++
		r.health.State = mesh.StateUnhealthy
		r.stopCancel()
	}
	idle := r.idle
	r.mu.Unlock()
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	select {
	case <-idle:
		return nil
	case <-cleanup.Done():
		return cleanup.Err()
	}
}

// Health includes calls blocked in cleanup.
func (r *Runtime) Health(ctx context.Context) (mesh.Health, error) {
	if err := ctx.Err(); err != nil {
		return mesh.Health{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health, nil
}

// Capabilities returns a synchronized value snapshot.
func (r *Runtime) Capabilities() mesh.Capabilities {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capabilities
}

// Counters returns exact synchronized totals.
func (r *Runtime) Counters() Counters { r.mu.Lock(); defer r.mu.Unlock(); return r.counters }

// Generate holds active ownership through cleanup and invokes emit synchronously.
// Gates make admission, first event, release, and cleanup observable without sleeps.
func (r *Runtime) Generate(ctx context.Context, _ mesh.Request, emit func(mesh.Event) error) (err error) {
	r.mu.Lock()
	if r.health.State != mesh.StateReady || r.stopped {
		r.mu.Unlock()
		return mesh.ErrUnavailable
	}
	if r.counters.Active == 0 {
		r.idle = make(chan struct{})
	}
	r.counters.Active++
	if r.counters.Active > r.counters.Peak {
		r.counters.Peak = r.counters.Active
	}
	r.health.Active = true
	session := r.session
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(session, cancel)
	defer stop()
	defer cancel()
	signal(r.Admitted)
	defer func() {
		signal(r.Cleaning)
		// Tests control cleanup independently of the canceled request, within a bound.
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := gate(cleanup, r.CleanupGate)
		cancelCleanup()
		if cleanupErr == nil {
			cleanupErr = r.CleanupError
		}
		err = errors.Join(err, cleanupErr)
		r.mu.Lock()
		r.counters.Active--
		r.health.Active = r.counters.Active != 0
		if cleanupErr != nil {
			r.health.State = mesh.StateUnhealthy
			r.health.LastError = cleanupErr.Error()
		}
		if r.counters.Active == 0 {
			close(r.idle)
		}
		r.mu.Unlock()
		signal(r.Released)
	}()
	if err = gate(ctx, r.AdmissionGate); err != nil {
		return err
	}
	if r.GenerateError != nil {
		return r.GenerateError
	}
	events := r.Events
	if events == nil {
		events = []mesh.Event{{Kind: mesh.EventStarted}, {Kind: mesh.EventCompleted, FinishReason: "stop"}}
	}
	for i, event := range events {
		if i == 0 {
			if err = gate(ctx, r.FirstEventGate); err != nil {
				return err
			}
			signal(r.FirstEvent)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		// Event values/usage are copied so callbacks may retain them safely.
		if event.InputTokens != nil {
			count := *event.InputTokens
			event.InputTokens = &count
		}
		if event.OutputTokens != nil {
			count := *event.OutputTokens
			event.OutputTokens = &count
		}
		if err = emit(event); err != nil {
			return err
		}
		if i == 0 {
			if err = gate(ctx, r.ReleaseGate); err != nil {
				return err
			}
		}
	}
	if len(events) == 0 {
		return gate(ctx, r.ReleaseGate)
	}
	return nil
}
func gate(ctx context.Context, ch <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
