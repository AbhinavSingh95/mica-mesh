// Package cli implements the command-line interface.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
)

// Main owns signal handling and returns a process exit status. It never closes
// supplied writers. Writers must cooperate with cancellation or return promptly;
// supplied files must support deadlines unless regular or the actual null device,
// with exclusive ownership
// of writes/deadlines for the call. File deadlines are cleared before return.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelInvocation := context.WithCancel(ctx)
	defer cancelInvocation()
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		release, err := bindOutput(ctx, stdout, stderr)
		if err != nil {
			return 1
		}
		_, err = fmt.Fprint(stdout, usage)
		if err != nil {
			cancelInvocation()
		}
		release()
		if err != nil {
			finalDiagnostic(ctx, stderr, "mica-mesh: write help: %v\n", err)
			return 1
		}
		return 0
	}
	c, err := parse(args)
	if err == nil && c.roles&config.RoleWorker != 0 {
		c.cfg, err = resolveWorkerConfig(ctx, c)
	}
	if err == nil && c.name == "run" {
		if err := runInference(ctx, c.cfg, c.prompt, stdout, stderr, resolveController(c.cfg.ControllerAddress)); err != nil {
			return 1
		}
		return 0
	}
	if c.name == "status" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	release, bindErr := bindOutput(ctx, stdout, stderr)
	if bindErr != nil {
		return 1
	}
	if err == nil {
		switch c.name {
		case "start":
			err = app.Run(ctx, c.cfg, c.roles)
		case "status":
			err = clusterStatus(ctx, c.cfg.ControllerAddress, stdout)
		}
	}
	if err != nil {
		cancelInvocation()
	}
	release()
	if err != nil {
		finalDiagnostic(ctx, stderr, "mica-mesh: %v\n", err)
		return 1
	}
	return 0
}

const usage = `mica-mesh — private LAN inference mesh

Usage:
  mica-mesh start --controller [--worker] [options]
  mica-mesh start --worker [--controller-address HOST:PORT] [options]
  mica-mesh status [--controller-address HOST:PORT] [options]
  mica-mesh run [--controller-address HOST:PORT] [options] "prompt"

Options:
  --config PATH                 JSON config (default ~/.config/mica-mesh/config.json)
  --controller-listen HOST:PORT  Controller listener (default 0.0.0.0:50051)
  --worker-listen HOST:PORT      Worker listener (default 0.0.0.0:50052)
  --advertise-address IPv4       Local LAN IPv4 address override
  --runtime-binary PATH          Absolute prepared llama-server path
  --model-path PATH              Absolute prepared GGUF path
  --runtime-port PORT            Loopback runtime port (default 8080)
  --backend cpu|metal            Native runtime backend
  --model ID                    Requested model ID
  --max-output-tokens N          1–512 (default 128)
  --timeout DURATION             Total run budget (default/maximum 300s)
  -h, --help                     Show this help

Without --controller-address, discover one compatible LAN controller (3 seconds).
If multicast is unavailable or several controllers are found, use an explicit address.
`
