package discovery

import (
	"context"
	"errors"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"net"
	"strings"
	"testing"
	"time"
)

const firstID = "9bfad16a-4010-4e03-9c63-8fa05a83b398"
const secondID = "93ed1402-a4ae-4ac5-ab9c-5781b4a142ff"

func candidate() ControllerInfo {
	return ControllerInfo{InstanceID: firstID, Hostname: "mac.local.", IPv4: "192.0.2.1", Port: 4321, ProtocolMajor: 1}
}
func entries(items ...ControllerInfo) browseFunc {
	return func(ctx context.Context, emit func(ControllerInfo)) error {
		for _, item := range items {
			emit(item)
		}
		return nil
	}
}
func TestExplicitAddressSkipsBrowse(t *testing.T) {
	got, err := resolve(context.Background(), "host.example:50051", func(context.Context, func(ControllerInfo)) error { t.Fatal("browsed explicit address"); return nil })
	if err != nil || got != "host.example:50051" {
		t.Fatalf("address=%q error=%v", got, err)
	}
}
func TestDuplicateInstanceIsOneController(t *testing.T) {
	c := candidate()
	d := c
	d.InstanceID = strings.ToUpper(d.InstanceID)
	d.IPv4 = "192.0.2.2"
	got, err := resolve(context.Background(), "", entries(c, d))
	if err != nil || got != "192.0.2.1:4321" {
		t.Fatalf("address=%q error=%v", got, err)
	}
}
func TestMultipleControllersRequireSelection(t *testing.T) {
	c := candidate()
	d := c
	d.InstanceID = secondID
	d.IPv4 = "192.0.2.2"
	_, err := resolve(context.Background(), "", entries(c, d))
	if err == nil || !strings.Contains(err.Error(), "192.0.2.1:4321") || !strings.Contains(err.Error(), "192.0.2.2:4321") || !strings.Contains(err.Error(), "--controller-address") {
		t.Fatalf("error=%v", err)
	}
}
func TestIncompatibleVersionIgnored(t *testing.T) {
	c := candidate()
	d := c
	d.InstanceID = secondID
	d.ProtocolMajor = 2
	got, err := resolve(context.Background(), "", entries(c, d))
	if err != nil || got != "192.0.2.1:4321" {
		t.Fatalf("address=%q error=%v", got, err)
	}
}
func TestNoControllerReportsFallback(t *testing.T) {
	_, err := resolve(context.Background(), "", entries())
	if err == nil || !strings.Contains(err.Error(), "--controller-address") {
		t.Fatalf("error=%v", err)
	}
}
func TestBrowseHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := resolve(ctx, "", func(ctx context.Context, emit func(ControllerInfo)) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Error("missing browse budget")
		}
		emit(candidate())
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
func TestRejectsInvalidCandidates(t *testing.T) {
	for _, ip := range []string{"0.0.0.0", "127.0.0.1", "::1", "224.0.0.1", "bad"} {
		c := candidate()
		c.IPv4 = ip
		_, err := resolve(context.Background(), "", entries(c))
		if err == nil {
			t.Errorf("accepted %s", ip)
		}
	}
}
func TestAmbiguousInterfaceNeedsOverride(t *testing.T) {
	addresses := []localAddress{{ip: net.ParseIP("192.0.2.1"), iface: net.Interface{Index: 1}}, {ip: net.ParseIP("192.0.2.2"), iface: net.Interface{Index: 2}}}
	if _, err := selectAddress("", net.IPv4zero, addresses); err == nil || !strings.Contains(err.Error(), "--advertise-address") {
		t.Fatalf("error=%v", err)
	}
	got, err := selectAddress("192.0.2.2", net.IPv4zero, addresses)
	if err != nil || !got.ip.Equal(addresses[1].ip) {
		t.Fatalf("address=%v error=%v", got, err)
	}
	for _, override := range []string{"192.0.2.3", "127.0.0.1", "0.0.0.0"} {
		if _, err := selectAddress(override, net.IPv4zero, addresses); err == nil {
			t.Errorf("accepted %s", override)
		}
	}
	if _, err := selectAddress("192.0.2.2", net.ParseIP("192.0.2.1"), addresses); err == nil {
		t.Error("accepted address outside listener")
	}
}

