// Package controller owns membership and atomic one-slot inference admission.
package controller

import (
	"encoding/hex"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	membershipExpiry     = 10 * time.Second
	unavailableRetention = 60 * time.Second
)

// Reservation identifies one controller-owned slot. Generation distinguishes
// repeated request IDs and survives deletion/recreation of a worker row.
type Reservation struct {
	WorkerID, Endpoint, RequestID string
	Generation                    uint64
}

// Probe captures an observation's target and revision before network work.
type Probe struct {
	WorkerID, Endpoint string
	Revision           uint64
}

type probeState uint8

const (
	probePending probeState = iota
	probeIdle
	probeOccupied
	probeFailed
)

type expiryCancellation uint8

const (
	cancellationNone expiryCancellation = iota
	cancellationPending
	cancellationReported
)

type worker struct {
	info         *meshv1.RegisterWorkerRequest
	received     time.Time
	revision     uint64
	probeState   probeState
	probeError   string
	reservation  *Reservation
	cancellation expiryCancellation
}

// Registry owns copied membership data; all mutations and admission are atomic.
// Callers supply controller receipt times and must not mutate protobuf inputs
// during a call. Registry methods perform no network work or cancellation.
type Registry struct {
	mu       sync.Mutex
	model    runtime.Model
	workers  map[string]*worker
	order    []string
	next     int
	sequence uint64
}

// NewRegistry uses an already validated configured model, including its context.
func NewRegistry(model runtime.Model) *Registry {
	return &Registry{model: model, workers: make(map[string]*worker)}
}

// Register creates or refreshes membership but requires a new reverse probe.
// Refreshes preserve reservations, including expiry cancellation owed to Expire.
func (r *Registry) Register(info *meshv1.RegisterWorkerRequest, now time.Time) error {
	if err := r.validateRegistration(info); err != nil {
		return err
	}
	owned := proto.Clone(info).(*meshv1.RegisterWorkerRequest)
	r.mu.Lock()
	defer r.mu.Unlock()
	w, exists := r.workers[info.WorkerId]
	if exists && w.info.Endpoint != info.Endpoint {
		return status.Error(codes.FailedPrecondition, "worker endpoint changed; restart the worker to use a new endpoint")
	}
	if !exists {
		w = &worker{}
		r.workers[info.WorkerId] = w
		r.order = append(r.order, info.WorkerId)
	} else {
		w.noteExpiry(now)
	}
	w.info = owned
	w.received = now
	w.probeState = probePending
	w.probeError = ""
	w.revision = r.advance()
	return nil
}

// Heartbeat updates receipt time and diagnostics. It cannot renew overdue
// membership, clear a reservation, or replace the required idle reverse probe.
func (r *Registry) Heartbeat(workerID string, report *meshv1.WorkerReport, now time.Time) error {
	if err := uuid.Validate(workerID); err != nil {
		return status.Error(codes.InvalidArgument, "worker_id must be a UUID")
	}
	if err := validateReport(report); err != nil {
		return err
	}
	owned := proto.Clone(report).(*meshv1.WorkerReport)
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[workerID]
	if !ok || w.expired(now) {
		return status.Error(codes.NotFound, "worker membership is unknown or expired; register again")
	}
	w.info.Report = owned
	w.received = now
	// Any observed activity or loss of runtime readiness invalidates cached idle
	// proof. A later idle heartbeat does not confirm cleanup has finished.
	if w.probeState != probeFailed {
		if report.RuntimeState != meshv1.RuntimeState_RUNTIME_STATE_READY {
			w.probeState = probePending
		} else if report.Active && w.probeState == probeIdle {
			w.probeState = probeOccupied
		} else if !report.Active && w.reservation == nil && w.probeState == probeOccupied {
			w.probeState = probePending
		}
	}
	w.revision = r.advance()
	return nil
}

