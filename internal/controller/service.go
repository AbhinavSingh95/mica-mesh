package controller

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	probeTimeout    = time.Second
	shutdownTimeout = 2 * time.Second
)

type connectionKey struct{ workerID, endpoint string }
type workerConnection struct {
	conn     *grpc.ClientConn
	users    int
	obsolete bool
}

// Service owns worker connections, bounded reverse probes and request cancellation.
// The process owner calls Run once, Close to begin shutdown, and stops its RPC listeners.
// Lock ordering is service mutex before registry mutex; network and cancellation
// operations happen outside both. Registry must not be expired by another owner.
type Service struct {
	meshv1.UnimplementedControllerServiceServer
	mu          sync.Mutex
	id          string
	registry    *Registry
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	running     bool
	runDone     chan struct{}
	active      map[Reservation]context.CancelFunc
	idle        chan struct{}
	connections map[connectionKey]*workerConnection
	pending     map[string]bool
	queue       []string
	wake        chan struct{}
}

// New uses the already configured registry. Run owns sweeps and queued probes;
// registration remains visible and ineligible until that reverse proof succeeds.
func New(id string, registry *Registry) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Service{id: id, registry: registry, ctx: ctx, cancel: cancel, active: make(map[Reservation]context.CancelFunc), idle: idle,
		connections: make(map[connectionKey]*workerConnection), pending: make(map[string]bool), wake: make(chan struct{}, 1)}
}

// RegisterWorker records membership before queueing an independent reverse probe.
func (s *Service) RegisterWorker(ctx context.Context, req *meshv1.RegisterWorkerRequest) (*meshv1.RegisterWorkerResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "controller is stopping")
	}
	if err := s.registry.Register(req, time.Now()); err != nil {
		return nil, err
	}
	s.enqueueLocked(req.WorkerId)
	return &meshv1.RegisterWorkerResponse{ControllerId: s.id, HeartbeatIntervalSeconds: 2, MembershipExpirySeconds: 10}, nil
}

// Heartbeat refreshes membership, never a reservation or an idle health proof.
func (s *Service) Heartbeat(ctx context.Context, req *meshv1.HeartbeatRequest) (*meshv1.HeartbeatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "heartbeat is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "controller is stopping")
	}
	if err := s.registry.Heartbeat(req.WorkerId, req.Report, time.Now()); err != nil {
		return nil, err
	}
	s.enqueueLocked(req.WorkerId)
	return &meshv1.HeartbeatResponse{}, nil
}

// GetClusterStatus returns detached membership snapshots without network calls.
func (s *Service) GetClusterStatus(ctx context.Context, _ *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return &meshv1.GetClusterStatusResponse{ControllerId: s.id, Workers: s.registry.Snapshot(time.Now())}, nil
}

