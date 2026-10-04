// Package integration exercises the real mesh transports without a model or LAN.
package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	mesh "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	"github.com/AbhinavSingh95/mica-mesh/internal/worker"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var model = mesh.Model{ID: "integration-model", SHA256: strings.Repeat("a", 64), ContextTokens: 2048}

type gate struct {
	ch   chan struct{}
	once sync.Once
}

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// Runtime gates live only at the designed external boundary. Generate remains
// synchronous, so neither the fixture nor the mesh can hide a token queue.
type runtimeControl struct {
	*fakeruntime.Runtime
	deltaGate   <-chan struct{}
	stopGate    <-chan struct{}
	stopping    chan struct{}
	crashed     atomic.Bool
	holdVersion atomic.Bool
	mu          sync.Mutex
	requests    []string
}

func newRuntime() *runtimeControl {
	r := &runtimeControl{Runtime: fakeruntime.New(), stopping: make(chan struct{}, 1)}
	r.Events = []mesh.Event{{Kind: mesh.EventStarted}, {Kind: mesh.EventTextDelta, Text: "partial 🌏"}, {Kind: mesh.EventCompleted, FinishReason: "stop"}}
	return r
}
func (r *runtimeControl) Generate(ctx context.Context, req mesh.Request, emit func(mesh.Event) error) error {
	r.mu.Lock()
	r.requests = append(r.requests, req.ID)
	r.mu.Unlock()
	return r.Runtime.Generate(ctx, req, func(e mesh.Event) error {
		if err := emit(e); err != nil {
			return err
		}
		if e.Kind == mesh.EventTextDelta && r.deltaGate != nil {
			select {
			case <-r.deltaGate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
}
func (r *runtimeControl) attempts() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.requests) }
func (r *runtimeControl) Health(ctx context.Context) (mesh.Health, error) {
	if r.crashed.Load() {
		return mesh.Health{State: mesh.StateUnhealthy}, mesh.ErrUnavailable
	}
	return r.Runtime.Health(ctx)
}
func (r *runtimeControl) Stop(ctx context.Context) error {
	select {
	case r.stopping <- struct{}{}:
	default:
	}
	if r.stopGate != nil {
		select {
		case <-r.stopGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := r.Runtime.Stop(ctx); err != nil {
		return err
	}
	r.crashed.Store(false)
	return nil
}
func (r *runtimeControl) Capabilities() mesh.Capabilities {
	caps := r.Runtime.Capabilities()
	if r.holdVersion.Load() {
		caps.RuntimeVersion = ""
	}
	return caps
}

type rpcServer struct {
	g       *grpc.Server
	address string
	done    chan error
	once    sync.Once
}
type workerProcess struct {
	id           string
	svc          *worker.Service
	rt           *runtimeControl
	rpc          *rpcServer
	cancel       context.CancelFunc
	done         chan error
	memberCancel context.CancelFunc
	memberDone   chan error
	once         sync.Once
}
type controllerProcess struct {
	svc    *controller.Service
	reg    *controller.Registry
	rpc    *rpcServer
	client meshv1.ControllerServiceClient
	conn   *grpc.ClientConn
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}
type fixture struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	gates       []*gate
	workers     []*workerProcess
	controllers []*controllerProcess
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f := &fixture{t: t, ctx: ctx, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		for _, gate := range f.gates {
			gate.open()
		}
		for _, c := range f.controllers {
			f.stopController(c)
		}
		for _, w := range f.workers {
			f.stopWorker(w)
		}
	})
	return f
}
func (f *fixture) gate() *gate {
	g := &gate{ch: make(chan struct{})}
	f.gates = append(f.gates, g)
	return g
}
func (f *fixture) server(address string, register func(*grpc.Server)) *rpcServer {
	f.t.Helper()
	if address == "" {
		address = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp4", address)
	if err != nil {
		f.t.Fatal(err)
	}
	g := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
	register(g)
	r := &rpcServer{g: g, address: l.Addr().String(), done: make(chan error, 1)}
	go func() { r.done <- g.Serve(l) }()
	return r
}
func (f *fixture) join(done <-chan error) {
	f.t.Helper()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			f.t.Error(err)
		}
	case <-time.After(8 * time.Second):
		f.t.Error("owned operation did not terminate")
	}
}
func (f *fixture) stopRPC(r *rpcServer) { r.once.Do(func() { r.g.Stop(); f.join(r.done) }) }
func (f *fixture) worker(rt *runtimeControl) *workerProcess {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	w := &workerProcess{id: uuid.NewString(), rt: rt, cancel: cancel, done: make(chan error, 1)}
	w.svc = worker.New(w.id, mesh.Config{Backend: "cpu", Model: model}, &meshv1.HardwareInfo{Hostname: w.id}, rt)
	w.rpc = f.server("", func(g *grpc.Server) { meshv1.RegisterWorkerServiceServer(g, w.svc) })
	f.workers = append(f.workers, w)
	go func() { w.done <- w.svc.RunRuntime(ctx) }()
	return w
}
func (f *fixture) stopWorker(w *workerProcess) {
	w.once.Do(func() {
		if w.memberCancel != nil {
			w.memberCancel()
			f.join(w.memberDone)
		}
		w.cancel()
		f.stopRPC(w.rpc)
		f.join(w.done)
		if w.rt.Counters().Active != 0 {
			f.t.Error("worker shutdown left active runtime work")
		}
	})
}
func (f *fixture) controller(address string, wrap func(*controller.Service) meshv1.ControllerServiceServer) *controllerProcess {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.ctx)
	c := &controllerProcess{reg: controller.NewRegistry(model), cancel: cancel, done: make(chan error, 1)}
	c.svc = controller.New(uuid.NewString(), c.reg)
	c.rpc = f.server(address, func(g *grpc.Server) {
		var service meshv1.ControllerServiceServer = c.svc
		if wrap != nil {
			service = wrap(c.svc)
		}
		meshv1.RegisterControllerServiceServer(g, service)
	})
	f.controllers = append(f.controllers, c)
	go func() { c.done <- c.svc.Run(ctx) }()
	conn, err := grpc.NewClient(c.rpc.address, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes)),
		grpc.WithInitialWindowSize(65535), grpc.WithInitialConnWindowSize(65535))
	if err != nil {
		f.t.Fatal(err)
	}
	c.conn = conn
	c.client = meshv1.NewControllerServiceClient(conn)
	return c
}
func (f *fixture) stopController(c *controllerProcess) {
	c.once.Do(func() {
		c.cancel()
		f.stopRPC(c.rpc)
		if err := c.svc.Close(); err != nil {
			f.t.Error(err)
		}
		f.join(c.done)
		if c.conn != nil {
			if err := c.conn.Close(); err != nil {
				f.t.Error(err)
			}
		}
	})
}
func (f *fixture) membership(c *controllerProcess, w *workerProcess) {
	ctx, cancel := context.WithCancel(f.ctx)
	w.memberCancel = cancel
	w.memberDone = make(chan error, 1)
	address := c.rpc.address
	go func() {
		w.memberDone <- worker.RunMembership(ctx, w.svc, w.rpc.address, func(context.Context) (string, error) { return address, nil }, nil)
	}()
}
func (f *fixture) register(c *controllerProcess, w *workerProcess) {
	f.t.Helper()
	rpc, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	_, err := c.client.RegisterWorker(rpc, &meshv1.RegisterWorkerRequest{WorkerId: w.id, Endpoint: w.rpc.address, ProtocolMajor: protocol.Major, Hardware: &meshv1.HardwareInfo{Hostname: w.id}, RuntimeVersion: "fake", Backend: "cpu", Model: &meshv1.ModelDescriptor{Id: model.ID, Sha256: model.SHA256, ContextTokens: 2048}, Capacity: 1, Report: w.svc.Report()})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) eventually(predicate func() bool, message string) {
	f.t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatal(message)
		case <-ticker.C:
		}
	}
}
func (f *fixture) rows(c *controllerProcess) []*meshv1.WorkerInfo {
	rpc, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	rows, err := c.client.GetClusterStatus(rpc, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
	if err != nil {
		return nil
	}
	return rows.Workers
}
func (f *fixture) ready(c *controllerProcess, count int) {
	f.t.Helper()
	f.eventually(func() bool {
		rows := f.rows(c)
		if len(rows) != count {
			return false
		}
		for _, r := range rows {
			if r.State != meshv1.WorkerState_WORKER_STATE_READY {
				return false
			}
		}
		return true
	}, "workers did not become ready through reverse proof")
}
func (f *fixture) runtimeReady(w *workerProcess) {
	f.eventually(func() bool { return w.svc.Report().RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY }, "runtime did not become ready")
}
func (f *fixture) signal(ch <-chan struct{}) {
	f.t.Helper()
	select {
	case <-ch:
	case <-f.ctx.Done():
		f.t.Fatal("runtime phase not observed")
	}
}
func request() *meshv1.InferenceRequest {
	return &meshv1.InferenceRequest{RequestId: uuid.NewString(), ModelId: model.ID, Prompt: "hello", MaxOutputTokens: 128}
}
func (f *fixture) infer(c *controllerProcess, ctx context.Context, req *meshv1.InferenceRequest) grpc.ServerStreamingClient[meshv1.InferenceEvent] {
	f.t.Helper()
	s, err := c.client.RunInference(ctx, req)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func recv(t *testing.T, s grpc.ServerStreamingClient[meshv1.InferenceEvent]) *meshv1.InferenceEvent {
	t.Helper()
	e, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func started(t *testing.T, s grpc.ServerStreamingClient[meshv1.InferenceEvent], req *meshv1.InferenceRequest) string {
	t.Helper()
	e := recv(t, s).GetStarted()
	if e == nil || e.RequestId != req.RequestId || e.ModelId != model.ID || e.WorkerId == "" {
		t.Fatalf("invalid Started=%v", e)
	}
	return e.WorkerId
}
func partial(t *testing.T, s grpc.ServerStreamingClient[meshv1.InferenceEvent]) {
	t.Helper()
	if e := recv(t, s).GetTextDelta(); e == nil || e.Text != "partial 🌏" {
		t.Fatalf("partial output=%v", e)
	}
}
func finish(t *testing.T, s grpc.ServerStreamingClient[meshv1.InferenceEvent]) {
	t.Helper()
	completed := false
	for {
		e, err := s.Recv()
		if err == io.EOF {
			if !completed {
				t.Fatal("OK without Completed")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.GetStarted() != nil || completed {
			t.Fatalf("unexpected stream event=%v", e)
		}
		if e.GetCompleted() != nil {
			if e.GetCompleted().FinishReason != "stop" {
				t.Fatal("invalid finish reason")
			}
			completed = true
		}
	}
}
func failed(t *testing.T, s grpc.ServerStreamingClient[meshv1.InferenceEvent]) codes.Code {
	t.Helper()
	for {
		e, err := s.Recv()
		if err != nil {
			if err == io.EOF {
				t.Fatal("failed request ended OK")
			}
			return status.Code(err)
		}
		if e.GetCompleted() != nil {
			t.Fatal("failed request emitted Completed")
		}
	}
}

func TestTwoWorkersAndThirdRequestRejected(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	for i := 0; i < 2; i++ {
		rt := newRuntime()
		rt.ReleaseGate = f.gate().ch
		w := f.worker(rt)
		f.runtimeReady(w)
		f.membership(c, w)
	}
	f.ready(c, 2)
	a, b := request(), request()
	sa := f.infer(c, f.ctx, a)
	sb := f.infer(c, f.ctx, b)
	ia, ib := started(t, sa, a), started(t, sb, b)
	if ia == ib {
		t.Fatal("overlapping requests selected the same worker")
	}
	third := f.infer(c, f.ctx, request())
	if code := failed(t, third); code != codes.ResourceExhausted {
		t.Fatalf("third request=%v", code)
	}
	for _, w := range f.workers {
		if w.rt.attempts() != 1 || w.rt.Counters().Peak != 1 {
			t.Fatal("third request entered runtime")
		}
	}
	for _, g := range f.gates {
		g.open()
	}
	finish(t, sa)
	finish(t, sb)
	f.ready(c, 2)
	next := request()
	fresh := f.infer(c, f.ctx, next)
	started(t, fresh, next)
	finish(t, fresh)
	for _, w := range f.workers {
		if w.rt.Counters().Active != 0 || w.rt.Counters().Peak != 1 {
			t.Fatal("capacity ownership leaked")
		}
	}
	if f.workers[0].rt.attempts()+f.workers[1].rt.attempts() != 3 {
		t.Fatal("rejected request was queued")
	}
}

func TestCancelDuringStreamAndReuse(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	delta, cleanup := f.gate(), f.gate()
	rt.deltaGate = delta.ch
	rt.CleanupGate = cleanup.ch
	w := f.worker(rt)
	f.runtimeReady(w)
	f.membership(c, w)
	f.ready(c, 1)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	req := request()
	s := f.infer(c, ctx, req)
	started(t, s, req)
	partial(t, s)
	cancel()
	if code := failed(t, s); code != codes.Canceled {
		t.Fatalf("cancel=%v", code)
	}
	f.signal(rt.Cleaning)
	if report := w.svc.Report(); !report.Active || report.GetActiveRequestId() != req.RequestId {
		t.Fatalf("cleanup ownership=%v", report)
	}
	f.eventually(func() bool { rows := f.rows(c); return len(rows) == 1 && rows[0].ReservedRequestId == nil }, "controller did not release canceled reservation")
	if code := failed(t, f.infer(c, f.ctx, request())); code != codes.Unavailable && code != codes.ResourceExhausted {
		t.Fatalf("cleanup admission=%v", code)
	}
	if rt.attempts() != 1 {
		t.Fatal("new generation overlapped cleanup")
	}
	cleanup.open()
	delta.open()
	f.signal(rt.Released)
	f.ready(c, 1)
	req = request()
	fresh := f.infer(c, f.ctx, req)
	started(t, fresh, req)
	partial(t, fresh)
	finish(t, fresh)
	if rt.Counters().Peak != 1 || rt.Counters().Active != 0 {
		t.Fatal("reuse overlapped cleanup")
	}
}

func TestWorkerDiesWithoutReplay(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	for i := 0; i < 2; i++ {
		rt := newRuntime()
		rt.deltaGate = f.gate().ch
		w := f.worker(rt)
		f.runtimeReady(w)
		f.membership(c, w)
	}
	f.ready(c, 2)
	req := request()
	s := f.infer(c, f.ctx, req)
	id := started(t, s, req)
	partial(t, s)
	var dead, survivor *workerProcess
	for _, w := range f.workers {
		if w.id == id {
			dead = w
		} else {
			survivor = w
		}
	}
	// Close the actual transport first: normal runtime-owner cancellation must
	// not win the race and turn this into another cancellation scenario.
	f.stopRPC(dead.rpc)
	if code := failed(t, s); code != codes.Unavailable {
		t.Fatalf("worker transport loss=%v", code)
	}
	f.stopWorker(dead)
	f.eventually(func() bool {
		for _, r := range f.rows(c) {
			if r.WorkerId == id {
				return r.ReservedRequestId == nil && r.State != meshv1.WorkerState_WORKER_STATE_READY
			}
		}
		return false
	}, "lost worker reservation did not release")
	if survivor.rt.attempts() != 0 {
		t.Fatal("failed request replayed on survivor")
	}
	for _, g := range f.gates {
		g.open()
	}
	req = request()
	fresh := f.infer(c, f.ctx, req)
	if selected := started(t, fresh, req); selected != survivor.id {
		t.Fatal("new request selected lost endpoint")
	}
	partial(t, fresh)
	finish(t, fresh)
	if dead.rt.attempts() != 1 || survivor.rt.attempts() != 1 {
		t.Fatal("ambiguous dispatch was retried")
	}
}

func TestRuntimeCrashRecovery(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	delta, stop := f.gate(), f.gate()
	rt.deltaGate = delta.ch
	rt.stopGate = stop.ch
	w := f.worker(rt)
	f.runtimeReady(w)
	f.membership(c, w)
	f.ready(c, 1)
	req := request()
	s := f.infer(c, f.ctx, req)
	started(t, s, req)
	partial(t, s)
	rt.crashed.Store(true)
	f.signal(rt.stopping)
	failed(t, s)
	f.eventually(func() bool {
		rows := f.rows(c)
		return len(rows) == 1 && rows[0].State == meshv1.WorkerState_WORKER_STATE_UNHEALTHY
	}, "runtime crash was not visible")
	if code := failed(t, f.infer(c, f.ctx, request())); code != codes.Unavailable {
		t.Fatalf("recovery admission=%v", code)
	}
	if rt.attempts() != 1 || rt.Counters().Starts != 1 {
		t.Fatal("admission reopened during stop")
	}
	stop.open()
	delta.open()
	f.ready(c, 1)
	if rt.Counters().Starts != 2 {
		t.Fatalf("restart count=%v", rt.Counters())
	}
	req = request()
	fresh := f.infer(c, f.ctx, req)
	if id := started(t, fresh, req); id != w.id {
		t.Fatal("recovery changed process identity")
	}
	partial(t, fresh)
	finish(t, fresh)
	if rt.attempts() != 2 || rt.Counters().Peak != 1 {
		t.Fatal("crashed request replayed or overlapped")
	}
}

func TestControllerRestartReregisters(t *testing.T) {
	f := newFixture(t)
	old := f.controller("", nil)
	for i := 0; i < 2; i++ {
		rt := newRuntime()
		rt.deltaGate = f.gate().ch
		w := f.worker(rt)
		f.runtimeReady(w)
		f.membership(old, w)
	}
	f.ready(old, 2)
	req := request()
	s := f.infer(old, f.ctx, req)
	started(t, s, req)
	partial(t, s)
	oldRows := f.rows(old)
	oldStatus, err := old.client.GetClusterStatus(f.ctx, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
	if err != nil {
		t.Fatal(err)
	}
	address := old.rpc.address
	f.stopController(old)
	failed(t, s)
	reachable := time.Now()
	replacement := f.controller(address, nil)
	f.ready(replacement, 2)
	newStatus, err := replacement.client.GetClusterStatus(f.ctx, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
	if err != nil || newStatus.GetControllerId() == oldStatus.ControllerId {
		t.Fatalf("controller identity did not change: status=%v error=%v", newStatus, err)
	}
	if time.Since(reachable) > 15*time.Second {
		t.Fatal("reregistration exceeded 15 seconds")
	}
	rows := f.rows(replacement)
	for _, r := range rows {
		found := false
		for _, previous := range oldRows {
			if r.WorkerId == previous.WorkerId && r.Endpoint == previous.Endpoint {
				found = true
			}
		}
		if !found {
			t.Fatal("worker restarted after controller loss")
		}
	}
	for _, w := range f.workers {
		if w.rt.Counters().Starts != 1 {
			t.Fatal("controller loss restarted runtime")
		}
	}
	if f.workers[0].rt.attempts()+f.workers[1].rt.attempts() != 1 {
		t.Fatal("abandoned request replayed")
	}
	for _, g := range f.gates {
		g.open()
	}
	req = request()
	fresh := f.infer(replacement, f.ctx, req)
	started(t, fresh, req)
	partial(t, fresh)
	finish(t, fresh)
}

func TestHeartbeatExpiryDuringStream(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	rt.deltaGate = f.gate().ch
	w := f.worker(rt)
	f.runtimeReady(w)
	f.register(c, w)
	f.ready(c, 1)
	req := request()
	s := f.infer(c, f.ctx, req)
	started(t, s, req)
	partial(t, s)
	// Only Service.Run consumes expiry obligations. Backdating this explicit
	// receipt input avoids sleeping through the real ten-second expiry policy.
	if err := c.reg.Heartbeat(w.id, w.svc.Report(), time.Now().Add(-11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if code := failed(t, s); code != codes.Canceled {
		t.Fatalf("expiry status=%v", code)
	}
	f.signal(rt.Released)
	f.eventually(func() bool {
		rows := f.rows(c)
		return len(rows) == 1 && rows[0].State == meshv1.WorkerState_WORKER_STATE_UNAVAILABLE && rows[0].ReservedRequestId == nil
	}, "expiry did not release reservation")
	if rt.attempts() != 1 || rt.Counters().Active != 0 {
		t.Fatal("expiry replayed or leaked generation")
	}
}

// Holding the first real heartbeat leaves membership alive but unable to renew
// its row before the real controller sweep removes it.
type heldHeartbeat struct {
	*controller.Service
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (h *heldHeartbeat) Heartbeat(ctx context.Context, r *meshv1.HeartbeatRequest) (*meshv1.HeartbeatResponse, error) {
	first := false
	h.once.Do(func() { first = true; close(h.entered) })
	if first {
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	return h.Service.Heartbeat(ctx, r)
}
func TestUnknownWorkerAfterRetentionReregisters(t *testing.T) {
	f := newFixture(t)
	release := f.gate()
	var held *heldHeartbeat
	c := f.controller("", func(s *controller.Service) meshv1.ControllerServiceServer {
		held = &heldHeartbeat{Service: s, entered: make(chan struct{}), release: release.ch}
		return held
	})
	w := f.worker(newRuntime())
	f.runtimeReady(w)
	f.membership(c, w)
	f.ready(c, 1)
	f.signal(held.entered)
	if err := c.reg.Heartbeat(w.id, w.svc.Report(), time.Now().Add(-71*time.Second)); err != nil {
		t.Fatal(err)
	}
	f.eventually(func() bool { return len(f.rows(c)) == 0 }, "retained row not deleted by Run sweep")
	release.open()
	f.ready(c, 1)
	row := f.rows(c)[0]
	if row.WorkerId != w.id || row.Endpoint != w.rpc.address || w.rt.Counters().Starts != 1 {
		t.Fatal("registry loss changed runtime identity")
	}
	req := request()
	fresh := f.infer(c, f.ctx, req)
	started(t, fresh, req)
	partial(t, fresh)
	finish(t, fresh)
}

func TestRuntimeMetadataRefreshPreservesReservation(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	start := f.gate()
	rt.StartGate = start.ch
	rt.holdVersion.Store(true)
	rt.deltaGate = f.gate().ch
	w := f.worker(rt)
	f.signal(rt.Started)
	f.membership(c, w)
	f.eventually(func() bool {
		rows := f.rows(c)
		return len(rows) == 1 && rows[0].RuntimeVersion == "unknown" && rows[0].State == meshv1.WorkerState_WORKER_STATE_STARTING
	}, "loading registration missing")
	start.open()
	f.ready(c, 1)
	req := request()
	s := f.infer(c, f.ctx, req)
	started(t, s, req)
	partial(t, s)
	rt.holdVersion.Store(false)
	f.eventually(func() bool { rows := f.rows(c); return len(rows) == 1 && rows[0].RuntimeVersion == "fake" }, "verified runtime metadata did not refresh")
	row := f.rows(c)[0]
	if row.WorkerId != w.id || row.Endpoint != w.rpc.address || row.GetReservedRequestId() != req.RequestId || row.State != meshv1.WorkerState_WORKER_STATE_BUSY || rt.Counters().Starts != 1 {
		t.Fatalf("metadata refresh lost ownership: %v", row)
	}
	// Registration requires a new reverse proof. Until it completes, rejection
	// can be Unavailable; the active reservation must never admit another call.
	if code := failed(t, f.infer(c, f.ctx, request())); code != codes.ResourceExhausted && code != codes.Unavailable {
		t.Fatalf("metadata refresh reopened slot: %v", code)
	}
	if rt.attempts() != 1 {
		t.Fatal("metadata refresh admitted overlapping generation")
	}
	for _, g := range f.gates {
		g.open()
	}
	finish(t, s)
	f.ready(c, 1)
}

// Entry and exit bracket actual worker transport Send. Capacity-one observation
// channels make the test consume both boundaries rather than queue every event.
type observedSend struct {
	grpc.ServerStreamingServer[meshv1.InferenceEvent]
	entered, exited chan struct{}
}

func (s *observedSend) Send(e *meshv1.InferenceEvent) error {
	select {
	case s.entered <- struct{}{}:
	case <-s.Context().Done():
		return s.Context().Err()
	}
	err := s.ServerStreamingServer.Send(e)
	// Preserve each boundary while running, but never hold canceled cleanup on
	// an observation the test no longer needs to consume.
	select {
	case s.exited <- struct{}{}:
	case <-s.Context().Done():
	}
	return err
}

type observedWorker struct {
	*worker.Service
	entered, exited chan struct{}
	done            chan error
}

func (w *observedWorker) Generate(r *meshv1.InferenceRequest, s grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	err := w.Service.Generate(r, &observedSend{ServerStreamingServer: s, entered: w.entered, exited: w.exited})
	w.done <- err
	return err
}
func TestSlowReaderStaysBounded(t *testing.T) {
	f := newFixture(t)
	c := f.controller("", nil)
	rt := newRuntime()
	rt.Events = []mesh.Event{{Kind: mesh.EventStarted}}
	for i := 0; i < 2048; i++ {
		rt.Events = append(rt.Events, mesh.Event{Kind: mesh.EventTextDelta, Text: strings.Repeat("x", 4096)})
	}
	rt.Events = append(rt.Events, mesh.Event{Kind: mesh.EventCompleted, FinishReason: "stop"})
	w := f.worker(rt)
	f.runtimeReady(w)
	// Replace only the test-owned listener before registration; production worker
	// logic still delegates through its actual generated gRPC service.
	f.stopRPC(w.rpc)
	observed := &observedWorker{Service: w.svc, entered: make(chan struct{}, 1), exited: make(chan struct{}, 1), done: make(chan error, 1)}
	w.rpc = f.server("", func(g *grpc.Server) { meshv1.RegisterWorkerServiceServer(g, observed) })
	f.membership(c, w)
	f.ready(c, 1)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	s := f.infer(c, ctx, request())
	stalled := false
	sends := 0
	for ; sends < 2050; sends++ {
		f.signal(observed.entered)
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-observed.exited:
			timer.Stop()
		case <-timer.C:
			stalled = true
		}
		if stalled {
			break
		}
	}
	if !stalled || sends == 0 {
		t.Fatalf("no end-to-end backpressure after %d sends", sends)
	}
	if rt.attempts() != 1 || rt.Counters().Active != 1 {
		t.Fatal("runtime emission lost active ownership")
	}
	cancel()
	select {
	case err := <-observed.done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("stalled handler=%v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("stalled handler did not terminate")
	}
	f.signal(rt.Released)
	failed(t, s)
	if rt.Counters().Active != 0 || rt.Counters().Peak != 1 {
		t.Fatal("slow reader cleanup leaked")
	}
}
