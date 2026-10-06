package worker

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type controlServer struct {
	meshv1.UnimplementedControllerServiceServer
	mu                   sync.Mutex
	registrationFailures int
	unknown              bool
	registered           chan *meshv1.RegisterWorkerRequest
	heartbeat            chan *meshv1.HeartbeatRequest
	invalid              bool
}

func (c *controlServer) RegisterWorker(ctx context.Context, r *meshv1.RegisterWorkerRequest) (*meshv1.RegisterWorkerResponse, error) {
	select {
	case c.registered <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registrationFailures > 0 {
		c.registrationFailures--
		return nil, status.Error(codes.Unavailable, "offline")
	}
	id := "00000000-0000-4000-8000-000000000003"
	if c.invalid {
		id = "bad"
	}
	return &meshv1.RegisterWorkerResponse{ControllerId: id, HeartbeatIntervalSeconds: 2, MembershipExpirySeconds: 10}, nil
}
func (c *controlServer) Heartbeat(ctx context.Context, r *meshv1.HeartbeatRequest) (*meshv1.HeartbeatResponse, error) {
	select {
	case c.heartbeat <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.unknown {
		c.unknown = false
		return nil, status.Error(codes.NotFound, "expired")
	}
	return &meshv1.HeartbeatResponse{}, nil
}
func controllerServer(t *testing.T, c *controlServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
	meshv1.RegisterControllerServiceServer(server, c)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); resultWait(t, done) })
	return listener.Addr().String()
}
func control() *controlServer {
	return &controlServer{registered: make(chan *meshv1.RegisterWorkerRequest, 16), heartbeat: make(chan *meshv1.HeartbeatRequest, 16)}
}

type membershipRun struct {
	cancel context.CancelFunc
	done   chan error
	waits  chan time.Duration
	ticks  chan struct{}
}

