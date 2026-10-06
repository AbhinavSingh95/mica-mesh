package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

// Private timing seams keep policy tests deterministic without exposing a clock framework.
type supervisorTiming struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

// RunRuntime owns startup/recovery/shutdown. Exhausted or permanent failures remain
// visible while membership continues, until ctx is canceled. Call exactly once.
func (s *Service) RunRuntime(ctx context.Context) error {
	return s.runRuntime(ctx, supervisorTiming{now: time.Now, wait: wait})
}
func (s *Service) runRuntime(ctx context.Context, timing supervisorTiming) (err error) {
	defer func() {
		s.mu.Lock()
		s.lifecycle = lifecycleStopping
		s.report.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY
		s.revision++
		s.mu.Unlock()
		err = errors.Join(err, s.stopRuntime())
	}()
	recoveries := 0
	for ctx.Err() == nil {
		s.mu.Lock()
		s.lifecycle = lifecycleStarting
		s.revision++
		s.mu.Unlock()
		s.setState(meshv1.RuntimeState_RUNTIME_STATE_STARTING, "")
		attempt, cancel := context.WithTimeout(ctx, 120*time.Second)
		failure := s.rt.Start(attempt, s.cfg)
		cancel()
		failureDetail := "runtime unavailable"
		if failure != nil {
			failureDetail = diagnostic(failure.Error())
		}
		if failure == nil {
			s.mu.Lock()
			s.lifecycle = lifecycleRunning
			s.revision++
			s.mu.Unlock()
			healthySince := timing.now()
			for ctx.Err() == nil {
				health, probeErr := s.refreshHealth(ctx)
				if probeErr != nil {
					failure = probeErr
					failureDetail = "runtime health check failed"
					if health.LastError != "" {
						failureDetail = diagnostic(health.LastError)
					}
					break
				}
				if health.State != mesh.StateReady {
					failure = mesh.ErrUnavailable
					if health.LastError != "" {
						failureDetail = diagnostic(health.LastError)
					}
					break
				}
				if timing.now().Sub(healthySince) >= 60*time.Second {
					recoveries = 0
				}
				if timing.wait(ctx, time.Second) != nil {
					break
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		s.mu.Lock()
		s.lifecycle = lifecycleRecovering
		s.revision++
		s.mu.Unlock()
		s.setState(meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, "runtime failed; recovery pending: "+failureDetail)
		if stopErr := s.stopRuntime(); stopErr != nil {
			s.setState(meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, "runtime cleanup failed; restart worker")
			_ = timing.wait(ctx, 0)
			return errors.Join(failure, stopErr)
		}
		if errors.Is(failure, mesh.ErrInvalidInput) || recoveries == 3 {
			diagnostic := "runtime recovery allowance exhausted; restart worker"
			if errors.Is(failure, mesh.ErrInvalidInput) {
				diagnostic = "runtime configuration or artifact invalid; correct configuration and restart worker"
			}
			s.setState(meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, diagnostic+": "+failureDetail)
			_ = timing.wait(ctx, 0)
			return nil
		}
		delay := time.Second << recoveries
		recoveries++
		if timing.wait(ctx, delay) != nil {
			return nil
		}
	}
	return nil
}

// stopRuntime cancels admission-owned work and observes its cleanup before Stop.
// Detached cleanup has a strict bound; admission stays closed through recovery/shutdown.
func (s *Service) stopRuntime() error {
	s.mu.Lock()
	cancelActive := s.activeCancel
	done := s.activeDone
	s.mu.Unlock()
	if cancelActive != nil {
		cancelActive()
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var waitErr error
	if done != nil {
		select {
		case <-done:
		case <-cleanup.Done():
			waitErr = fmt.Errorf("wait for inference cleanup: %w", cleanup.Err())
		}
	}
	stop, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	stopErr := s.rt.Stop(stop)
	if stopErr == nil {
		s.mu.Lock()
		s.runtimeActive = false
		s.updateActivityLocked()
		s.revision++
		s.mu.Unlock()
	}
	return errors.Join(waitErr, stopErr)
}

// A zero delay parks a permanently unhealthy worker until shutdown.
func wait(ctx context.Context, d time.Duration) error {
	if d == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