// Reserve immediately selects a ready idle worker in stable round-robin order.
// Occupied reachable ready workers yield ResourceExhausted; pending health proof
// alone yields Unavailable. No requests are queued.
func (r *Registry) Reserve(modelID, requestID string, now time.Time) (Reservation, error) {
	if err := uuid.Validate(requestID); err != nil {
		return Reservation{}, status.Error(codes.InvalidArgument, "request_id must be a UUID")
	}
	if modelID != r.model.ID {
		return Reservation{}, status.Error(codes.NotFound, "model is not supported")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	busy := false
	for offset := 0; offset < len(r.order); offset++ {
		i := (r.next + offset) % len(r.order)
		w := r.workers[r.order[i]]
		switch w.state(now) {
		case meshv1.WorkerState_WORKER_STATE_BUSY:
			if w.info.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY && (w.probeState == probeIdle || w.probeState == probeOccupied) {
				busy = true
			}
		case meshv1.WorkerState_WORKER_STATE_READY:
			reservation := Reservation{WorkerID: w.info.WorkerId, Endpoint: w.info.Endpoint, RequestID: requestID, Generation: r.advance()}
			w.reservation = &reservation
			w.cancellation = cancellationNone
			w.probeState = probeOccupied
			w.revision = r.advance()
			r.next = (i + 1) % len(r.order)
			return reservation, nil
		}
	}
	if busy {
		return Reservation{}, status.Error(codes.ResourceExhausted, "all compatible healthy workers are occupied")
	}
	return Reservation{}, status.Error(codes.Unavailable, "no reachable ready worker is available")
}

// Release frees only the exact current reservation and succeeds once. The row
// remains excluded until a new revision-bound idle probe confirms cleanup.
func (r *Registry) Release(reservation Reservation) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[reservation.WorkerID]
	if !ok || w.reservation == nil || *w.reservation != reservation {
		return false
	}
	w.reservation = nil
	w.cancellation = cancellationNone
	// Confirmed runtime cleanup remains healthy-but-occupied. Once it reports
	// idle, Heartbeat still requires a fresh idle probe before reuse.
	if w.probeState == probeIdle || (w.probeState == probeOccupied && !w.info.Report.Active) {
		w.probeState = probePending
	}
	w.revision = r.advance()
	return true
}

// Snapshot returns deterministic independent protobuf snapshots. Membership age
// uses controller receipt time, and overdue rows are unavailable before a sweep.
func (r *Registry) Snapshot(now time.Time) []*meshv1.WorkerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*meshv1.WorkerInfo, 0, len(r.order))
	for _, workerID := range r.order {
		w := r.workers[workerID]
		age := now.Sub(w.received)
		if age < 0 {
			age = 0
		}
		info := w.info
		snapshot := &meshv1.WorkerInfo{WorkerId: info.WorkerId, Endpoint: info.Endpoint, ProtocolMajor: info.ProtocolMajor,
			Hardware: info.Hardware, RuntimeVersion: info.RuntimeVersion, Backend: info.Backend, Model: info.Model, Capacity: info.Capacity,
			Report: info.Report, State: w.state(now), HeartbeatAgeMilliseconds: uint64(age / time.Millisecond), LastError: info.Report.LastError}
		if w.probeError != "" {
			snapshot.LastError = w.probeError
		}
		if w.reservation != nil {
			requestID := w.reservation.RequestID
			snapshot.ReservedRequestId = &requestID
		}
		result = append(result, proto.Clone(snapshot).(*meshv1.WorkerInfo))
	}
	return result
}

// Expire reports each overdue reservation once for cancellation outside the
// mutex. It retains unavailable rows until receipt+10s+60s, even on late sweeps.
// Reported reservations remain held until Release or row deletion.
func (r *Registry) Expire(now time.Time) []Reservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cancellations []Reservation
	for i := 0; i < len(r.order); {
		workerID := r.order[i]
		w := r.workers[workerID]
		w.noteExpiry(now)
		if w.cancellation == cancellationPending {
			cancellations = append(cancellations, *w.reservation)
			w.cancellation = cancellationReported
		}
		if !now.Before(w.received.Add(membershipExpiry + unavailableRetention)) {
			delete(r.workers, workerID)
			r.order = append(r.order[:i], r.order[i+1:]...)
			if i < r.next {
				r.next--
			}
			if len(r.order) == 0 {
				r.next = 0
			} else {
				r.next %= len(r.order)
			}
			continue
		}
		i++
	}
	return cancellations
}

// ProbeTarget captures the current row, including unavailable retained entries.
// Its caller owns bounded network work and passes the result to ApplyProbe.
func (r *Registry) ProbeTarget(workerID string) (Probe, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[workerID]
	if !ok {
		return Probe{}, false
	}
	return Probe{WorkerID: workerID, Endpoint: w.info.Endpoint, Revision: w.revision}, true
}

