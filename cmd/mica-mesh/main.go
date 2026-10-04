package main

import (
	"context"
	"os"

	"github.com/AbhinavSingh95/mica-mesh/internal/cli"
)

func main() { os.Exit(entrypoint()) }
func entrypoint() int {
	input := captureInput(0)
	outputs, err := ownOutputs(1, 2)
	if err != nil {
		reportOutputError(err)
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
