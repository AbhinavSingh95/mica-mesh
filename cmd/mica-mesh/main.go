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

	input := captureInput(0)
	outputs, err := ownOutputs(1, 2)
	if err != nil {
		reportOutputError(fmt.Errorf("initialize cancellable output: %w", err))
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restoreLogger, loggingFailed := ownLogger(outputs[1].file, cancel)
	code := cli.Main(ctx, os.Args[1:], input, outputs[0].file, outputs[1].file)
	restoreLogger()
	if loggingFailed() {
		code = 1
	}
	// Main has joined consent reads, callbacks, service work, and diagnostics.
	if err := input.close(); err != nil {
		code = 1
	}
	if err := closeOutputs(outputs); err != nil {
		code = 1
	}
	return code
}