// RunInference makes one dispatch and proxies incrementally with backpressure.
// Completed is retained until the upstream stream terminates with gRPC OK.
func (s *Service) RunInference(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	ctx := out.Context()
	cancel, ok := protocol.GenerationCancel(ctx)
	if !ok {
		return status.Error(codes.Internal, "generation transport lifetime is not configured")
	}
	defer cancel()
	if err := protocol.ValidateRequest(req, s.registry.model.ID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "controller is stopping")
	}
	reservation, err := s.registry.Reserve(req.ModelId, req.RequestId, time.Now())
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if len(s.active) == 0 {
		s.idle = make(chan struct{})
	}
	s.active[reservation] = cancel
	s.mu.Unlock()
	defer func() {
		// This cancel also unblocks the inbound transport Send, unlike a handler-only context.
		cancel()
		s.mu.Lock()
		delete(s.active, reservation)
		if s.registry.Release(reservation) {
			s.enqueueLocked(reservation.WorkerID)
		}
		if len(s.active) == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}()
	connection, err := s.acquireConnection(reservation.WorkerID, reservation.Endpoint)
	if err != nil {
		return proxyError(ctx, err)
	}
	defer s.releaseConnection(connection)
	upstream, err := meshv1.NewWorkerServiceClient(connection.conn).Generate(ctx, req,
		grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes))
	if err != nil {
		return proxyError(ctx, err)
	}
	started := false
	var terminal *meshv1.Completed
	for {
		event, err := upstream.Recv()
		if err == io.EOF {
			if !started || terminal == nil {
				return status.Error(codes.Internal, "worker stream ended without completion")
			}
			if err := ctx.Err(); err != nil {
				return status.FromContextError(err).Err()
			}
			if err := out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Completed{Completed: terminal}}); err != nil {
				return proxyError(ctx, err)
			}
			if err := ctx.Err(); err != nil {
				return status.FromContextError(err).Err()
			}
			return nil
		}
		if err != nil {
			return proxyError(ctx, err)
		}
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if terminal != nil || event == nil {
			return status.Error(codes.Internal, "invalid worker stream")
		}
		switch payload := event.Payload.(type) {
		case *meshv1.InferenceEvent_Started:
			e := payload.Started
			if started || e == nil || e.RequestId != req.RequestId || e.WorkerId != reservation.WorkerID || e.ModelId != req.ModelId {
				return status.Error(codes.Internal, "invalid worker stream identity or order")
			}
			started = true
		case *meshv1.InferenceEvent_TextDelta:
			e := payload.TextDelta
			if !started || e == nil || !utf8.ValidString(e.Text) || len(e.Text) > 4096 {
				return status.Error(codes.Internal, "invalid worker text delta")
			}
		case *meshv1.InferenceEvent_Completed:
			e := payload.Completed
			if !started || e == nil || (e.FinishReason != "stop" && e.FinishReason != "length") || (e.InputTokens != nil && *e.InputTokens < 0) || (e.OutputTokens != nil && *e.OutputTokens < 0) {
				return status.Error(codes.Internal, "invalid worker completion")
			}
			// Retain only supported terminal metadata, not an entire wire message
			// whose unknown extension fields could consume the buffering budget.
			terminal = &meshv1.Completed{FinishReason: e.FinishReason}
			if e.InputTokens != nil {
				count := *e.InputTokens
				terminal.InputTokens = &count
			}
			if e.OutputTokens != nil {
				count := *e.OutputTokens
				terminal.OutputTokens = &count
			}
			continue
		default:
			return status.Error(codes.Internal, "invalid worker stream payload")
		}
		if err := out.Send(event); err != nil {
			return proxyError(ctx, err)
		}
	}
}

func proxyError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return status.Error(codes.InvalidArgument, "worker rejected input or context budget")
	case codes.NotFound:
		return status.Error(codes.NotFound, "worker does not support the requested model")
	case codes.ResourceExhausted:
		return status.Error(codes.ResourceExhausted, "worker generation resource limit exceeded")
	case codes.Unavailable:
		return status.Error(codes.Unavailable, "worker unavailable")
	case codes.Canceled:
		return status.Error(codes.Canceled, "generation canceled")
	case codes.DeadlineExceeded:
		return status.Error(codes.DeadlineExceeded, "generation deadline exceeded")
	default:
		return status.Error(codes.Internal, "worker generation failed")
	}
}

