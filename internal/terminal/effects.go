package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/client"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/doctor"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

// roleHandle holds only the owned effects the session uses. Production adapts
// app.Process; tests can hold startup and shutdown without launching a runtime.
type roleHandle struct {
	endpoint    string
	snapshot    func() (app.AgentStatus, bool)
	wait, close func() error
}
type controllerConnection struct {
	generate func(context.Context, *meshv1.InferenceRequest, func(*meshv1.InferenceEvent) error) error
	status   func(context.Context) (*meshv1.GetClusterStatusResponse, error)
	close    func() error
}
type sessionEffects struct {
	check      func(context.Context, Options) (Options, bool, error)
	prepare    func(context.Context, Options, func(setup.Progress) error) (config.Config, error)
	start      func(context.Context, config.Config, config.Role, app.Options) (*roleHandle, error)
	connect    func(string) (*controllerConnection, error)
	interfaces func() ([]discovery.InterfaceAddress, error)
	candidates func(context.Context) ([]discovery.Candidate, error)
	diagnose   func(context.Context, config.Config, setup.Layout, doctor.Role, bool) (doctor.Report, error)
	now        func() time.Time
}

func productionEffects() sessionEffects {
	return sessionEffects{
		check: checkAgentFiles, prepare: prepareAgentFiles,
		start: func(ctx context.Context, cfg config.Config, role config.Role, o app.Options) (*roleHandle, error) {
			p, err := app.Start(ctx, cfg, role, o)
			if err != nil {
				return nil, err
			}
			return &roleHandle{endpoint: p.ControllerAddress(), snapshot: p.AgentStatus, wait: p.Wait, close: p.Close}, nil
		},
		connect: func(address string) (*controllerConnection, error) {
			c, err := client.New(address)
			if err != nil {
				return nil, err
			}
			return &controllerConnection{generate: c.Generate, status: c.Status, close: c.Close}, nil
		},
		interfaces: discovery.InterfaceAddresses, candidates: discovery.Candidates, diagnose: doctor.Run, now: time.Now,
	}
}
func validateRole(o Options) error {
	if o.Network == app.Local {
		return config.ValidateLocal(o.Config, o.Role)
	}
	return config.Validate(o.Config, o.Role)
}

// Resolve roots at the Agent boundary, so a missing bundle cannot block Controller.
func checkAgentFiles(ctx context.Context, o Options) (Options, bool, error) {
	if o.Assets.RuntimeBinary && o.Assets.ModelPath {
		return o, false, validateRole(o)
	}
	if o.Layout.ReleaseRoot == "" || o.Layout.DataRoot == "" {
		executable, err := os.Executable()
		if err != nil {
			return o, false, err
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			return o, false, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return o, false, err
		}
		o.Layout = setup.Layout{ReleaseRoot: filepath.Dir(filepath.Dir(executable)), DataRoot: filepath.Join(home, "Library", "Application Support", "Mica Mesh")}
	}
	cfg, err := setup.ResolveManaged(ctx, o.Config, o.Assets, o.Layout)
	if err == nil {
		o.Config = cfg
		return o, false, validateRole(o)
	}
	// Only absent installation files can lead to preparation. Explicit invalid
	// configuration and damaged artifacts retain their actionable failure.
	if !errors.Is(err, os.ErrNotExist) {
		return o, false, err
	}
	release, loadErr := setup.LoadRelease(ctx, o.Layout.ReleaseRoot)
	if loadErr != nil {
		return o, false, fmt.Errorf("check Agent package: %w; install the native package, then press F5 to retry", loadErr)
	}
	o.Release = &release
	return o, true, nil
}
func prepareAgentFiles(ctx context.Context, o Options, progress func(setup.Progress) error) (config.Config, error) {
	if o.Release == nil {
		return o.Config, errors.New("Agent package is missing; install the native package and retry")
	}
	manager := setup.New(o.Layout, *o.Release, nil)
	if _, err := manager.Prepare(ctx, progress); err != nil {
		return o.Config, err
	}
	cfg, err := setup.ResolveManaged(ctx, o.Config, o.Assets, o.Layout)
	if err != nil {
		return cfg, err
	}
	o.Config = cfg
	return cfg, validateRole(o)
}
