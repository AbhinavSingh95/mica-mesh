package doctor

import (
	"context"
	"errors"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshruntime "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"github.com/AbhinavSingh95/mica-mesh/internal/testutil/fakeruntime"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDoctorRoleScopesChecks(t *testing.T) {
	for _, role := range []Role{Controller, Client} {
		cfg := config.Default()
		cfg.ControllerListen = freeAddress(t)
		report, err := Run(context.Background(), cfg, setup.Layout{}, role, Options{Mode: Preflight})
		if err != nil || len(report.Checks) == 0 {
			t.Fatalf("%s report=%+v err=%v", role, report, err)
		}
		for _, check := range report.Checks {
			if check.State == Failed {
				t.Errorf("%s requires Agent files: %+v", role, check)
			}
		}
	}
}
func TestDoctorReportsAction(t *testing.T) {
	report, err := Run(context.Background(), config.Default(), setup.Layout{ReleaseRoot: t.TempDir(), DataRoot: t.TempDir()}, Agent, Options{Mode: Preflight})
	failed := false
	for _, check := range report.Checks {
		if check.State == Failed {
			failed = true
			if check.Action == "" {
				t.Errorf("no recovery: %+v", check)
			}
		}
	}
	if err != nil || !failed {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}
func TestDoctorHasNoOutboundOrMutationEffects(t *testing.T) {
	cfg := config.Default()
	cfg.ControllerAddress = "invalid.invalid:1234"
	root := t.TempDir()
	data := filepath.Join(root, "absent")
	report, err := Run(context.Background(), cfg, setup.Layout{ReleaseRoot: root, DataRoot: data}, Client, Options{Mode: Preflight})
	if err != nil || len(report.Checks) == 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("doctor changed root: %v %v", entries, err)
	}
}
func doctorFixture(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.WorkerListen = freeAddress(t)
	cfg.RuntimeBinary = filepath.Join(t.TempDir(), "server")
	cfg.ModelPath = filepath.Join(t.TempDir(), "model")
	for _, p := range []string{cfg.RuntimeBinary, cfg.ModelPath} {
		if err := os.WriteFile(p, []byte("fixture"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimePort = l.Addr().(*net.TCPAddr).Port
	l.Close()
	return cfg
}
func TestDoctorPreservesPortOwner(t *testing.T) {
	cfg := doctorFixture(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cfg.RuntimePort = l.Addr().(*net.TCPAddr).Port
	created := false
	report, err := run(context.Background(), cfg, setup.Layout{}, Agent, Options{Mode: Probe}, func() meshruntime.Runtime { created = true; return fakeruntime.New() })
	failed := false
	for _, check := range report.Checks {
		if check.Name == "runtime port" && check.State == Failed && check.Action != "" {
			failed = true
		}
	}
	if err != nil || !failed || created {
		t.Fatalf("report=%+v created=%v err=%v", report, created, err)
	}
	conn, err := net.DialTimeout("tcp4", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("port owner lost: %v", err)
	}
	conn.Close()
}

func TestDiagnosisDistinguishesActivePortsFromStartupConflicts(t *testing.T) {
	for _, role := range []Role{Agent, Controller} {
		t.Run(string(role), func(t *testing.T) {
			cfg := doctorFixture(t)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			cfg.WorkerListen = listener.Addr().String()
			cfg.ControllerListen = listener.Addr().String()
			cfg.RuntimePort = listener.Addr().(*net.TCPAddr).Port
			ports := []string{"controller port"}
			if role == Agent {
				ports = []string{"Agent port", "runtime port"}
			}
			for _, mode := range []Mode{Preflight, Active} {
				report, err := run(context.Background(), cfg, setup.Layout{}, role, Options{Mode: mode}, func() meshruntime.Runtime {
					t.Fatal("diagnosis started an extra runtime")
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range ports {
					found := false
					for _, c := range report.Checks {
						if c.Name != name {
							continue
						}
						found = true
						if mode == Preflight && (c.State != Failed || c.Action == "") || mode == Active && (c.State != NotChecked || c.Action != "") {
							t.Errorf("mode %d: %+v", mode, c)
						}
					}
					if !found {
						t.Errorf("missing %s check", name)
					}
				}
			}
		})
	}
}

func TestActiveDiagnosisStillReportsMissingFiles(t *testing.T) {
	cfg := doctorFixture(t)
	if err := os.Remove(cfg.ModelPath); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), cfg, setup.Layout{}, Agent, Options{Mode: Active})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Name == "model file" && c.State == Failed && c.Action != "" {
			return
		}
	}
	t.Fatalf("missing model was hidden: %+v", report)
}

func TestDoctorProbeAlwaysReaps(t *testing.T) {
	for _, cancelStart := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "canceled"}[cancelStart], func(t *testing.T) {
			cfg := doctorFixture(t)
			rt := &observedRuntime{Runtime: fakeruntime.New()}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelStart {
				rt.StartGate = make(chan struct{})
			}
			done := make(chan struct{})
			var report Report
			var err error
			go func() {
				defer close(done)
				report, err = run(ctx, cfg, setup.Layout{}, Agent, Options{Mode: Probe}, func() meshruntime.Runtime { return rt })
			}()
			select {
			case <-rt.Started:
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not start")
			}
			if cancelStart {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not return")
			}
			if rt.stops != 1 || !rt.boundedCleanup || rt.canceledCleanup {
				t.Fatalf("cleanup: stops=%d bounded=%v canceled=%v", rt.stops, rt.boundedCleanup, rt.canceledCleanup)
			}
			health, _ := rt.Health(context.Background())
			if health.State != meshruntime.StateUnhealthy {
				t.Fatalf("runtime left ready: %+v", health)
			}
			if !cancelStart && err != nil {
				t.Fatal(err)
			}
			if cancelStart && !errors.Is(err, context.Canceled) {
				found := false
				for _, c := range report.Checks {
					found = found || c.State == Failed
				}
				if !found {
					t.Fatalf("canceled probe passed: %+v %v", report, err)
				}
			}
		})
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// Observe the runtime ownership boundary, including partial startup failure.
type observedRuntime struct {
	*fakeruntime.Runtime
	stops                           int
	boundedCleanup, canceledCleanup bool
}

func (r *observedRuntime) Stop(ctx context.Context) error {
	r.stops++
	_, r.boundedCleanup = ctx.Deadline()
	r.canceledCleanup = ctx.Err() != nil
	return r.Runtime.Stop(ctx)
}
func TestBasicDoctorDoesNotCreateRuntime(t *testing.T) {
	cfg := doctorFixture(t)
	root := t.TempDir()
	report, err := run(context.Background(), cfg, setup.Layout{DataRoot: filepath.Join(root, "absent")}, Agent, Options{Mode: Preflight}, func() meshruntime.Runtime { t.Fatal("basic doctor created a runtime"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.State == Failed {
			t.Errorf("manual files require managed setup: %+v", check)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("doctor changed files: %v %v", entries, err)
	}
}