func member(t *testing.T, s *Service, c *controlServer) *membershipRun {
	t.Helper()
	addr := controllerServer(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	r := &membershipRun{cancel: cancel, done: make(chan error, 1), waits: make(chan time.Duration, 16), ticks: make(chan struct{})}
	timing := membershipTiming{jitter: func(d time.Duration) time.Duration { return d }, wait: func(ctx context.Context, d time.Duration) error {
		select {
		case r.waits <- d:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-r.ticks:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	go func() {
		r.done <- runMembership(ctx, s, "127.0.0.1:12345", func(context.Context) (string, error) { return addr, nil }, nil, timing)
	}()
	t.Cleanup(func() {
		cancel()
		if err := resultWait(t, r.done); err != nil {
			t.Error(err)
		}
	})
	return r
}
func registration(t *testing.T, c *controlServer) *meshv1.RegisterWorkerRequest {
	t.Helper()
	select {
	case r := <-c.registered:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no registration")
		return nil
	}
}
func heartbeat(t *testing.T, c *controlServer) *meshv1.HeartbeatRequest {
	t.Helper()
	select {
	case r := <-c.heartbeat:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat")
		return nil
	}
}
func tick(t *testing.T, r *membershipRun, want time.Duration) {
	t.Helper()
	select {
	case got := <-r.waits:
		if got != want {
			t.Errorf("wait=%v want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no membership wait")
	}
	select {
	case r.ticks <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("membership wait not cancellable")
	}
}
func TestHeartbeatDuringRuntimeLoading(t *testing.T) {
	rt := fakeruntime.New()
	start := make(chan struct{})
	rt.StartGate = start
	s := New(workerID, config(), &meshv1.HardwareInfo{Hostname: "host"}, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.RunRuntime(ctx) }()
	signalWait(t, rt.Started)
	c := control()
	r := member(t, s, c)
	reg := registration(t, c)
	if reg.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_STARTING || reg.Model.ContextTokens != 4096 || reg.Capacity != 1 {
		t.Errorf("registration=%v", reg)
	}
	tick(t, r, 2*time.Second)
	if hb := heartbeat(t, c); hb.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_STARTING {
		t.Errorf("heartbeat=%v", hb)
	}
	cancel()
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
}
func TestUnknownWorkerReregisters(t *testing.T) {
	rt := fakeruntime.New()
	s := New(workerID, config(), &meshv1.HardwareInfo{}, rt)
	c := control()
	c.unknown = true
	r := member(t, s, c)
	first := registration(t, c)
	tick(t, r, 2*time.Second)
	heartbeat(t, c)
	second := registration(t, c)
	if first.WorkerId != second.WorkerId || first.Endpoint != second.Endpoint {
		t.Fatal("identity changed after registry loss")
	}
}

func TestRuntimeVersionRefreshesAfterLoading(t *testing.T) {
	rt := fakeruntime.New()
	start := make(chan struct{})
	rt.StartGate = start
	s := New(workerID, config(), &meshv1.HardwareInfo{Hostname: "host"}, rt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.RunRuntime(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := resultWait(t, done); err != nil {
			t.Error(err)
		}
	})
	signalWait(t, rt.Started)
	c := control()
	r := member(t, s, c)
	first := registration(t, c)
	if first.RuntimeVersion != "unknown" {
		t.Fatalf("loading version=%q", first.RuntimeVersion)
	}
	close(start)
	// Start publishes its verified capabilities before readiness; its owner is
	// allowed to finish asynchronously without a guessed pre-start version.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for rt.Capabilities().RuntimeVersion == "" {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("startup did not publish capabilities")
		}
	}
	tick(t, r, 2*time.Second)
	updated := registration(t, c)
	if updated.RuntimeVersion != "fake" || updated.WorkerId != first.WorkerId || updated.Endpoint != first.Endpoint {
		t.Fatalf("metadata refresh=%v", updated)
	}
	tick(t, r, 2*time.Second)
	heartbeat(t, c)
	select {
	case extra := <-c.registered:
		t.Fatalf("unchanged capabilities registered again: %v", extra)
	default:
	}
	if rt.Counters().Starts != 1 {
		t.Fatal("metadata refresh restarted runtime")
	}
}
func TestRegistrationRetryUsesSameProcessID(t *testing.T) {
	c := control()
	c.registrationFailures = 2
	s := New(workerID, config(), &meshv1.HardwareInfo{}, fakeruntime.New())
	r := member(t, s, c)
	first := registration(t, c)
	tick(t, r, time.Second)
	second := registration(t, c)
	tick(t, r, 2*time.Second)
	third := registration(t, c)
	if first.WorkerId != workerID || second.WorkerId != workerID || third.WorkerId != workerID {
		t.Fatal("registration retry changed process identity")
	}
}
func TestMembershipFailureDoesNotRestartRuntime(t *testing.T) {
	rt := fakeruntime.New()
	s := New(workerID, config(), &meshv1.HardwareInfo{}, rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	park := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.runRuntime(ctx, supervisorTiming{now: time.Now, wait: func(ctx context.Context, _ time.Duration) error { park <- struct{}{}; <-ctx.Done(); return ctx.Err() }})
	}()
	signalWait(t, park)
	c := control()
	c.registrationFailures = 3
	r := member(t, s, c)
	registration(t, c)
	tick(t, r, time.Second)
	registration(t, c)
	tick(t, r, 2*time.Second)
	registration(t, c)
	if rt.Counters().Starts != 1 || s.Report().RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_READY {
		t.Fatal("controller outage changed runtime")
	}
	cancel()
	if err := resultWait(t, done); err != nil {
		t.Error(err)
	}
}
func TestInvalidRegistrationResponseRetries(t *testing.T) {
	c := control()
	c.invalid = true
	s := New(workerID, config(), &meshv1.HardwareInfo{}, fakeruntime.New())
	r := member(t, s, c)
	registration(t, c)
	tick(t, r, time.Second)
	registration(t, c)
	select {
	case <-c.heartbeat:
		t.Fatal("invalid response accepted")
	default:
	}
}

type diagnosticRuntime struct{ *fakeruntime.Runtime }

func (r *diagnosticRuntime) Health(context.Context) (mesh.Health, error) {
	return mesh.Health{State: mesh.StateUnhealthy, LastError: strings.Repeat("x", 65536) + "\xff"}, nil
}
func TestRuntimeDiagnosticsFitMembershipRPC(t *testing.T) {
	r := &diagnosticRuntime{Runtime: fakeruntime.New()}
	s := New(workerID, config(), &meshv1.HardwareInfo{}, r)
	s.refreshHealth(context.Background())
	c := control()
	member(t, s, c)
	reg := registration(t, c)
	if reg.Report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY || len(reg.Report.LastError) > 4096 || !utf8.ValidString(reg.Report.LastError) {
		t.Fatal("unhealthy runtime diagnostics broke membership boundary")
	}
}

func TestReconnectJitterHonorsBounds(t *testing.T) {
	for _, tc := range []struct {
		base    time.Duration
		maximum bool
		want    time.Duration
	}{{time.Second, false, time.Second}, {time.Second, true, 1500 * time.Millisecond}, {8 * time.Second, false, 8 * time.Second}, {8 * time.Second, true, 10 * time.Second}, {10 * time.Second, false, 10 * time.Second}, {10 * time.Second, true, 10 * time.Second}} {
		got := membershipJitter(tc.base, func(n int64) int64 {
			if tc.maximum {
				return n - 1
			}
			return 0
		})
		if got != tc.want {
			t.Errorf("retry base=%v maximum=%v got=%v want=%v", tc.base, tc.maximum, got, tc.want)
		}
	}
}

func TestMembershipStatusRequiresRegistration(t *testing.T) {
	c := control()
	c.registrationFailures = 1
	c.unknown = true
	addr := controllerServer(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := make(chan MembershipStatus, 32)
	ticks := make(chan struct{})
	waits := make(chan time.Duration, 8)
	timing := membershipTiming{jitter: func(d time.Duration) time.Duration { return d }, wait: func(ctx context.Context, d time.Duration) error {
		waits <- d
		select {
		case <-ticks:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	done := make(chan error, 1)
	svc := New(workerID, config(), &meshv1.HardwareInfo{}, fakeruntime.New())
	go func() {
		done <- runMembership(ctx, svc, "127.0.0.1:12345", func(context.Context) (string, error) { return addr, nil }, func(s MembershipStatus) { observed <- s }, timing)
	}()
	t.Cleanup(func() {
		cancel()
		if err := resultWait(t, done); err != nil {
			t.Error(err)
		}
	})
	state := func(want bool) {
		t.Helper()
		select {
		case s := <-observed:
			if s.Registered != want || s.ControllerAddress != addr {
				t.Fatalf("membership=%+v want registered=%v", s, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing membership transition")
		}
	}
	registration(t, c)
	select {
	case <-waits:
	case <-time.After(time.Second):
		t.Fatal("retry not waiting")
	}
	state(false)
	ticks <- struct{}{}
	registration(t, c)
	// Address refresh may publish waiting again. Consume until registration proof.
	for {
		select {
		case s := <-observed:
			if s.Registered {
				goto registered
			}
		case <-time.After(time.Second):
			t.Fatal("registration not observed")
		}
	}
registered:
	select {
	case <-waits:
	case <-time.After(time.Second):
		t.Fatal("heartbeat not waiting")
	}
	ticks <- struct{}{}
	heartbeat(t, c)
	state(false)
	registration(t, c)
	state(true)
	cancel()
	// Final callback clears the connected state and retains the attempted address.
	state(false)
}
