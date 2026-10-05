package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSeparateGuidedTerminalsPreserveRoleLifetimes(t *testing.T) {
	for _, order := range []string{"agent-first", "controller-first"} {
		t.Run(order, func(t *testing.T) {
			f := manualTerminalRuntime(t)
			binary := stockCLI(t)
			env := terminalEnvironment("HOME="+t.TempDir(), "MICA_TEST_CONTROL="+f.server.URL)
			var agent, controller *terminalChild
			startAgent := func() {
				agent = startTerminal(t, binary, []string{"agent", "--local", "--config", f.config, "--worker-listen", "127.0.0.1:0", "--controller-address", f.address}, env)
				agent.await("Runtime  Ready")
			}
			startController := func() {
				controller = startTerminal(t, binary, []string{"controller", "--local", "--config", f.config, "--controller-listen", f.address}, env)
				controller.await("Each prompt is a new request.")
			}
			if order == "agent-first" {
				startAgent()
				agent.await("Connecting")
				startController()
			} else {
				startController()
				controller.await("0 ready")
				startAgent()
			}
			controller.await("1 ready")
			pid := f.pid.Load()
			if pid == 0 || f.launches.Load() != 1 {
				t.Fatal("runtime readiness lacked one owned child")
			}
			controller.send("\x1b[200~" + strings.Repeat("x", 16385) + "\x1b[201~")
			controller.await("at most 16 KiB")
			prompt := "first line 🌏\nsecond line é"
			controller.send("\x1b[200~" + prompt + "\x1b[201~\r")
			waitTerminalEvent(t, f.partial)
			select {
			case got := <-f.prompts:
				if got != prompt {
					t.Fatalf("prompt changed: %q", got)
				}
			case <-time.After(time.Second):
				t.Fatal("prompt not observed")
			}
			controller.await("partial 🌏")
			if text := controller.text(); strings.Contains(text, "\x1b]52;c;hostile") || strings.Contains(text, "hostilepayload") || strings.Contains(text, "\x1b[31m") {
				t.Fatal("model controls reached display")
			}
			controller.resize(30, 8)
			controller.await("Resize")
			controller.send("\x03")
			waitTerminalEvent(t, f.canceled)
			controller.resize(120, 40)
			// F1 follows Ctrl-C in the same input stream. Its rendered panel
			// proves the second interrupt was accepted while cleanup was held.
			controller.send("\x03\x1bOP")
			controller.await("Run one role in each terminal.")
			f.processing.Store(false)
			controller.send("\x1bOP")
			controller.await("cleanup confirmed")
			if f.pid.Load() != pid || f.launches.Load() != 1 {
				t.Fatal("request cancellation restarted runtime")
			}
			controller.clear()
			controller.send("second request\r")
			controller.await("reused answer 🌏")
			controller.await("Complete")
			if f.streams.Load() != 2 {
				t.Fatal("canceled request replayed or fresh request missing")
			}
			for _, output := range []string{"pipe", "file"} {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				cmd := exec.CommandContext(ctx, binary, "run", "--config", f.config, "--controller-address", f.address, "plain independent prompt")
				cmd.Env = env
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				var file *os.File
				if output == "file" {
					var err error
					file, err = os.CreateTemp(t.TempDir(), "plain-output")
					if err != nil {
						t.Fatal(err)
					}
					cmd.Stdout = file
				}
				err := cmd.Run()
				cancel()
				if file != nil {
					if _, err := file.Seek(0, 0); err != nil {
						t.Fatal(err)
					}
					if _, err := stdout.ReadFrom(file); err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err != nil || stdout.String() != "reused answer 🌏" || strings.Contains(stderr.String(), "\x1b") {
					t.Fatalf("plain %s output=%q diagnostic=%q error=%v", output, stdout.String(), stderr.String(), err)
				}
			}
			controller.finish(nil, "\x03")
			if err := unix.Kill(int(pid), 0); err != nil {
				t.Fatalf("Controller exit killed Agent runtime: %v", err)
			}
			agent.clear()
			agent.await("Connecting")
			startController()
			controller.await("1 ready")
			if f.pid.Load() != pid || f.launches.Load() != 1 {
				t.Fatal("Controller restart restarted runtime")
			}
			agent.finish(syscall.SIGTERM, "")
			if err := unix.Kill(int(pid), 0); err == nil {
				t.Fatal("Agent exit left runtime alive or unreaped")
			}
			f.pid.Store(0)
			controller.clear()
			controller.await("0 ready")
			controller.resize(25, 7)
			controller.await("Resize")
			controller.finish(nil, "\x03")
		})
	}
}

func TestGuidedSIGTERMDuringGenerationJoinsRequestOnly(t *testing.T) {
	f := manualTerminalRuntime(t)
	binary := stockCLI(t)
	env := terminalEnvironment("HOME="+t.TempDir(), "MICA_TEST_CONTROL="+f.server.URL)
	agent := startTerminal(t, binary, []string{"agent", "--local", "--config", f.config, "--worker-listen", "127.0.0.1:0", "--controller-address", f.address}, env)
	agent.await("Runtime  Ready")
	controller := startTerminal(t, binary, []string{"controller", "--local", "--config", f.config, "--controller-listen", f.address}, env)
	controller.await("1 ready")
	controller.send("interrupt this request\r")
	waitTerminalEvent(t, f.partial)
	controller.await("partial 🌏")
	pid := int(f.pid.Load())
	if err := controller.child.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitTerminalEvent(t, f.canceled)
	f.processing.Store(false)
	controller.finish(nil, "")
	if err := unix.Kill(pid, 0); err != nil || f.launches.Load() != 1 {
		t.Fatalf("request shutdown stopped runtime: %v", err)
	}
	agent.finish(syscall.SIGTERM, "")
	if err := unix.Kill(pid, 0); err == nil {
		t.Fatal("Agent did not reap runtime")
	}
	f.pid.Store(0)
}

func TestGuidedDiagnosisDoesNotFailOwnedPorts(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprintf("ephemeral=%v", ephemeral), func(t *testing.T) {
			f := manualTerminalRuntime(t)
			binary := stockCLI(t)
			env := terminalEnvironment("HOME="+t.TempDir(), "MICA_TEST_CONTROL="+f.server.URL)
			address := freeEndpoint(t)
			if ephemeral {
				address = "127.0.0.1:0"
			}
			agent := startTerminal(t, binary, []string{"agent", "--local", "--config", f.config, "--worker-listen", address, "--controller-address", f.address, "--controller-listen", "127.0.0.1:0"}, env)
			agent.await("Runtime  Ready")
			controller := startTerminal(t, binary, []string{"controller", "--local", "--config", f.config, "--controller-listen", f.address, "--worker-listen", "127.0.0.1:0"}, env)
			controller.await("1 ready")
			pid := int(f.pid.Load())
			for _, role := range []*terminalChild{agent, controller} {
				role.send("\x1bOQ") // F2
				role.await("DIAGNOSIS")
				role.await("configuration · passed")
				if role == agent {
					role.await("runtime probe · not checked")
				} else {
					role.await("controller connection · not checked")
				}
				text := role.view()
				for _, port := range []string{"Agent port", "controller port", "runtime port"} {
					if strings.Contains(text, port+" · failed") {
						t.Errorf("diagnosis rejected an owned port:\n%s", text)
					}
				}
				if strings.Contains(text, "Use doctor --probe") {
					t.Error("diagnosis recommends another runtime probe while Agent is active")
				}
			}
			if !strings.Contains(agent.view(), "Runtime  Ready") {
				t.Error("diagnosis hides the running Agent's runtime health")
			}
			if f.launches.Load() != 1 || int(f.pid.Load()) != pid {
				t.Fatal("diagnosis restarted the owned runtime")
			}
			controller.finish(nil, "\x03")
			agent.finish(nil, "\x03")
			f.pid.Store(0)
		})
	}
}

func TestGuidedRuntimePortFailureCanRecover(t *testing.T) {
	f := manualTerminalRuntime(t)
	data, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		RuntimePort int `json:"runtime_port"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", cfg.RuntimePort))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	env := terminalEnvironment("HOME="+t.TempDir(), "MICA_TEST_CONTROL="+f.server.URL)
	agent := startTerminal(t, stockCLI(t), []string{"agent", "--local", "--config", f.config, "--worker-listen", "127.0.0.1:0", "--controller-address", f.address}, env)
	agent.await("Runtime  Failed")
	agent.await("F6 Change runtime port")
	// The listener belongs to this test and must remain usable throughout.
	conn, err := net.DialTimeout("tcp4", occupied.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	agent.send("\x1b[17~")
	agent.await("Runtime port")
	_, port, err := net.SplitHostPort(freeEndpoint(t))
	if err != nil {
		t.Fatal(err)
	}
	agent.send(port + "\r")
	agent.await("Runtime  Ready")
	pid := int(f.pid.Load())
	agent.finish(syscall.SIGTERM, "")
	if err := unix.Kill(pid, 0); err == nil {
		t.Fatal("recovered Agent did not reap runtime")
	}
	f.pid.Store(0)
}
