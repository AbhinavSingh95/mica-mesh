// Package cli implements the command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
)

// Main owns signal handling and returns a process exit status. Consent input must
// report terminal identity through IsTerminal and support read deadlines, or be
// a prompt/cooperative test reader. Unread commands never inspect stdin. It never closes
// supplied writers. Writers must cooperate with cancellation or return promptly;
// supplied files must support deadlines unless regular or the actual null device,
// with exclusive ownership
// of writes/deadlines for the call. File deadlines are cleared before return.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	args, _, verbose, help, commonErr := commonFlags(args)
	if commonErr != nil {
		finalDiagnostic(ctx, stderr, "mica-mesh: %v\n", commonErr)
		return 1
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelInvocation := context.WithCancel(ctx)
	defer cancelInvocation()
	if len(args) == 0 || len(args) == 1 && help {
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
	c.verbose = c.verbose || verbose
	if errors.Is(err, flag.ErrHelp) {
		release, bindErr := bindOutput(ctx, stdout, stderr)
		if bindErr != nil {
			return 1
		}
		_, writeErr := fmt.Fprint(stdout, commandHelp(c.name))
		release()
		if writeErr != nil {
			finalDiagnostic(ctx, stderr, "mica-mesh: write help: %v\n", writeErr)
			return 1
		}
		return 0
	}
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
		case "setup":
			err = runSetup(ctx, c.yes, stdin, stderr)
		case "doctor":
			err = runDoctor(ctx, c, stderr)
		case "start":
			err = app.Run(ctx, c.cfg, c.roles)
		case "controller", "agent":
			network := app.LAN
			if c.local {
				network = app.Local
			} else {
				listener := c.cfg.ControllerListen
				if c.roles == config.RoleWorker {
					listener = c.cfg.WorkerListen
				}
				_, err = fmt.Fprintf(stderr, "%s listens on %s. Devices on your LAN can connect.\n", c.name, listener)
			}
			if err == nil {
				var process *app.Process
				process, err = app.Start(ctx, c.cfg, c.roles, app.Options{Network: network})
				if err == nil {
					err = process.Wait()
				}
			}
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
  mica-mesh controller [--local] [--plain] [options]
  mica-mesh agent [--local] [--plain] [options]
  mica-mesh setup [--yes]
  mica-mesh doctor [--role agent|controller|client] [--probe] [--verbose] [options]
  mica-mesh start --controller [--worker] [options]
  mica-mesh start --worker [--controller-address HOST:PORT] [options]
  mica-mesh status [--controller-address HOST:PORT] [options]
  mica-mesh run [--controller-address HOST:PORT] [options] "prompt"

` + commandOptions + discoveryHelp

const commandOptions = `Options:
  --config PATH                 JSON config (default ~/.config/mica-mesh/config.json)
  --controller-listen HOST:PORT  Controller listener (default 0.0.0.0:50051)
  --worker-listen HOST:PORT      Agent listener (default 0.0.0.0:50052)
  --advertise-address IPv4       Local LAN IPv4 address override
  --runtime-binary PATH          Absolute prepared llama-server path
  --model-path PATH              Absolute prepared GGUF path
  --runtime-port PORT            Loopback runtime port (default 8080)
  --backend cpu|metal            Native runtime backend
  --model ID                    Requested model ID
  --max-output-tokens N          1–512 (default 128)
  --timeout DURATION             Total run budget (default/maximum 300s)
  -h, --help                     Show this help

`

const discoveryHelp = `Without --controller-address, discover one compatible LAN controller (3 seconds).
If multicast is unavailable or several controllers are found, use an explicit address.
`

func commandHelp(name string) string {
	switch name {
	case "controller", "agent":
		return "Usage: mica-mesh " + name + " [--local] [--plain] [options]\nRun one role per terminal. Controller accepts requests through run.\nAgent owns the prepared runtime. No prompts or downloads in plain mode.\n--local uses loopback only: Controller 127.0.0.1:50051, Agent 127.0.0.1:50052.\n--plain runs foreground services.\n\n" + commandOptions + discoveryHelp
	case "setup":
		return "Usage: mica-mesh setup [--yes]\nPrepare the pinned model in your managed data folder.\nUse --yes to consent in scripts. Setup needs an installed native runtime bundle.\n"
	case "doctor":
		return "Usage: mica-mesh doctor [--role agent|controller|client] [--probe] [--verbose] [options]\nCheck local files and ports. Default role: agent.\n--probe starts and stops the Agent runtime. --verbose shows diagnostic details.\n" + commandOptions
	case "start":
		return "Usage: mica-mesh start --controller [--worker] [options]\n       mica-mesh start --worker [--controller-address HOST:PORT] [options]\n\n" + commandOptions + discoveryHelp
	case "run":
		return "Usage: mica-mesh run [--controller-address HOST:PORT] [options] \"prompt\"\n\n" + commandOptions + discoveryHelp
	case "status":
		return "Usage: mica-mesh status [--controller-address HOST:PORT] [options]\n\n" + commandOptions + discoveryHelp
	default:
		return usage
	}
}