func TestCollectsFullBrowseWindow(t *testing.T) {
	start := time.Now()
	got, err := resolve(context.Background(), "", func(ctx context.Context, emit func(ControllerInfo)) error {
		emit(candidate())
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil || got != "192.0.2.1:4321" {
		t.Fatalf("address=%q error=%v", got, err)
	}
	if time.Since(start) < browseBudget {
		t.Fatal("selected before collecting full interval")
	}
}
func TestEarlierDeadlineWinsEvenWithCandidate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := resolve(ctx, "", func(ctx context.Context, emit func(ControllerInfo)) error {
		emit(candidate())
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}
func TestAdvertisementOverflowFailsClosed(t *testing.T) {
	_, err := resolve(context.Background(), "", func(ctx context.Context, emit func(ControllerInfo)) error {
		for range maxEntries + 1 {
			emit(candidate())
		}
		if ctx.Err() == nil {
			t.Error("overflow did not cancel browse")
		}
		return ctx.Err()
	})
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("error=%v", err)
	}
}
func TestServiceWireName(t *testing.T) {
	if service+"."+domain != ServiceName || ServiceName != "_mica-mesh._tcp.local." {
		t.Fatal("incompatible DNS-SD service")
	}
}
func TestInvalidAdvertisementBeforeMulticast(t *testing.T) {
	for _, ip := range []string{"0.0.0.0", "127.0.0.1", "::1", "224.0.0.1"} {
		info := candidate()
		info.IPv4 = ip
		if stop, err := Advertise(context.Background(), info); err == nil {
			stop()
			t.Fatalf("advertised %s", ip)
		}
	}
}

func TestAdvertisementStopIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	stop := stopAdvertisement(ctx, func() { calls++ })
	stop()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second stop deadlocked after preventing cancellation callback")
	}
	if calls != 1 {
		t.Fatalf("shutdown calls=%d", calls)
	}
}

func TestAdvertisementCancellationJoinsCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	stop := stopAdvertisement(ctx, func() { close(entered); <-release })
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cancellation missed shutdown")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
		t.Fatal("stop returned before cleanup")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not join cleanup")
	}
	stop()
}

func TestDiscoveryStillRequiresReverseWorkerConnectivity(t *testing.T) {
	target, err := resolve(context.Background(), "", entries(candidate()))
	if err != nil || target == "" {
		t.Fatal(err)
	}
	model := config.Default().ModelDescriptor
	svc := controller.New(firstID, controller.NewRegistry(model))
	defer svc.Close()
	// Membership is accepted before the probe owner starts. Successful discovery
	// must not substitute for the controller's later, independent Health proof.
	_, err = svc.RegisterWorker(context.Background(), &meshv1.RegisterWorkerRequest{WorkerId: secondID, Endpoint: "192.0.2.2:50052", ProtocolMajor: 1, Hardware: &meshv1.HardwareInfo{Hostname: "worker"}, Model: &meshv1.ModelDescriptor{Id: model.ID, Sha256: model.SHA256, ContextTokens: uint32(model.ContextTokens)}, Capacity: 1, Report: &meshv1.WorkerReport{RuntimeState: meshv1.RuntimeState_RUNTIME_STATE_READY}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := svc.GetClusterStatus(context.Background(), &meshv1.GetClusterStatusRequest{})
	if err != nil || len(state.Workers) != 1 || state.Workers[0].State != meshv1.WorkerState_WORKER_STATE_STARTING {
		t.Fatalf("status=%v error=%v", state, err)
	}
}

func TestBrowseInterfacesFilterIPv4LANs(t *testing.T) {
	const eligible = net.FlagUp | net.FlagMulticast
	tests := []struct {
		name  string
		ip    string
		flags net.Flags
		want  bool
	}{
		{"LAN", "192.0.2.1", eligible, true},
		{"link-local", "169.254.1.2", eligible, true},
		{"down", "192.0.2.1", net.FlagMulticast, false},
		{"no-multicast", "192.0.2.1", net.FlagUp, false},
		{"loopback-interface", "192.0.2.1", eligible | net.FlagLoopback, false},
		{"loopback-address", "127.0.0.1", eligible, false},
		{"IPv6-only", "fe80::1", eligible, false},
		{"unspecified", "0.0.0.0", eligible, false},
		{"without-address", "", eligible, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := browseInterfaces([]localAddress{{ip: net.ParseIP(test.ip), iface: net.Interface{Index: 7, Flags: test.flags}}})
			if test.want {
				if err != nil || len(got) != 1 || got[0].Index != 7 {
					t.Fatalf("interfaces=%v error=%v", got, err)
				}
			} else if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "--controller-address") {
				t.Fatalf("ineligible interface accepted or missing fallback: interfaces=%v error=%v", got, err)
			}
		})
	}
}

