// Package cli implements the command-line interface.
package cli

import (
	"context"
	"fmt"
	"io"
)

// Main runs the CLI and returns a process exit status.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		if _, err := fmt.Fprint(stdout, usage); err != nil {
			fmt.Fprintf(stderr, "mica-mesh: write help: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "mica-mesh: unknown command or arguments %q; use --help for usage\n", args)
	return 1
}

const usage = `mica-mesh — private LAN inference mesh

Usage: mica-mesh [--help]

Options:
  -h, --help  Show this help.

This build provides the CLI scaffold. Mesh commands are not implemented yet.
`
