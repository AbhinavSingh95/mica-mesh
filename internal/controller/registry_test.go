package controller_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var receipt = time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)

func model() runtime.Model {
	return runtime.Model{ID: "configured-model", SHA256: strings.Repeat("ab", 32), ContextTokens: 4096}
}
func id(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }
func report() *meshv1.WorkerReport {
	return &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}
}
func registration(n int) *meshv1.RegisterWorkerRequest {
	m := model()
	cpu := "test cpu"
	return &meshv1.RegisterWorkerRequest{WorkerId: id(n), Endpoint: fmt.Sprintf("192.0.2.%d:50052", n), ProtocolMajor: 1,
		Hardware: &meshv1.HardwareInfo{Hostname: fmt.Sprintf("worker-%d", n), Cpu: &cpu},
		Model:    &meshv1.ModelDescriptor{Id: m.ID, Sha256: m.SHA256, ContextTokens: uint32(m.ContextTokens)}, Capacity: 1, Report: report()}
}
func newRegistry() *controller.Registry { return controller.NewRegistry(model()) }
func register(t *testing.T, r *controller.Registry, n int) {
	t.Helper()
	if err := r.Register(registration(n), receipt); err != nil {
		t.Fatal(err)
	}
}
func probe(t *testing.T, r *controller.Registry, n int, healthReport *meshv1.WorkerReport) {
	t.Helper()
	p, ok := r.ProbeTarget(id(n))
	if !ok {
		t.Fatalf("missing probe target for worker %d", n)
	}
	if !r.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: id(n), Report: healthReport}, nil) {
		t.Fatal("current matching health probe rejected")
	}
}
func ready(t *testing.T, r *controller.Registry, n int) {
	t.Helper()
	register(t, r, n)
	probe(t, r, n, report())
}
func reserve(t *testing.T, r *controller.Registry, request int, now time.Time) controller.Reservation {
	t.Helper()
	got, err := r.Reserve(model().ID, id(request), now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func code(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("code = %s, want %s (error %v)", got, want, err)
	}
}
func state(t *testing.T, r *controller.Registry, now time.Time, want meshv1.WorkerState) *meshv1.WorkerInfo {
	t.Helper()
	workers := r.Snapshot(now)
	if len(workers) != 1 {
		t.Fatalf("snapshot has %d workers, want 1", len(workers))
	}
	if workers[0].State != want {
		t.Fatalf("state = %s, want %s", workers[0].State, want)
	}
	return workers[0]
}

func TestRegisterIdempotentPreservesReservation(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	updated := registration(1)
	updated.Hardware.Hostname = "updated"
	if err := r.Register(updated, receipt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	probe(t, r, 1, report())
	s := state(t, r, receipt.Add(time.Second), meshv1.WorkerState_WORKER_STATE_BUSY)
	if s.GetReservedRequestId() != held.RequestID || s.Hardware.Hostname != "updated" {
		t.Fatalf("registration lost reservation or metadata: %v", s)
	}
	_, err := r.Reserve(model().ID, id(102), receipt.Add(time.Second))
	code(t, err, codes.ResourceExhausted)
	if !r.Release(held) {
		t.Fatal("reservation did not survive registration")
	}
}
func TestEndpointChangeRejected(t *testing.T) {
	r := newRegistry()
	register(t, r, 1)
	changed := registration(1)
	changed.Endpoint = "192.0.2.2:50052"
	err := r.Register(changed, receipt)
	code(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("missing restart instruction: %v", err)
	}
	if got := r.Snapshot(receipt)[0].Endpoint; got != registration(1).Endpoint {
		t.Fatalf("endpoint changed to %q", got)
	}
}
func TestUnknownHeartbeat(t *testing.T) {
	r := newRegistry()
	code(t, r.Heartbeat(id(1), report(), receipt), codes.NotFound)
}
func TestRoundRobin(t *testing.T) {
	r := newRegistry()
	for n := 1; n <= 3; n++ {
		ready(t, r, n)
	}
	for i, want := range []int{1, 2, 3, 1, 2, 3} {
		held := reserve(t, r, 101+i, receipt)
		if held.WorkerID != id(want) {
			t.Fatalf("turn %d chose %q, want worker %d", i, held.WorkerID, want)
		}
		if !r.Release(held) {
			t.Fatal("release failed")
		}
		probe(t, r, want, report())
	}
}
func TestConcurrentReserveCapacityOne(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	start := make(chan struct{})
	results := make(chan error, 32)
	held := make(chan controller.Reservation, 32)
	for i := 0; i < 32; i++ {
		go func(n int) {
			<-start
			res, err := r.Reserve(model().ID, id(101+n), receipt)
			if err == nil {
				held <- res
			}
			results <- err
		}(i)
	}
	close(start)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	successes := 0
	for i := 0; i < 32; i++ {
		select {
		case err := <-results:
			if err == nil {
				successes++
			} else {
				code(t, err, codes.ResourceExhausted)
			}
		case <-timer.C:
			t.Fatal("concurrent reservations did not terminate")
		}
	}
	if successes != 1 {
		t.Fatalf("%d reservations admitted, want 1", successes)
	}
	if !r.Release(<-held) {
		t.Fatal("winning reservation not releasable")
	}
}
func TestEmptyVersusBusy(t *testing.T) {
	r := newRegistry()
	_, err := r.Reserve("unknown", id(101), receipt)
	code(t, err, codes.NotFound)
	_, err = r.Reserve(model().ID, id(101), receipt)
	code(t, err, codes.Unavailable)
	register(t, r, 1)
	_, err = r.Reserve(model().ID, id(101), receipt)
	code(t, err, codes.Unavailable)
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_STARTING)
	probe(t, r, 1, report())
	reserve(t, r, 101, receipt)
	_, err = r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.ResourceExhausted)
}
func TestReleaseExactlyOnce(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	wrong := held
	wrong.Endpoint = "192.0.2.2:50052"
	if r.Release(wrong) {
		t.Fatal("release accepted wrong endpoint")
	}
	wrong = held
	wrong.RequestID = id(102)
	if r.Release(wrong) {
		t.Fatal("release accepted wrong request")
	}
	if !r.Release(held) || r.Release(held) {
		t.Fatal("release must succeed exactly once")
	}
	_, err := r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.Unavailable)
	if err := r.Heartbeat(id(1), report(), receipt); err != nil {
		t.Fatal(err)
	}
	_, err = r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.Unavailable)
	probe(t, r, 1, report())
	reserve(t, r, 102, receipt)
}
func TestOldReleaseCannotClearNewReservation(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	old := reserve(t, r, 101, receipt)
	if !r.Release(old) {
		t.Fatal("release failed")
	}
	probe(t, r, 1, report())
	next := reserve(t, r, 101, receipt)
	if next.Generation <= old.Generation {
		t.Fatal("generation did not advance for repeated request ID")
	}
	if r.Release(old) {
		t.Fatal("old release cleared new reservation")
	}
	if !r.Release(next) {
		t.Fatal("new reservation lost")
	}
}
func TestLateProbeCannotRestoreReady(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	old, _ := r.ProbeTarget(id(1))
	held := reserve(t, r, 101, receipt)
	if r.ApplyProbe(old, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("pre-admission probe accepted")
	}
	probe(t, r, 1, report())
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_BUSY)
	heldProbe, _ := r.ProbeTarget(id(1))
	if !r.Release(held) {
		t.Fatal("release failed")
	}
	if r.ApplyProbe(heldProbe, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("pre-release probe accepted")
	}
	_, err := r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.Unavailable)
}
func TestExpiryUsesReceiptTime(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	state(t, r, receipt.Add(10*time.Second-time.Nanosecond), meshv1.WorkerState_WORKER_STATE_BUSY)
	s := state(t, r, receipt.Add(10*time.Second), meshv1.WorkerState_WORKER_STATE_UNAVAILABLE)
	if s.HeartbeatAgeMilliseconds != 10000 {
		t.Fatalf("age = %d, want 10000", s.HeartbeatAgeMilliseconds)
	}
	_, err := r.Reserve(model().ID, id(102), receipt.Add(10*time.Second))
	code(t, err, codes.Unavailable)
	code(t, r.Heartbeat(id(1), report(), receipt.Add(10*time.Second)), codes.NotFound)
	cancelled := r.Expire(receipt.Add(11 * time.Second))
	if len(cancelled) != 1 || cancelled[0] != held {
		t.Fatalf("expiry cancelled %v, want %v", cancelled, held)
	}
	if got := r.Expire(receipt.Add(12 * time.Second)); len(got) != 0 {
		t.Fatalf("duplicate cancellation: %v", got)
	}
	if len(r.Snapshot(receipt.Add(70*time.Second-time.Nanosecond))) != 1 {
		t.Fatal("retention ended early")
	}
	r.Expire(receipt.Add(70 * time.Second))
	if len(r.Snapshot(receipt.Add(70*time.Second))) != 0 {
		t.Fatal("expired worker retained past deletion deadline")
	}
}
func TestHeartbeatRefreshDoesNotProbeOrLoseReservation(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	if err := r.Heartbeat(id(1), report(), receipt.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	state(t, r, receipt.Add(18*time.Second), meshv1.WorkerState_WORKER_STATE_BUSY)
	if !r.Release(held) {
		t.Fatal("heartbeat lost reservation")
	}
	state(t, r, receipt.Add(18*time.Second), meshv1.WorkerState_WORKER_STATE_STARTING)
	state(t, r, receipt.Add(19*time.Second), meshv1.WorkerState_WORKER_STATE_UNAVAILABLE)
}
func TestReregistrationPreservesOverdueCancellation(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	if err := r.Register(registration(1), receipt.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	probe(t, r, 1, report())
	state(t, r, receipt.Add(11*time.Second), meshv1.WorkerState_WORKER_STATE_BUSY)
	got := r.Expire(receipt.Add(11 * time.Second))
	if len(got) != 1 || got[0] != held {
		t.Fatalf("late register hid cancellation: %v", got)
	}
	if len(r.Expire(receipt.Add(12*time.Second))) != 0 {
		t.Fatal("expiry repeated cancellation")
	}
	if !r.Release(held) {
		t.Fatal("expiry erased held reservation")
	}
}
func TestDelayedSweepUsesOriginalDeletionDeadline(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	got := r.Expire(receipt.Add(100 * time.Second))
	if len(got) != 1 || got[0] != held {
		t.Fatalf("delayed expiry lost cancellation: %v", got)
	}
	if len(r.Snapshot(receipt.Add(100*time.Second))) != 0 {
		t.Fatal("delayed sweep extended retention")
	}
	if r.Release(held) {
		t.Fatal("deleted row retained reservation")
	}
}
func TestRecreatedRowRejectsOldIdentities(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	oldProbe, _ := r.ProbeTarget(id(1))
	old := reserve(t, r, 101, receipt)
	r.Expire(receipt.Add(70 * time.Second))
	if err := r.Register(registration(1), receipt.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	if r.ApplyProbe(oldProbe, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("deleted-row probe accepted")
	}
	probe(t, r, 1, report())
	next := reserve(t, r, 101, receipt.Add(70*time.Second))
	if next.Generation <= old.Generation || r.Release(old) {
		t.Fatal("deleted-row release affected recreated row")
	}
}
func TestProbeValidationAndRevision(t *testing.T) {
	cases := []struct {
		name   string
		health *meshv1.HealthResponse
		err    error
	}{
		{"failure", nil, errors.New("network failed")}, {"nil", nil, nil},
		{"wrong identity", &meshv1.HealthResponse{WorkerId: id(2), Report: report()}, nil},
		{"invalid report", &meshv1.HealthResponse{WorkerId: id(1), Report: &meshv1.WorkerReport{RuntimeState: 99}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry()
			ready(t, r, 1)
			p, _ := r.ProbeTarget(id(1))
			if r.ApplyProbe(p, tc.health, tc.err) {
				t.Fatal("failed or malformed probe returned success")
			}
			state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_UNHEALTHY)
			if r.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
				t.Fatal("failed observation did not invalidate revision")
			}
			probe(t, r, 1, report())
			state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_READY)
			stale, _ := r.ProbeTarget(id(1))
			probe(t, r, 1, report())
			if r.ApplyProbe(stale, nil, errors.New("old failure")) {
				t.Fatal("stale failure accepted")
			}
			state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_READY)
		})
	}
}
func TestProbeDoesNotRefreshMembership(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	probe(t, r, 1, report())
	state(t, r, receipt.Add(10*time.Second), meshv1.WorkerState_WORKER_STATE_UNAVAILABLE)
}
func TestReportedActivityRequiresIdleProbe(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	active := report()
	active.Active = true
	if err := r.Heartbeat(id(1), active, receipt); err != nil {
		t.Fatal(err)
	}
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_BUSY)
	_, err := r.Reserve(model().ID, id(101), receipt)
	code(t, err, codes.ResourceExhausted)
	if err := r.Heartbeat(id(1), report(), receipt); err != nil {
		t.Fatal(err)
	}
	_, err = r.Reserve(model().ID, id(101), receipt)
	code(t, err, codes.Unavailable)
	probe(t, r, 1, active)
	_, err = r.Reserve(model().ID, id(101), receipt)
	code(t, err, codes.ResourceExhausted)
	probe(t, r, 1, report())
	reserve(t, r, 101, receipt)
}
func TestRegistryOwnsInputAndSnapshots(t *testing.T) {
	r := newRegistry()
	in := registration(1)
	in.Hardware.RamBytes = new(uint64)
	*in.Hardware.RamBytes = 4096
	if err := r.Register(in, receipt); err != nil {
		t.Fatal(err)
	}
	probe(t, r, 1, report())
	in.Hardware.Hostname = "mutated"
	*in.Hardware.Cpu = "mutated"
	*in.Hardware.RamBytes = 0
	in.Model.Id = "mutated"
	in.Report.Active = true
	s := state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_READY)
	if s.Hardware.Hostname != "worker-1" || s.Hardware.GetCpu() != "test cpu" || s.Hardware.GetRamBytes() != 4096 || s.Model.Id != model().ID || s.Report.Active {
		t.Fatal("registration aliases caller input")
	}
	s.Hardware.Hostname = "mutated snapshot"
	*s.Hardware.Cpu = "mutated"
	s.Model.Id = "mutated"
	s.Report.Active = true
	active := report()
	active.Active = true
	req := id(101)
	active.ActiveRequestId = &req
	if err := r.Heartbeat(id(1), active, receipt); err != nil {
		t.Fatal(err)
	}
	active.Active = false
	*active.ActiveRequestId = id(102)
	s = state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_BUSY)
	if s.Hardware.Hostname != "worker-1" || s.Hardware.GetCpu() != "test cpu" || s.Model.Id != model().ID || !s.Report.Active || s.Report.GetActiveRequestId() != id(101) {
		t.Fatal("snapshot or heartbeat aliases caller input")
	}
	health := report()
	probe(t, r, 1, health)
	health.RuntimeState = meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_READY)
}
func TestRegistrationValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*meshv1.RegisterWorkerRequest)
		want   codes.Code
	}{
		{"worker UUID", func(i *meshv1.RegisterWorkerRequest) { i.WorkerId = "bad" }, codes.InvalidArgument},
		{"endpoint", func(i *meshv1.RegisterWorkerRequest) { i.Endpoint = "bad" }, codes.InvalidArgument},
		{"wildcard endpoint", func(i *meshv1.RegisterWorkerRequest) { i.Endpoint = "0.0.0.0:50052" }, codes.InvalidArgument},
		{"zero port", func(i *meshv1.RegisterWorkerRequest) { i.Endpoint = "192.0.2.1:0" }, codes.InvalidArgument},
		{"capacity", func(i *meshv1.RegisterWorkerRequest) { i.Capacity = 2 }, codes.FailedPrecondition},
		{"protocol", func(i *meshv1.RegisterWorkerRequest) { i.ProtocolMajor = 2 }, codes.FailedPrecondition},
		{"model ID", func(i *meshv1.RegisterWorkerRequest) { i.Model.Id = "other" }, codes.FailedPrecondition},
		{"model digest", func(i *meshv1.RegisterWorkerRequest) { i.Model.Sha256 = strings.Repeat("cd", 32) }, codes.FailedPrecondition},
		{"malformed digest", func(i *meshv1.RegisterWorkerRequest) { i.Model.Sha256 = "bad" }, codes.FailedPrecondition},
		{"context", func(i *meshv1.RegisterWorkerRequest) { i.Model.ContextTokens = 2048 }, codes.FailedPrecondition},
		{"missing model", func(i *meshv1.RegisterWorkerRequest) { i.Model = nil }, codes.FailedPrecondition},
		{"missing report", func(i *meshv1.RegisterWorkerRequest) { i.Report = nil }, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry()
			in := registration(1)
			tc.mutate(in)
			code(t, r.Register(in, receipt), tc.want)
			if len(r.Snapshot(receipt)) != 0 {
				t.Fatal("invalid registration retained")
			}
		})
	}
	code(t, newRegistry().Register(nil, receipt), codes.InvalidArgument)
	r := newRegistry()
	in := registration(1)
	in.Model.Sha256 = strings.ToUpper(in.Model.Sha256)
	if err := r.Register(in, receipt); err != nil {
		t.Fatalf("equivalent hex digest rejected: %v", err)
	}
}
func TestReportValidationDoesNotMutate(t *testing.T) {
	req := id(101)
	cases := []struct {
		name  string
		value *meshv1.WorkerReport
	}{
		{"nil", nil}, {"unspecified", &meshv1.WorkerReport{}}, {"unknown", &meshv1.WorkerReport{RuntimeState: 99}},
		{"idle ID", &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY, ActiveRequestId: &req}},
		{"invalid active ID", &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY, Active: true, ActiveRequestId: new(string)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRegistry()
			ready(t, r, 1)
			p, _ := r.ProbeTarget(id(1))
			code(t, r.Heartbeat(id(1), tc.value, receipt.Add(time.Second)), codes.InvalidArgument)
			state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_READY)
			if !r.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
				t.Fatal("invalid heartbeat mutated row")
			}
			in := registration(2)
			in.Report = tc.value
			code(t, r.Register(in, receipt), codes.InvalidArgument)
		})
	}
	r := newRegistry()
	ready(t, r, 1)
	unhealthy := &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_UNHEALTHY, Active: true}
	if err := r.Heartbeat(id(1), unhealthy, receipt); err != nil {
		t.Fatalf("cleanup activity rejected: %v", err)
	}
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_UNHEALTHY)
	_, err := r.Reserve(model().ID, "bad", receipt)
	code(t, err, codes.InvalidArgument)
	code(t, r.Heartbeat("bad", report(), receipt), codes.InvalidArgument)
}

