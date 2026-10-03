package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/sys/unix"
)

func TestNullRedirection(t *testing.T) {
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"stdout", "stderr", "merged"} {
		t.Run(mode, func(t *testing.T) {
			null, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			cmd := exec.Command(binary, "--help")
			cmd.Stdout = regularSink(t)
			cmd.Stderr = regularSink(t)
			if mode == "stdout" || mode == "merged" {
				cmd.Stdout = null
			}
			if mode == "stderr" || mode == "merged" {
				cmd.Stderr = null
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitChild(t, cmd, false)
		})
	}
}
func TestFinalDiagnosticOnInheritedPipe(t *testing.T) {
	for _, mode := range []string{"signal", "deadline", "final-non-OK", "status-timeout"} {
		t.Run(mode, func(t *testing.T) {
			address, s := binaryServerConfigured(t, mode == "final-non-OK", mode == "status-timeout")
			budget := "4s"
			if mode == "deadline" {
				budget = "150ms"
			}
			cmd := child(t, address, budget)
			if mode == "status-timeout" {
				cmd.Args[1] = "status"
				cmd.Args = cmd.Args[:len(cmd.Args)-1]
			}
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			fd := int(w.Fd())
			initial := flags(t, fd) &^ unix.O_NONBLOCK
			cmd.Stdout = regularSink(t)
			cmd.Stderr = w
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			if mode == "signal" {
				waitSignal(t, s.dispatched)
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			waitChild(t, cmd, true)
			if got := flags(t, fd) & unix.O_NONBLOCK; got != initial&unix.O_NONBLOCK {
				t.Fatalf("nonblocking flags=%d", got)
			}
			_ = w.Close()
			_ = r.SetReadDeadline(time.Now().Add(time.Second))
			data, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("error=")) && mode != "status-timeout" {
				t.Fatalf("no final error diagnostic: %q", data)
			}
			if mode == "status-timeout" && !bytes.Contains(data, []byte("DeadlineExceeded")) {
				t.Fatalf("missing status deadline error: %q", data)
			}
			if mode != "status-timeout" && !bytes.Contains(data, []byte("request_id=")) {
				t.Fatalf("missing request identity: %q", data)
			}
		})
	}
}
func TestOwnedLoggerRestoresDefaults(t *testing.T) {
	file := regularSink(t)
	var previous bytes.Buffer
	originalSlog, originalWriter, originalFlags := slog.Default(), log.Writer(), log.Flags()
	defer func() { slog.SetDefault(originalSlog); log.SetOutput(originalWriter); log.SetFlags(originalFlags) }()
	log.SetOutput(&previous)
	log.SetFlags(log.Ldate | log.Lshortfile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restore, failed := ownLogger(file, cancel)
	slog.Info("service diagnostic")
	log.Print("standard diagnostic")
	restore()
	if failed() || ctx.Err() != nil {
		t.Fatal("healthy logger canceled process")
	}
	if slog.Default() != originalSlog || log.Writer() != &previous || log.Flags() != log.Ldate|log.Lshortfile {
		t.Fatal("logger settings not restored")
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || !bytes.Contains(data, []byte("service diagnostic")) || !bytes.Contains(data, []byte("standard diagnostic")) {
		t.Fatalf("owned log output=%q error=%v", data, err)
	}
}
func TestStartLoggingFailureReapsRuntime(t *testing.T) {
	for _, mode := range []string{"broken-stderr", "full-stderr"} {
		t.Run(mode, func(t *testing.T) {
			pid := make(chan int, 1)
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/launch" {
					var launch struct{ PID int }
					if err := json.NewDecoder(r.Body).Decode(&launch); err != nil {
						t.Error(err)
						return
					}
					pid <- launch.PID
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer control.Close()
			runtimeBinary := filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("MICA_TEST_SERVER"))
			model := filepath.Join(t.TempDir(), "model.gguf")
			data := []byte("test gguf")
			if err := os.WriteFile(model, data, 0600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := l.Addr().(*net.TCPAddr).Port
			_ = l.Close()
			workerListener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			workerAddress := workerListener.Addr().String()
			_ = workerListener.Close()
			cfg := filepath.Join(t.TempDir(), "config.json")
			body, err := json.Marshal(map[string]any{"runtime_binary": runtimeBinary, "model_path": model, "backend": "cpu", "runtime_port": port, "worker_listen": workerAddress, "controller_address": "127.0.0.1:1", "model_descriptor": map[string]any{"id": "test-model", "sha256": hex.EncodeToString(digest[:]), "context_tokens": 2048}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg, body, 0600); err != nil {
				t.Fatal(err)
			}
			binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "start", "--worker", "--config", cfg)
			cmd.Env = append(os.Environ(), "MICA_TEST_CONTROL="+control.URL, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
			var r, w *os.File
			var fd, initial int
			if mode == "full-stderr" {
				r, w, fd, initial = fullBlockingPipe(t)
			} else {
				r, w, err = os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				fd = int(w.Fd())
				initial = flags(t, fd)
				defer r.Close()
				defer w.Close()
			}
			cmd.Stdout = regularSink(t)
			cmd.Stderr = w
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var childPID int
			select {
			case childPID = <-pid:
			case <-time.After(5 * time.Second):
				t.Fatal("worker runtime not launched")
			}
			defer func() { _ = unix.Kill(childPID, unix.SIGKILL) }()
			if mode == "full-stderr" {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = r.Close()
			}
			waitChild(t, cmd, true)
			if cmd.ProcessState.ExitCode() < 0 {
				t.Error("mesh exited by signal, bypassing cooperative shutdown")
			}
			if got := flags(t, fd) & unix.O_NONBLOCK; got != initial&unix.O_NONBLOCK {
				t.Errorf("nonblocking flag not restored: %d", got)
			}
			process, err := os.FindProcess(childPID)
			if err != nil {
				t.Fatal(err)
			}
			defer process.Release()
			if err := process.Signal(syscall.Signal(0)); err == nil {
				t.Errorf("runtime child %d still alive or unreaped", childPID)
			}
		})
	}
}
