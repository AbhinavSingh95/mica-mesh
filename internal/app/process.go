package app

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/worker"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
)

// NetworkMode selects LAN discovery or explicit loopback connections.
type NetworkMode int

const (
	LAN NetworkMode = iota
	Local
)

// Options controls network policy. ResolveController runs inside the membership
// owner's four-second lookup context. It must return promptly on cancellation;
// a guided resolver publishes choices and returns instead of waiting for input.
// Local bypasses this override and all LAN discovery effects.
type Options struct {
	Network           NetworkMode
	ResolveController func(context.Context, string) (string, error)
}

// AgentStatus is a copied local snapshot. Runtime and membership are independent.
type AgentStatus struct {
	WorkerID, Hostname, Endpoint string
	Report                       *meshv1.WorkerReport
	Membership                   worker.MembershipStatus
}

// Process owns the services started by Start. Close or parent cancellation stops
// admission/transports, joins every owner and reaps the runtime before completion.
// Identity and service pointers are immutable once Start returns.
type Process struct {
	cancel            context.CancelFunc
	done              chan struct{}
	err               error // Written once before done closes; readers first wait for done.
	controllerAddress string
	worker            *worker.Service
	workerID          string
	hostname          string
	mu                sync.Mutex
	endpoint          string
	membership        worker.MembershipStatus
}

// ControllerAddress returns the actual endpoint, or empty for an Agent-only role.
func (p *Process) ControllerAddress() string { return p.controllerAddress }

// AgentStatus returns false when this process has no Agent. No network work or
// worker lock is held while the membership snapshot is protected.
func (p *Process) AgentStatus() (AgentStatus, bool) {
	if p.worker == nil {
		return AgentStatus{}, false
	}
	p.mu.Lock()
	s := AgentStatus{WorkerID: p.workerID, Hostname: p.hostname, Endpoint: p.endpoint, Membership: p.membership}
	p.mu.Unlock()
	s.Report = p.worker.Report()
	return s, true
}
func (p *Process) observeMembership(s worker.MembershipStatus) {
	p.mu.Lock()
	p.membership = s
	p.mu.Unlock()
}
func (p *Process) setEndpoint(endpoint string) { p.mu.Lock(); p.endpoint = endpoint; p.mu.Unlock() }

// Wait joins completion. Concurrent Wait and Close callers see the same result.
func (p *Process) Wait() error { <-p.done; return p.err }

// Close is idempotent. It requests shutdown and joins all owned work.
func (p *Process) Close() error { p.cancel(); return p.Wait() }

func (p *Process) finish(ctx context.Context, results <-chan error, owners int, servers []*grpc.Server, listeners []net.Listener, svc *controller.Service, stopAdvertisement func()) {
	var result error
	select {
	case <-ctx.Done():
	case failure := <-results:
		owners--
		if ctx.Err() == nil {
			if failure == nil {
				failure = errors.New("mesh owner stopped unexpectedly")
			}
			result = failure
		} else {
			result = failure
		}
	}
	p.cancel()
	// Stop transports before joining owners to unblock handlers and flow control.
	// Owned listeners retain their first close result, including a close by gRPC.
	result = errors.Join(result, closeListeners(listeners...))
	for _, server := range servers {
		server.Stop()
	}
	for owners > 0 {
		failure := <-results
		owners--
		result = errors.Join(result, failure)
	}
	if stopAdvertisement != nil {
		stopAdvertisement()
	}
	if svc != nil {
		result = errors.Join(result, svc.Close())
	}
	p.err = result
	close(p.done)
}
func closeListeners(listeners ...net.Listener) error {
	var result error
	for _, listener := range listeners {
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

// serve suppresses expected transport shutdown only at the server boundary.
// Runtime and membership cleanup errors must reach the process unchanged.
func serve(ctx context.Context, server *grpc.Server, listener net.Listener) error {
	err := server.Serve(listener)
	if ctx.Err() != nil && (errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed)) {
		return nil
	}
	return err
}

// ownedListener shares one Close operation and result between gRPC and the
// process coordinator. gRPC discards Close errors, including after fatal Accept;
// retaining the first result lets the coordinator report that cleanup failure.
type ownedListener struct {
	net.Listener
	closeOnce sync.Once
	closeErr  error
}

func (l *ownedListener) Close() error {
	l.closeOnce.Do(func() { l.closeErr = l.Listener.Close() })
	return l.closeErr
}