func TestBrowseInterfacesDeduplicateAndKeepAllLANs(t *testing.T) {
	first := net.Interface{Index: 7, Flags: net.FlagUp | net.FlagMulticast}
	second := net.Interface{Index: 9, Flags: net.FlagUp | net.FlagMulticast}
	got, err := browseInterfaces([]localAddress{
		{ip: net.ParseIP("192.0.2.1"), iface: first},
		{ip: net.ParseIP("192.0.2.2"), iface: first},
		{ip: net.ParseIP("198.51.100.1"), iface: second},
	})
	if err != nil || len(got) != 2 || got[0].Index != first.Index || got[1].Index != second.Index {
		t.Fatalf("interfaces=%v error=%v", got, err)
	}
}

func TestNoBrowseInterfacesReportsFallback(t *testing.T) {
	if got, err := browseInterfaces(nil); err == nil || len(got) != 0 || !strings.Contains(err.Error(), "--controller-address") {
		t.Fatalf("empty selection could reactivate upstream defaults: interfaces=%v error=%v", got, err)
	}
}

func TestCandidatesAreBoundedCopiedAndSorted(t *testing.T) {
	a := candidate()
	b := a
	b.InstanceID = secondID
	b.IPv4 = "192.0.2.2"
	b.Hostname = "other.local."
	duplicate := a
	duplicate.InstanceID = strings.ToUpper(a.InstanceID)
	duplicate.Hostname = "aaa.local."
	input := []ControllerInfo{b, a, duplicate}
	got, err := candidates(context.Background(), entries(input...))
	if err != nil || len(got) != 2 {
		t.Fatalf("candidates=%v error=%v", got, err)
	}
	if got[0].InstanceID != firstID || got[0].Hostname != "aaa.local." || got[0].Address != "192.0.2.1:4321" || got[1].Address != "192.0.2.2:4321" {
		t.Fatalf("choices=%+v", got)
	}
	got[0].Hostname = "mutated"
	again, err := candidates(context.Background(), entries(input...))
	if err != nil || again[0].Hostname != "aaa.local." {
		t.Fatalf("caller changed snapshot: %v %v", again, err)
	}
	tooMany := make([]ControllerInfo, 257)
	if _, err := candidates(context.Background(), entries(tooMany...)); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("invalid entries escaped bound: %v", err)
	}
}
func TestCandidatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := candidates(ctx, func(ctx context.Context, emit func(ControllerInfo)) error {
		emit(candidate())
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
func TestNamedInterfaceAddresses(t *testing.T) {
	input := []localAddress{
		{ip: net.ParseIP("192.0.2.2"), iface: net.Interface{Name: "en1", Flags: net.FlagUp}},
		{ip: net.ParseIP("127.0.0.1"), iface: net.Interface{Name: "lo0", Flags: net.FlagUp}},
		{ip: net.ParseIP("192.0.2.1"), iface: net.Interface{Name: "en0", Flags: net.FlagUp}},
		{ip: net.ParseIP("192.0.2.3"), iface: net.Interface{Name: "down"}},
	}
	got := interfaceAddresses(input)
	if len(got) != 2 || got[0].Name != "en0" || got[0].IPv4 != "192.0.2.1" || got[1].Name != "en1" {
		t.Fatalf("choices=%v", got)
	}
	got[0].Name = "changed"
	if next := interfaceAddresses(input); next[0].Name != "en0" {
		t.Fatal("snapshot aliases input")
	}
}
