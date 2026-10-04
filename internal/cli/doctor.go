package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/AbhinavSingh95/mica-mesh/internal/doctor"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"io"
	"path/filepath"
)

func runDoctor(ctx context.Context, c command, stderr io.Writer) error {
	// Preserve explicit invalid values before the doctor resolves omitted paths.
	if c.doctorRole == doctor.Agent && (c.assets.RuntimeBinary && !filepath.IsAbs(c.cfg.RuntimeBinary) || c.assets.ModelPath && !filepath.IsAbs(c.cfg.ModelPath)) {
		return errors.New("explicit runtime and model paths must be absolute; correct --runtime-binary or --model-path, then run doctor again")
	}
	var layout setup.Layout
	var err error
	if c.doctorRole == doctor.Agent && (!c.assets.RuntimeBinary || !c.assets.ModelPath) {
		layout, err = installedLayout()
		if err != nil {
			return err
		}
	}
	report, runErr := doctor.Run(ctx, c.cfg, layout, c.doctorRole, c.probe)
	failed := false
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(stderr, "%s: %s\n", check.State, check.Name); err != nil {
			return err
		}
		if c.verbose && check.Detail != "" {
			if _, err := fmt.Fprintf(stderr, "  %s\n", check.Detail); err != nil {
				return err
			}
		}
		if check.State == doctor.Failed {
			failed = true
			if _, err := fmt.Fprintf(stderr, "  Next: %s\n", check.Action); err != nil {
				return err
			}
		}
	}
	if runErr != nil {
		return runErr
	}
	if failed {
		return errors.New("doctor found a failed check; follow the listed recovery action")
	}
	_, err = fmt.Fprintln(stderr, "Local checks complete. Port availability can change. No controller connection was tested.")
	return err
}
