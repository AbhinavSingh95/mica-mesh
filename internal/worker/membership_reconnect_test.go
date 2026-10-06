package worker

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
)

// outageListener controls only the network boundary. Rejected real TCP
// connections advance the actual gRPC transport's independent backoff.
type outageListener struct {
	net.Listener
	mu        sync.Mutex
	available bool
	active    []net.Conn
	rejected  chan struct{}
	failures  atomic.Int32
	holdNext  bool
	held      chan struct{}
}

func (l *outageListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		available := l.available
		hold := l.holdNext
		l.holdNext = false
		if available || hold {
			l.active = append(l.active, conn)
		}
		l.mu.Unlock()
		if hold {
			l.held <- struct{}{}
			continue
		}
		if available {
			return conn, nil
		}
		l.failures.Add(1)
		conn.Close()
		select {
		case l.rejected <- struct{}{}:
		default:
		}
	}
}
func (l *outageListener) setAvailable(available bool) {
	l.mu.Lock()
	l.available = available
	var active []net.Conn
	if !available {
		active, l.active = l.active, nil
	}
	l.mu.Unlock()
	for _, conn := range active {
		conn.Close()
	}
}
func gatedController(t *testing.T, available bool) (*controlServer, *outageListener) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &outageListener{Listener: listener, available: available, rejected: make(chan struct{}, 16), held: make(chan struct{}, 1)}
	c := control()
	g := grpc.NewServer(protocol.GenerationServerOption())
	meshv1.RegisterControllerServiceServer(g, c)
	done := make(chan error, 1)
	go func() { done <- g.Serve(l) }()
	t.Cleanup(func() { l.setAvailable(false); g.Stop(); resultWait(t, done) })
	return c, l
}

func TestRegistrationRetryOverridesAccumulatedTransportBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, listener := gatedController(t, true)
	rt := fakeruntime.New()
	svc := New(workerID, config(), &meshv1.HardwareInfo{Hostname: "rejoin"}, rt)
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	runtimeDone := make(chan error, 1)
	runtimeReady := make(chan struct{}, 1)
	go func() {
		runtimeDone <- svc.runRuntime(runtimeCtx, supervisorTiming{now: time.Now, wait: func(ctx context.Context, _ time.Duration) error {
			runtimeReady <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}})
	}()
	t.Cleanup(func() {
		stopRuntime()
		if err := resultWait(t, runtimeDone); err != nil {
			t.Error(err)
		}
	})
	signalWait(t, runtimeReady)
	memberCtx, stopMember := context.WithCancel(ctx)
	r := &membershipRun{cancel: stopMember, done: make(chan error, 1), waits: make(chan time.Duration, 16), ticks: make(chan struct{})}
	timing := membershipTiming{jitter: func(d time.Duration) time.Duration { return d }, wait: func(ctx context.Context, d time.Duration) error {
		if listener.failures.Load() < 4 {
			// Accelerate the application clock while preserving real network
			// backoff. Keep retries paced; no spin or sleep guesses a phase.
			return wait(ctx, time.Second)
		}
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
	joined := make(chan struct{}, 2)
	go func() {
		r.done <- runMembership(memberCtx, svc, "127.0.0.1:12345", func(context.Context) (string, error) { return listener.Addr().String(), nil }, func(s MembershipStatus) {
			if s.Registered {
				joined <- struct{}{}
			}
		}, timing)
	}()
	t.Cleanup(func() {
		stopMember()
		if err := resultWait(t, r.done); err != nil {
			t.Error(err)
		}
	})
	first := registration(t, c)
	signalWait(t, joined)
	// Park membership only after four actual transport failures. Before
	// then its attempts keep the retained channel active through backoff.
	listener.setAvailable(false)
	for range 4 {
		select {
		case <-listener.rejected:
		case <-ctx.Done():
			t.Fatal("transport did not enter accumulated backoff")
		}
	}
	select {
	case delay := <-r.waits:
		if delay < time.Second || delay > 10*time.Second {
			t.Fatalf("membership retry escaped its bounds: %v", delay)
		}
	case <-ctx.Done():
		t.Fatal("missing application retry")
	}
	listener.setAvailable(true)
	// Expire the application's own wait. The next bounded registration must
	// reconnect, not fail fast and park behind another independent timer.
	select {
	case r.ticks <- struct{}{}:
	case <-ctx.Done():
		t.Fatal("retry did not resume")
	}
	second := registration(t, c)
	signalWait(t, joined)
	if second.WorkerId != first.WorkerId || second.Endpoint != first.Endpoint {
		t.Fatal("reconnection changed Agent identity")
	}
	if got := rt.Counters(); got.Starts != 1 || got.Stops != 0 {
		t.Fatalf("membership outage changed runtime ownership: %+v", got)
	}
}

func TestMembershipCancellationJoinsPendingReconnect(t *testing.T) {
	_, listener := gatedController(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := New(workerID, config(), &meshv1.HardwareInfo{}, fakeruntime.New())
	done := make(chan error, 1)
	go func() {
		done <- RunMembership(ctx, svc, "127.0.0.1:12345", func(context.Context) (string, error) { return listener.Addr().String(), nil }, nil)
	}()
	select {
	case <-listener.rejected:
	case <-time.After(2 * time.Second):
		cancel()
		resultWait(t, done)
		t.Fatal("connection attempt did not reach listener")
	}
	cancel()
	if err := resultWait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestMembershipRecoversFromStalledHandshake(t *testing.T) {
	c, listener := gatedController(t, true)
	listener.mu.Lock()
	listener.holdNext = true
	listener.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc := New(workerID, config(), &meshv1.HardwareInfo{}, fakeruntime.New())
	done := make(chan error, 1)
	go func() {
		done <- RunMembership(ctx, svc, "127.0.0.1:12345", func(context.Context) (string, error) { return listener.Addr().String(), nil }, nil)
	}()
	t.Cleanup(func() {
		cancel()
		if err := resultWait(t, done); err != nil {
			t.Error(err)
		}
	})
	// The accepted socket withholds HTTP/2 headers. All later sockets reach
	// the real server immediately; the old connection must not hide it for
	// gRPC's default 20-second minimum connection timeout.
	select {
	case <-listener.held:
	case <-ctx.Done():
		t.Fatal("initial connection was not held")
	}
	select {
	case request := <-c.registered:
		if request.WorkerId != workerID {
			t.Fatal("stalled connection changed Agent identity")
		}
	case <-ctx.Done():
		t.Fatal("reachable Controller did not receive registration within 15 seconds")
	}
}