func TestReleasePreservesConfirmedCleanupOccupancy(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	held := reserve(t, r, 101, receipt)
	active := report()
	active.Active = true
	probe(t, r, 1, active)
	if !r.Release(held) {
		t.Fatal("release failed")
	}
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_BUSY)
	_, err := r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.ResourceExhausted)
	if err := r.Heartbeat(id(1), report(), receipt); err != nil {
		t.Fatal(err)
	}
	_, err = r.Reserve(model().ID, id(102), receipt)
	code(t, err, codes.Unavailable)
	probe(t, r, 1, report())
	reserve(t, r, 102, receipt)
}

func TestMetadataAndReportMutationsInvalidateProbes(t *testing.T) {
	r := newRegistry()
	ready(t, r, 1)
	p, _ := r.ProbeTarget(id(1))
	starting := &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_STARTING}
	if err := r.Heartbeat(id(1), starting, receipt); err != nil {
		t.Fatal(err)
	}
	if r.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("pre-heartbeat probe accepted")
	}
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_STARTING)
	p, _ = r.ProbeTarget(id(1))
	register(t, r, 1)
	if r.ApplyProbe(p, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("pre-registration probe accepted")
	}
	state(t, r, receipt, meshv1.WorkerState_WORKER_STATE_STARTING)
	wrong, _ := r.ProbeTarget(id(1))
	wrong.Endpoint = "192.0.2.2:50052"
	if r.ApplyProbe(wrong, &meshv1.HealthResponse{WorkerId: id(1), Report: report()}, nil) {
		t.Fatal("wrong endpoint probe accepted")
	}
	probe(t, r, 1, report())
}

