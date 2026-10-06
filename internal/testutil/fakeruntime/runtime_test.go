package fakeruntime

import (
	"context"
	"errors"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"testing"
	"time"
)

var _ mesh.Runtime = (*Runtime)(nil)

func wait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("fake coordination timed out")
		var zero T
		return zero
	}
}
func TestLifecycleGatesAndCounters(t *testing.T) {
	r := New()
	gate := make(chan struct{})
	r.StartGate = gate
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx, mesh.Config{Model: mesh.Model{ID: "fake"}}) }()
	select {
	case err := <-done:
		t.Fatalf("Start returned before gate: %v", err)
	case <-r.Started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	h, _ := r.Health(ctx)
	if h.State != mesh.StateStarting {
		t.Fatalf("start health = %+v", h)
	}
	close(gate)
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	c := r.Counters()
	if c.Starts != 1 || c.Stops != 1 {
		t.Fatalf("counters = %+v", c)
	}
}
func TestCancellationRetainsActiveThroughCleanup(t *testing.T) {
	r := New()
	if err := r.Start(context.Background(), mesh.Config{}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	cleanup := make(chan struct{})
	r.ReleaseGate = release
	r.CleanupGate = cleanup
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Generate(ctx, mesh.Request{}, func(mesh.Event) error { return nil }) }()
	select {
	case <-r.Admitted:
	case err := <-done:
		t.Fatalf("Generate returned before release: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no admission")
	}
	cancel()
	wait(t, r.Cleaning)
	h, _ := r.Health(context.Background())
	c := r.Counters()
	if !h.Active || c.Active != 1 || c.Peak != 1 {
		t.Fatalf("cleanup health/counters = %+v/%+v", h, c)
	}
	close(cleanup)
	if err := wait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Generate = %v", err)
	}
	wait(t, r.Released)
	if r.Counters().Active != 0 {
		t.Fatal("active not released")
	}
}
func TestEventsAndCallbackFailure(t *testing.T) {
	r := New()
	r.Start(context.Background(), mesh.Config{})
	sentinel := errors.New("downstream failed")
	calls := 0
	err := r.Generate(context.Background(), mesh.Request{}, func(event mesh.Event) error {
		calls++
		if event.Kind != mesh.EventStarted {
			t.Fatalf("first event = %+v", event)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("callback result = %v, calls %d", err, calls)
	}
}
func TestFailedStartAndCleanupLeaveUnhealthy(t *testing.T) {
	r := New()
	r.StartError = mesh.ErrInvalidInput
	if err := r.Start(context.Background(), mesh.Config{}); !errors.Is(err, mesh.ErrInvalidInput) {
		t.Fatalf("Start = %v", err)
	}
	h, _ := r.Health(context.Background())
	if h.State != mesh.StateUnhealthy {
		t.Fatalf("failed start = %+v", h)
	}
	r.StartError = nil
	r.Start(context.Background(), mesh.Config{})
	r.CleanupError = mesh.ErrUnavailable
	if err := r.Generate(context.Background(), mesh.Request{}, func(mesh.Event) error { return nil }); !errors.Is(err, mesh.ErrUnavailable) {
		t.Fatalf("Generate = %v", err)
	}
	h, _ = r.Health(context.Background())
	if h.State != mesh.StateUnhealthy || h.Active {
		t.Fatalf("failed cleanup = %+v", h)
	}
}
func TestReleaseGateNeedsOnlyOneSignal(t *testing.T) {
	r := New()
	r.Start(context.Background(), mesh.Config{})
	release := make(chan struct{})
	r.ReleaseGate = release
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Generate(ctx, mesh.Request{}, func(mesh.Event) error { return nil }) }()
	wait(t, r.FirstEvent)
	release <- struct{}{}
	if err := wait(t, done); err != nil {
		t.Fatalf("one release signal did not finish generation: %v", err)
	}
}
func TestStopWaitsForCleanupAndRejectsRestart(t *testing.T) {
	r := New()
	r.Start(context.Background(), mesh.Config{})
	r.ReleaseGate = make(chan struct{})
	cleanup := make(chan struct{})
	r.CleanupGate = cleanup
	generation := make(chan error, 1)
	go func() {
		generation <- r.Generate(context.Background(), mesh.Request{}, func(mesh.Event) error { return nil })
	}()
	wait(t, r.FirstEvent)
	stopped := make(chan error, 1)
	go func() { stopped <- r.Stop(context.Background()) }()
	wait(t, r.Cleaning)
	restartErr := r.Start(context.Background(), mesh.Config{})
	if !errors.Is(restartErr, mesh.ErrUnavailable) {
		t.Errorf("restart while old cleanup owns work = %v", restartErr)
	}
	select {
	case err := <-stopped:
		t.Errorf("Stop returned before cleanup: %v", err)
	default:
	}
	close(cleanup)
	if err := wait(t, stopped); err != nil {
		t.Error(err)
	}
	if err := wait(t, generation); !errors.Is(err, context.Canceled) {
		t.Errorf("generation after Stop = %v", err)
	}
}