func (s *Service) enqueueLocked(workerID string) {
	if s.closed || s.pending[workerID] {
		return
	}
	s.pending[workerID] = true
	s.queue = append(s.queue, workerID)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run keeps sweeps independent of the single probe loop: a slow peer cannot
// postpone expiry. Queueing coalesces heartbeats to one pending item per worker.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "controller is stopping")
	}
	if s.running {
		s.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "controller lifecycle already running")
	}
	s.running = true
	s.runDone = make(chan struct{})
	s.mu.Unlock()
	probesDone := make(chan struct{})
	go func() { defer close(probesDone); s.probeLoop() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		s.stop()
		<-probesDone
		s.mu.Lock()
		close(s.runDone)
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case <-ticker.C:
			s.sweep(time.Now())
		}
	}
}
func (s *Service) probeLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if s.closed || len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			workerID := s.queue[0]
			s.queue = s.queue[1:]
			delete(s.pending, workerID)
			s.mu.Unlock()
			s.probeWorker(workerID)
		}
	}
}
func (s *Service) probeWorker(workerID string) {
	target, ok := s.registry.ProbeTarget(workerID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, probeTimeout)
	defer cancel()
	connection, err := s.acquireConnection(target.WorkerID, target.Endpoint)
	if err != nil {
		s.registry.ApplyProbe(target, nil, err)
		return
	}
	defer s.releaseConnection(connection)
	health, err := meshv1.NewWorkerServiceClient(connection.conn).Health(ctx, &meshv1.HealthRequest{}, grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes))
	// A rejected stale failure must not close a connection used by a newer request.
	s.registry.ApplyProbe(target, health, err)
}
func (s *Service) sweep(now time.Time) {
	s.mu.Lock()
	var cancels []context.CancelFunc
	for _, reservation := range s.registry.Expire(now) {
		if cancel, ok := s.active[reservation]; ok {
			cancels = append(cancels, cancel)
		}
	}
	live := make(map[string]string)
	for _, row := range s.registry.Snapshot(now) {
		live[row.WorkerId] = row.Endpoint
	}
	// Keep queued work bounded by retained membership even if probes fall behind
	// repeated process restarts. One already-running probe stays timeout-bound.
	kept := 0
	for _, workerID := range s.queue {
		if _, ok := live[workerID]; ok {
			s.queue[kept] = workerID
			kept++
		} else {
			delete(s.pending, workerID)
		}
	}
	clear(s.queue[kept:])
	s.queue = s.queue[:kept]
	var closeConnections []*grpc.ClientConn
	for key, c := range s.connections {
		if live[key.workerID] == key.endpoint {
			continue
		}
		delete(s.connections, key)
		c.obsolete = true
		if c.users == 0 {
			closeConnections = append(closeConnections, c.conn)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, connection := range closeConnections {
		// grpc Close only errors when already closed; overlapping shutdown is harmless.
		_ = connection.Close()
	}
}
func (s *Service) acquireConnection(workerID, endpoint string) (*workerConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "controller is stopping")
	}
	key := connectionKey{workerID, endpoint}
	c := s.connections[key]
	if c == nil {
		// NewClient does not do network I/O; the first cancellable RPC connects.
		conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		c = &workerConnection{conn: conn}
		s.connections[key] = c
	}
	c.users++
	return c, nil
}
func (s *Service) releaseConnection(c *workerConnection) {
	s.mu.Lock()
	c.users--
	closeConnection := c.obsolete && c.users == 0
	s.mu.Unlock()
	if closeConnection {
		// Close may have closed this retired connection during shutdown.
		_ = c.conn.Close()
	}
}
func (s *Service) stop() {
	s.mu.Lock()
	s.closed = true
	s.queue = nil
	s.pending = make(map[string]bool)
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	s.cancel()
	for _, cancel := range cancels {
		cancel()
	}
}

// Close rejects admission, cancels actual transports, joins Run/handlers, then
// closes reusable connections. The bounded wait reports any incomplete shutdown.
func (s *Service) Close() error {
	s.stop()
	s.mu.Lock()
	idle, runDone := s.idle, s.runDone
	s.mu.Unlock()
	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	var err error
	for _, done := range []<-chan struct{}{idle, runDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-timer.C:
			err = errors.New("controller shutdown did not finish within two seconds")
		}
		if err != nil {
			break
		}
	}
	s.mu.Lock()
	connections := make([]*grpc.ClientConn, 0, len(s.connections))
	for key, c := range s.connections {
		c.obsolete = true
		connections = append(connections, c.conn)
		delete(s.connections, key)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		err = errors.Join(err, connection.Close())
	}
	return err
}
