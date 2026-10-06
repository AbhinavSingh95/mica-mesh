package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
)

// ErrNotPrepared reports unavailable managed assets and preserves their cause.
type ErrNotPrepared struct{ Cause error }

func (e *ErrNotPrepared) Error() string {
	return fmt.Sprintf("managed files are not prepared: %v; run mica-mesh setup", e.Cause)
}
func (e *ErrNotPrepared) Unwrap() error { return e.Cause }

// ResolveManaged fills asset settings omitted from the file and visited flags.
// It reads local metadata without downloads or writes.
func ResolveManaged(ctx context.Context, cfg config.Config, fields config.AssetFields, layout Layout) (config.Config, error) {
	if fields.RuntimeBinary && !filepath.IsAbs(cfg.RuntimeBinary) {
		return cfg, errors.New("worker runtime_binary must be an absolute path")
	}
	if fields.ModelPath && !filepath.IsAbs(cfg.ModelPath) {
		return cfg, errors.New("worker model_path must be an absolute path")
	}
	if fields.Backend && cfg.Backend != "cpu" && cfg.Backend != "metal" {
		return cfg, errors.New("backend must be metal or cpu")
	}
	if fields.RuntimeBinary && fields.ModelPath {
		return cfg, nil
	}
	if err := ctx.Err(); err != nil {
		return cfg, &ErrNotPrepared{Cause: err}
	}
	defaults := config.Default()
	if cfg.ModelDescriptor != defaults.ModelDescriptor || cfg.Model != defaults.Model {
		return cfg, &ErrNotPrepared{Cause: errors.New("managed files require the pinned model descriptor; use manual paths for another descriptor")}
	}
	release, err := LoadRelease(ctx, layout.ReleaseRoot)
	if err != nil {
		return cfg, &ErrNotPrepared{Cause: err}
	}
	if !fields.RuntimeBinary && fields.Backend && cfg.Backend != release.Backend {
		return cfg, &ErrNotPrepared{Cause: errors.New("backend differs from the packaged runtime; set runtime_binary to a matching manual runtime")}
	}
	installed, err := LoadInstallation(ctx, layout, release)
	if err != nil {
		return cfg, &ErrNotPrepared{Cause: err}
	}
	if !fields.RuntimeBinary {
		cfg.RuntimeBinary = installed.RuntimeBinary
	}
	if !fields.ModelPath {
		cfg.ModelPath = installed.ModelPath
	}
	if !fields.Backend {
		cfg.Backend = installed.Backend
	}
	return cfg, nil
}
