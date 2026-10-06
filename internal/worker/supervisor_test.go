package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Crash after one successful health observation per process. Start/Stop/Generate
// and cleanup semantics still come from the designed fake runtime.
type crashingRuntime struct {
	*fakeruntime.Runtime
	mu        sync.Mutex
	checks    int
	stable    bool
	stopped   chan int
	stopCount int
}

func (r *crashingRuntime) Start(ctx context.Context, cfg mesh.Config) error {
	r.mu.Lock()
	r.checks = 0
	r.mu.Unlock()
	return r.Runtime.Start(ctx, cfg)
}
func (r *crashingRuntime) Health(ctx context.Context) (mesh.Health, error) {
	r.mu.Lock()
	r.checks++
	checks := r.checks
	stable := r.stable
	r.mu.Unlock()
	if checks > 1 && (!stable || r.Counters().Starts != 2 || checks > 62) {
		return mesh.Health{State: mesh.StateUnhealthy, LastError: "crash"}, nil
	}
	return r.Runtime.Health(ctx)
}
func (r *crashingRuntime) Stop(ctx context.Context) error {
	err := r.Runtime.Stop(ctx)
	r.mu.Lock()
	r.stopCount++
	n := r.stopCount
	r.mu.Unlock()
	r.stopped <- n
	return err
}
func TestRecoveryAllowance(t *testing.T)            { testRecovery(t, false) }
func TestStableRuntimeResetsAllowance(t *testing.T) { testRecovery(t, true) }
func testRecovery(t *testing.T, stable bool) {
	t.Helper()
	r := &crashingRuntime{Runtime: fakeruntime.New(), stable: stable, stopped: make(chan int, 16)}
	s := New(workerID, config(), &meshv1.HardwareInfo{}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	now := time.Unix(1, 0)
	var recoveries []time.Duration
	timing := supervisorTiming{now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }, wait: func(ctx context.Context, d time.Duration) error {
		if d == 0 {
			<-ctx.Done()
			return ctx.Err()
		}
		mu.Lock()
		now = now.Add(d)
		if s.Report().RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY && d > 0 {
			recoveries = append(recoveries, d)
		}
		mu.Unlock()
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { done <- s.runRuntime(ctx, timing) }()
	target := 4
	if stable {
		target = 5
	}
	for i := 0; i < target; i++ {
		select {
		case <-r.stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("runtime failure not supervised")
		}
	}
	cancel()
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if stable {
		if r.Counters().Starts < 5 {
			t.Errorf("allowance not reset: starts=%d", r.Counters().Starts)
		}
	} else {
		if r.Counters().Starts != 4 {
			t.Errorf("starts=%d want initial+3", r.Counters().Starts)
		}
		if len(recoveries) != 3 || recoveries[0] != time.Second || recoveries[1] != 2*time.Second || recoveries[2] != 4*time.Second {
			t.Errorf("delays=%v", recoveries)
		}
	}
}
func TestPermanentStartErrorDoesNotRetry(t *testing.T) {
	rt := fakeruntime.New()
	rt.StartError = mesh.ErrInvalidInput
	s := New(workerID, config(), &meshv1.HardwareInfo{}, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	park := make(chan struct{}, 1)
	timing := supervisorTiming{now: time.Now, wait: func(ctx context.Context, _ time.Duration) error { park <- struct{}{}; <-ctx.Done(); return ctx.Err() }}
	done := make(chan error, 1)
	go func() { done <- s.runRuntime(ctx, timing) }()
	signalWait(t, rt.Started)
	signalWait(t, park)
	if rt.Counters().Starts != 1 || s.Report().RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY {
		t.Error("permanent failure retried or hidden")
	}
	cancel()
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
}
func TestSupervisorStopWaitsForCleanup(t *testing.T) {
	rt := fakeruntime.New()
	release := make(chan struct{})
	cleanup := make(chan struct{})
	rt.ReleaseGate = release
	rt.CleanupGate = cleanup
	s := New(workerID, config(), &meshv1.HardwareInfo{}, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	park := make(chan struct{}, 1)
	timing := supervisorTiming{now: time.Now, wait: func(ctx context.Context, _ time.Duration) error { park <- struct{}{}; <-ctx.Done(); return ctx.Err() }}
	done := make(chan error, 1)
	go func() { done <- s.runRuntime(ctx, timing) }()
	signalWait(t, park)
	gen := make(chan error, 1)
	go func() { gen <- generate(t, s, request(), &stream{ctx: context.Background()}) }()
	signalWait(t, rt.Admitted)
	cancel()
	signalWait(t, rt.Cleaning)
	select {
	case <-done:
		t.Error("shutdown finished during cleanup")
	default:
	}
	if !s.Report().Active {
		t.Error("shutdown lost ownership")
	}
	if code := status.Code(generate(t, s, request(), &stream{ctx: context.Background()})); code != codes.ResourceExhausted {
		t.Errorf("overlap=%v", code)
	}
	close(cleanup)
	if status.Code(resultWait(t, gen)) != codes.Canceled {
		t.Error("shutdown did not cancel generation")
	}
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
	if rt.Counters().Active != 0 || rt.Counters().Stops != 1 {
		t.Error("runtime not stopped")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("test context")
	}
}

type failureRuntime struct {
	*fakeruntime.Runtime
	mu       sync.Mutex
	fail     bool
	stopping chan struct{}
	stopGate chan struct{}
}

func (r *failureRuntime) Health(ctx context.Context) (mesh.Health, error) {
	r.mu.Lock()
	fail := r.fail
	r.fail = false
	r.mu.Unlock()
	if fail {
		return mesh.Health{State: mesh.StateUnhealthy}, nil
	}
	return r.Runtime.Health(ctx)
}
func (r *failureRuntime) Stop(ctx context.Context) error {
	select {
	case r.stopping <- struct{}{}:
	default:
	}
	select {
	case <-r.stopGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Runtime.Stop(ctx)
}
func TestRecoveryDoesNotReopenAdmission(t *testing.T) {
	rt := fakeruntime.New()
	release := make(chan struct{})
	cleanup := make(chan struct{})
	rt.ReleaseGate = release
	rt.CleanupGate = cleanup
	r := &failureRuntime{Runtime: rt, stopping: make(chan struct{}, 1), stopGate: make(chan struct{})}
	s := New(workerID, config(), &meshv1.HardwareInfo{}, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	poll := make(chan struct{}, 1)
	tick := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.runRuntime(ctx, supervisorTiming{now: time.Now, wait: func(ctx context.Context, d time.Duration) error {
			select {
			case poll <- struct{}{}:
			default:
			}
			select {
			case <-tick:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	}()
	signalWait(t, poll)
	gen := make(chan error, 1)
	go func() { gen <- generate(t, s, request(), &stream{ctx: context.Background()}) }()
	signalWait(t, rt.Admitted)
	r.mu.Lock()
	r.fail = true
	r.mu.Unlock()
	tick <- struct{}{}
	signalWait(t, rt.Cleaning)
	close(cleanup)
	resultWait(t, gen)
	signalWait(t, r.stopping)
	close(release)
	if code := status.Code(generate(t, s, request(), &stream{ctx: context.Background()})); code != codes.Unavailable {
		t.Errorf("admission reopened during runtime stop: %v", code)
	}
	cancel()
	close(r.stopGate)
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
}
