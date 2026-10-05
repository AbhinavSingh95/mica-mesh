// Package doctor checks local readiness without repairs or outbound connections.
package doctor

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshruntime "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime/llamacpp"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"golang.org/x/sys/unix"
)

// Role selects relevant local resources. Client covers direct run/status commands.
type Role string

const (
	Agent      Role = "agent"
	Controller Role = "controller"
	Client     Role = "client"
)

// Mode selects which resource checks are safe for the caller's role lifetime.
type Mode uint8

const (
	Preflight Mode = iota // Check whether a new role can acquire its ports.
	Probe                 // Also start, warm and stop one temporary Agent runtime.
	Active                // Caller owns the role; omit binds and runtime launches.
)

// Options preserves the caller's lifetime and configuration validation policy.
type Options struct {
	Mode  Mode
	Local bool // Validate loopback-only settings, including ephemeral listeners.
}

// CheckState distinguishes failure from an omitted or unavailable observation.
type CheckState string

const (
	Passed     CheckState = "passed"
	Failed     CheckState = "failed"
	NotChecked CheckState = "not checked"
)

// Check holds one result and a concrete recovery action for failures.
type Check struct {
	Name           string
	State          CheckState
	Detail, Action string
}

// Report contains local observations; port results hold only at check time.
type Report struct{ Checks []Check }

// Run never repairs files or makes outbound connections. Probe explicitly owns
// one runtime start/warm-up/stop. Failed checks belong in the report; errors mean
// the requested checks could not run (invalid options or canceled invocation).
// Active does not assess health: the caller must show its current role status.
func Run(ctx context.Context, cfg config.Config, layout setup.Layout, role Role, options Options) (Report, error) {
	return run(ctx, cfg, layout, role, options, func() meshruntime.Runtime { return llamacpp.New() })
}
func run(ctx context.Context, cfg config.Config, layout setup.Layout, role Role, options Options, newRuntime func() meshruntime.Runtime) (Report, error) {
	var report Report
	mode := options.Mode
	if role != Agent && role != Controller && role != Client {
		return report, errors.New("select doctor --role agent, controller, or client")
	}
	if mode != Preflight && mode != Probe && mode != Active {
		return report, errors.New("invalid diagnosis mode")
	}
	if mode == Probe && role != Agent {
		return report, errors.New("use --probe only with --role agent")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	record := func(name, detail, action string, err error) bool {
		check := Check{Name: name, State: Passed, Detail: detail}
		if err != nil {
			check.State = Failed
			check.Detail = err.Error()
			check.Action = action
		}
		report.Checks = append(report.Checks, check)
		return err == nil
	}
	port := func(name, address, action string) bool {
		if mode == Active {
			report.Checks = append(report.Checks, Check{Name: name, State: NotChecked, Detail: "This session owns the role. See current status; no new port bind was attempted."})
			return true
		}
		return record(name, "Available at check time.", action, checkPort(ctx, address))
	}
	var archErr error
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		archErr = errors.New("unsupported native architecture")
	}
	ready := record("architecture", runtime.GOARCH, "Install the native package on an arm64 or amd64 Mac.", archErr)
	roles := config.Role(0)
	if role == Controller {
		roles = config.RoleController
	}
	validate := config.Validate
	if options.Local {
		validate = config.ValidateLocal
	}
	if !record("configuration", "Configuration is valid.", "Correct the configuration or command options, then run doctor again.", validate(cfg, roles)) {
		return report, nil
	}
	if role == Controller {
		port("controller port", cfg.ControllerListen, "Stop the process you own on this port, or set --controller-listen HOST:PORT.")
	}
	if role != Agent {
		report.Checks = append(report.Checks, Check{Name: "Agent files and runtime", State: NotChecked, Detail: "This role needs no local runtime or model."}, Check{Name: "controller connection", State: NotChecked, Detail: "Doctor makes no outbound connection. Use mica-mesh status to check the controller."})
		return report, ctx.Err()
	}
	managed := cfg.RuntimeBinary == "" || cfg.ModelPath == ""
	if managed {
		resolved, err := setup.ResolveManaged(ctx, cfg, config.AssetFields{RuntimeBinary: cfg.RuntimeBinary != "", ModelPath: cfg.ModelPath != "", Backend: true}, layout)
		ready = record("managed files", "Installed file identities are valid.", "Run mica-mesh setup. If the runtime bundle is missing, install the native package.", err) && ready
		if err == nil {
			cfg = resolved
		}
		ready = record("data folder", "Existing parent permits writes; no file was created.", "Give your user write access to the managed data folder, then run doctor again.", writableParent(layout.DataRoot)) && ready
	}
	if cfg.RuntimeBinary != "" {
		ready = record("runtime file", cfg.RuntimeBinary, "Set --runtime-binary to an executable llama-server file, or run mica-mesh setup.", checkFile(cfg.RuntimeBinary, true)) && ready
	}
	if cfg.ModelPath != "" {
		ready = record("model file", cfg.ModelPath, "Set --model-path to the prepared GGUF file, or run mica-mesh setup.", checkFile(cfg.ModelPath, false)) && ready
	}
	ready = port("Agent port", cfg.WorkerListen, "Stop the process you own on this port, or set --worker-listen HOST:PORT.") && ready
	ready = port("runtime port", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.RuntimePort)), "Keep the current port owner running; set --runtime-port to a free port.") && ready
	if mode == Active {
		report.Checks = append(report.Checks, Check{Name: "runtime probe", State: NotChecked, Detail: "The active Agent supplies runtime health. No extra runtime was started."})
	} else if mode == Probe && ready {
		rt := newRuntime()
		startup, cancel := context.WithTimeout(ctx, 120*time.Second)
		startErr := rt.Start(startup, meshruntime.Config{BinaryPath: cfg.RuntimeBinary, ModelPath: cfg.ModelPath, Backend: cfg.Backend, Port: cfg.RuntimePort, Model: cfg.ModelDescriptor})
		cancel()
		// Cleanup outlives caller cancellation, within the runtime's existing bounded
		// termination/reap policy. Stop is required even after a partial Start.
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		stopErr := rt.Stop(cleanup)
		cancelCleanup()
		record("runtime probe", "Runtime became ready and stopped.", "Check the runtime and model paths, backend, and --runtime-port; then run doctor --probe again.", errors.Join(startErr, stopErr))
	} else {
		report.Checks = append(report.Checks, Check{Name: "runtime probe", State: NotChecked, Detail: "Use doctor --probe after the required local checks pass."})
	}
	return report, ctx.Err()
}

func checkFile(path string, executable bool) error {
	if !filepath.IsAbs(path) {
		return errors.New("file path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	mode := uint32(unix.R_OK)
	if executable {
		mode |= unix.X_OK
	}
	return unix.Access(path, mode)
}
func writableParent(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("managed data folder must be absolute")
	}
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.IsDir() {
				return errors.New("managed data parent must be a directory without a link")
			}
			return unix.Access(path, unix.W_OK|unix.X_OK)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return err
		}
		path = parent
	}
}
func checkPort(ctx context.Context, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	// Reject names here so a diagnosis never causes a DNS lookup.
	if host != "" && net.ParseIP(host) == nil {
		return errors.New("use a numeric local IPv4 address for the port check")
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp4", address)
	if err != nil {
		return err
	}
	return listener.Close()
}