func TestRoundRobinAfterDeletionAndReregistration(t *testing.T) {
	r := newRegistry()
	for n := 1; n <= 3; n++ {
		ready(t, r, n)
	}
	first := reserve(t, r, 101, receipt)
	if first.WorkerID != id(1) {
		t.Fatal("first admission did not select worker 1")
	}
	// Preserve rows 2 and 3 through row 1's deletion deadline.
	for _, seconds := range []int{9, 18, 27, 36, 45, 54, 63} {
		for n := 2; n <= 3; n++ {
			if err := r.Heartbeat(id(n), report(), receipt.Add(time.Duration(seconds)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.Expire(receipt.Add(70 * time.Second))
	next := reserve(t, r, 102, receipt.Add(70*time.Second))
	if next.WorkerID != id(2) {
		t.Fatalf("deletion skipped worker 2: %v", next)
	}
	if !r.Release(next) {
		t.Fatal("release failed")
	}
	probe(t, r, 2, report())
	if err := r.Register(registration(1), receipt.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	probe(t, r, 1, report())
	workers := r.Snapshot(receipt.Add(70 * time.Second))
	if len(workers) != 3 || workers[0].WorkerId != id(2) || workers[1].WorkerId != id(3) || workers[2].WorkerId != id(1) {
		t.Fatalf("unstable/duplicated snapshot order: %v", workers)
	}
	third := reserve(t, r, 103, receipt.Add(70*time.Second))
	if third.WorkerID != id(3) {
		t.Fatalf("reregistration skipped worker 3: %v", third)
	}
	fourth := reserve(t, r, 104, receipt.Add(70*time.Second))
	if fourth.WorkerID != id(1) {
		t.Fatalf("recreated worker never eligible: %v", fourth)
	}
}
