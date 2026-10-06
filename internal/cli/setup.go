package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
)

// terminalInput lets the executable report real terminal identity without
// File.Fd changing its pollable descriptor back to blocking mode.
type terminalInput interface{ IsTerminal() (bool, error) }
type deadlineInput interface{ SetReadDeadline(time.Time) error }

func requireTerminal(stdin io.Reader) error {
	terminal, ok := stdin.(terminalInput)
	if !ok {
		return errors.New("setup needs a terminal; use mica-mesh setup --yes for scripts")
	}
	yes, err := terminal.IsTerminal()
	if err != nil {
		return fmt.Errorf("open consent input: %w; use mica-mesh setup --yes for scripts", err)
	}
	if !yes {
		return errors.New("setup needs a terminal; use mica-mesh setup --yes for scripts")
	}
	return nil
}

func installedLayout() (setup.Layout, error) {
	executable, err := os.Executable()
	if err != nil {
		return setup.Layout{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return setup.Layout{}, err
	}
	return managedLayout(executable, home)
}

func runSetup(ctx context.Context, yes bool, stdin io.Reader, stderr io.Writer) error {
	if !yes {
		if err := requireTerminal(stdin); err != nil {
			return err
		}
	}
	layout, err := installedLayout()
	if err != nil {
		return err
	}
	release, err := setup.LoadRelease(ctx, layout.ReleaseRoot)
	if err != nil {
		return fmt.Errorf("check installed runtime bundle: %w; install the native package, or follow the maintainer release instructions to build a package", err)
	}
	manager := setup.New(layout, release, nil)
	return prepareSetup(ctx, yes, stdin, stderr, layout, manager.Prepare)
}

// prepareSetup owns presentation and consent. The supplied preparation operation
// is the external download/write boundary; it must honor context and callbacks.
func prepareSetup(ctx context.Context, yes bool, stdin io.Reader, stderr io.Writer, layout setup.Layout, prepare func(context.Context, func(setup.Progress) error) (setup.Installation, error)) error {
	if !yes {
		if err := requireTerminal(stdin); err != nil {
			return err
		}
	}
	model := setup.ModelDownload(layout)
	if _, err := fmt.Fprintf(stderr, "Model: %s\nDownload: %d bytes over HTTPS from %s\nStore: %s\nA verified existing model needs no download.\n", model.ID, model.SizeBytes, model.URL, model.Path); err != nil {
		return err
	}
	if !yes {
		if _, err := fmt.Fprint(stderr, "Prepare these files? [y/N] "); err != nil {
			return err
		}
		if err := readConsent(ctx, stdin); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := prepare(ctx, func(progress setup.Progress) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stderr, "%s: %d/%d bytes\n", progress.Phase, progress.CompletedBytes, progress.TotalBytes)
		return err
	})
	if err != nil {
		return fmt.Errorf("prepare files: %w; resolve this issue, then run mica-mesh setup again", err)
	}
	_, err = fmt.Fprintln(stderr, "Files prepared. The Agent verifies runtime readiness when it starts.")
	return err
}

// readConsent reads synchronously with a fixed limit. The executable supplies
// deadline input; other readers must return promptly or honor cancellation.
func readConsent(ctx context.Context, stdin io.Reader) (err error) {
	if input, ok := stdin.(deadlineInput); ok {
		if err := input.SetReadDeadline(time.Time{}); err != nil {
			return fmt.Errorf("consent input cannot support cancellation: %w", err)
		}
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(done); _ = input.SetReadDeadline(time.Now()) })
		defer func() {
			if !stop() {
				<-done
			}
			err = errors.Join(err, input.SetReadDeadline(time.Time{}))
		}()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	line, readErr := bufio.NewReaderSize(io.LimitReader(stdin, 129), 128).ReadString('\n')
	if err := ctx.Err(); err != nil {
		return err
	}
	if readErr != nil || len(line) > 128 {
		return errors.New("setup consent ended without a complete answer; run mica-mesh setup again")
	}
	answer := strings.TrimSpace(line)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return errors.New("setup declined; run mica-mesh setup when ready")
	}
	return nil
}
