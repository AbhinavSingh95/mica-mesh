package main

import (
	"context"
	"fmt"
	"os"

	"github.com/AbhinavSingh95/mica-mesh/internal/cli"
	"github.com/AbhinavSingh95/mica-mesh/internal/terminal"
)

func main() { os.Exit(entrypoint()) }
func entrypoint() int {
	mode, err := cli.SelectMode(os.Args[1:], terminal.Available(0, 1, 2), os.Getenv("TERM") == "dumb")
	if err != nil {
		reportOutputError(err)
		return 1
	}
	if mode == cli.GuidedMode {
		console, err := terminal.Open(0, 1, 2)
		if err != nil {
			reportOutputError(err)
			return 1
		}
		restoreLogger := installLogger(console.Diagnostics())
		code := cli.Guided(context.Background(), os.Args[1:], console)
		restoreLogger()
		if err := console.Close(); err != nil {
			reportOutputError(fmt.Errorf("restore terminal: %w; reset this terminal before retrying", err))
			code = 1
		}
		return code
	}

	plain, err := openPlain(context.Background())
	if err != nil {
		reportOutputError(fmt.Errorf("initialize cancellable output: %w", err))
		_ = plain.close() // Output setup has already restored its partial ownership.
		return 1
	}
	restoreLogger, loggingFailed := ownLogger(plain.outputs[1].file, plain.cancel)
	code := cli.Main(plain.ctx, os.Args[1:], plain.input, plain.outputs[0].file, plain.outputs[1].file)
	restoreLogger()
	if loggingFailed() {
		code = 1
	}
	// Main has joined consent reads, callbacks, service work, and diagnostics.
	if err := plain.close(); err != nil {
		code = 1
	}
	return code
}