// ApplyProbe returns true only for a current valid matching response. Stale
// results do nothing. A current failed, malformed, or wrong-identity result
// returns false but marks the row unhealthy and advances its revision. Successful
// observations also advance revision and never clear an active reservation.
// Probes do not refresh membership receipt time.
func (r *Registry) ApplyProbe(probe Probe, health *meshv1.HealthResponse, probeErr error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[probe.WorkerID]
	if !ok || w.info.Endpoint != probe.Endpoint || w.revision != probe.Revision {
		return false
	}
	w.revision = r.advance()
	switch {
	case probeErr != nil:
		w.probeError = "worker health probe failed"
	case health == nil:
		w.probeError = "worker health probe returned no response"
	case health.WorkerId != probe.WorkerID:
		w.probeError = "worker health probe identity mismatch"
	case validateReport(health.Report) != nil:
		w.probeError = "worker health probe returned an invalid report"
	default:
		w.info.Report = proto.Clone(health.Report).(*meshv1.WorkerReport)
		w.probeError = ""
		w.probeState = probePending
		if health.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY {
			w.probeState = probeIdle
			if health.Report.Active || w.reservation != nil {
				w.probeState = probeOccupied
			}
		}
		return true
	}
	w.probeState = probeFailed
	return false
}

func (r *Registry) advance() uint64          { r.sequence++; return r.sequence }
func (w *worker) expired(now time.Time) bool { return !now.Before(w.received.Add(membershipExpiry)) }
func (w *worker) noteExpiry(now time.Time) {
	if w.reservation != nil && w.cancellation == cancellationNone && w.expired(now) {
		w.cancellation = cancellationPending
	}
}
func (w *worker) state(now time.Time) meshv1.WorkerState {
	if w.expired(now) {
		return meshv1.WorkerState_WORKER_STATE_UNAVAILABLE
	}
	if w.probeState == probeFailed || w.info.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY {
		return meshv1.WorkerState_WORKER_STATE_UNHEALTHY
	}
	if w.reservation != nil || w.info.Report.Active {
		return meshv1.WorkerState_WORKER_STATE_BUSY
	}
	if w.info.Report.RuntimeState == meshv1.RuntimeState_RUNTIME_STATE_READY && w.probeState == probeIdle {
		return meshv1.WorkerState_WORKER_STATE_READY
	}
	return meshv1.WorkerState_WORKER_STATE_STARTING
}
func (r *Registry) validateRegistration(info *meshv1.RegisterWorkerRequest) error {
	if info == nil {
		return status.Error(codes.InvalidArgument, "worker registration is required")
	}
	if err := uuid.Validate(info.WorkerId); err != nil {
		return status.Error(codes.InvalidArgument, "worker_id must be a UUID")
	}
	host, portText, err := net.SplitHostPort(info.Endpoint)
	port, portErr := strconv.Atoi(portText)
	ip := net.ParseIP(host)
	if err != nil || portErr != nil || port < 1 || port > 65535 || ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return status.Error(codes.InvalidArgument, "endpoint must be a concrete IPv4 address with a port between 1 and 65535")
	}
	if info.ProtocolMajor != protocol.Major {
		return status.Error(codes.FailedPrecondition, "worker protocol major is incompatible")
	}
	if info.Capacity != 1 {
		return status.Error(codes.FailedPrecondition, "worker capacity must be one")
	}
	if info.Model == nil {
		return status.Error(codes.FailedPrecondition, "worker model descriptor is required")
	}
	digest, digestErr := hex.DecodeString(info.Model.Sha256)
	if info.Model.Id != r.model.ID || digestErr != nil || len(digest) != 32 || !strings.EqualFold(info.Model.Sha256, r.model.SHA256) || uint64(info.Model.ContextTokens) != uint64(r.model.ContextTokens) {
		return status.Error(codes.FailedPrecondition, "worker model descriptor is incompatible")
	}
	return validateReport(info.Report)
}
func validateReport(report *meshv1.WorkerReport) error {
	if report == nil {
		return status.Error(codes.InvalidArgument, "worker report is required")
	}
	switch report.RuntimeState {
	case meshv1.RuntimeState_RUNTIME_STATE_STARTING, meshv1.RuntimeState_RUNTIME_STATE_READY, meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY:
	default:
		return status.Error(codes.InvalidArgument, "worker runtime state is unsupported")
	}
	if report.ActiveRequestId != nil {
		if !report.Active {
			return status.Error(codes.InvalidArgument, "an idle worker cannot report an active request ID")
		}
		if err := uuid.Validate(*report.ActiveRequestId); err != nil {
			return status.Error(codes.InvalidArgument, "active_request_id must be a UUID")
		}
	}
	return nil
}
