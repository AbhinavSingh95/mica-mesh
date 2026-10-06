package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/terminal"
)

// Guided uses Console's signal/I/O owner. It never calls the plain Main path.
func Guided(ctx context.Context, args []string, console *terminal.Console) int {
	options, err := guidedOptions(args)
	if err == nil {
		err = terminal.Run(ctx, options, console)
	}
	if err != nil {
		// The invocation already failed. A failed final diagnostic has no second writer.
		_ = console.Report(err)
		return 1
	}
	return 0
}
func guidedOptions(args []string) (terminal.Options, error) {
	args, _, _, _, err := commonFlags(args)
	if err != nil {
		return terminal.Options{}, err
	}
	options := terminal.Options{Config: config.Default(), Network: app.LAN}
	if len(args) == 0 || args[0] == "ui" && len(args) == 1 {
		home, err := os.UserHomeDir()
		if err != nil {
			return options, fmt.Errorf("find configuration home: %w", err)
		}
		loaded, err := config.Load(filepath.Join(home, ".config", "mica-mesh", "config.json"), false)
		if err != nil {
			return options, err
		}
		options.Config, options.Assets = loaded.Config, loaded.Assets
		return options, nil
	}
	if args[0] == "ui" {
		return options, fmt.Errorf("ui has no role options; use agent or controller")
	}
	c, err := parse(args)
	if err != nil {
		return options, err
	}
	if c.name != "controller" && c.name != "agent" {
		return options, fmt.Errorf("guided mode needs agent or controller; use --help")
	}
	options.Config = c.cfg
	options.Assets = c.assets
	options.Role = c.roles
	if c.local {
		options.Network = app.Local
	}
	return options, nil
}
