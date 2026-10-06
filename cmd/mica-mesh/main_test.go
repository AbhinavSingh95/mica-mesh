package main

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"context"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type binaryController struct {
	meshv1.UnimplementedControllerServiceServer
	dispatched, canceled     chan struct{}
	finalFailure, statusWait bool
}

func (s *binaryController) RunInference(req *meshv1.InferenceRequest, out grpc.ServerStreamingServer[meshv1.InferenceEvent]) error {
	close(s.dispatched)
	_ = out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_Started{Started: &meshv1.Started{RequestId: req.RequestId, WorkerId: uuid.NewString(), WorkerHostname: "binary-test", ModelId: req.ModelId}}})
	if s.finalFailure {
		return status.Error(codes.Unavailable, "final failure")
	}
	for {
		if err := out.Send(&meshv1.InferenceEvent{Payload: &meshv1.InferenceEvent_TextDelta{TextDelta: &meshv1.TextDelta{Text: strings.Repeat("x", 4096)}}}); err != nil {
			break
		}
	}
	<-out.Context().Done()
	close(s.canceled)
	return status.FromContextError(out.Context().Err()).Err()
}
func binaryServer(t *testing.T) (string, *binaryController) {
	return binaryServerConfigured(t, false, false)
}
func binaryServerConfigured(t *testing.T, finalFailure, statusWait bool) (string, *binaryController) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &binaryController{dispatched: make(chan struct{}), canceled: make(chan struct{}), finalFailure: finalFailure, statusWait: statusWait}
	g := grpc.NewServer(protocol.GenerationServerOption())
	meshv1.RegisterControllerServiceServer(g, s)
	done := make(chan struct{})
	go func() { defer close(done); _ = g.Serve(l) }()
	t.Cleanup(func() { g.Stop(); <-done })
	return l.Addr().String(), s
}
func child(t *testing.T, address, budget string) *exec.Cmd {
	t.Helper()
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	return exec.Command(binary, "run", "--config", cfg, "--controller-address", address, "--timeout", budget, "hello")
}
func flags(t *testing.T, fd int) int {
	t.Helper()
	v, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func fullBlockingPipe(t *testing.T) (*os.File, *os.File, int, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	fd := int(w.Fd())
	original := flags(t, fd) &^ unix.O_NONBLOCK
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, original|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := unix.Write(fd, []byte(strings.Repeat("p", 4096)))
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, original); err != nil {
		t.Fatal(err)
	}
	return r, w, fd, original
}
func waitChild(t *testing.T, cmd *exec.Cmd, wantFailure bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if (err != nil) != wantFailure {
			t.Fatalf("child exit=%v failure=%v", err, wantFailure)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("child did not exit after output cancellation")
	}
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("missing child/RPC handshake")
	}
}
func TestInheritedBlockingOutputCancellation(t *testing.T) {
	for _, mode := range []string{"signal-stdout", "deadline-stdout", "signal-stderr", "deadline-stderr", "signal-merged"} {
		t.Run(mode, func(t *testing.T) {
			address, s := binaryServer(t)
			_, w, fd, original := fullBlockingPipe(t)
			budget := "4s"
			if strings.HasPrefix(mode, "deadline") {
				budget = "300ms"
			}
			cmd := child(t, address, budget)
			if strings.Contains(mode, "stderr") {
				cmd.Stdout = regularSink(t)
				cmd.Stderr = w
			} else if strings.Contains(mode, "merged") {
				cmd.Stdout = w
				cmd.Stderr = w
			} else {
				cmd.Stdout = w
				cmd.Stderr = regularSink(t)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitSignal(t, s.dispatched)
			if strings.HasPrefix(mode, "signal") {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			waitChild(t, cmd, true)
			waitSignal(t, s.canceled)
			if got := flags(t, fd); got&unix.O_NONBLOCK != original&unix.O_NONBLOCK {
				t.Fatalf("shared flags=%#x want %#x", got, original)
			}
		})
	}
}
func TestBrokenReaderCancelsRPC(t *testing.T) {
	address, s := binaryServer(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	_ = r.Close()
	cmd := child(t, address, "2s")
	cmd.Stdout = w
	cmd.Stderr = regularSink(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitChild(t, cmd, true)
	waitSignal(t, s.canceled)
}
func TestRegularFileRedirection(t *testing.T) {
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command(binary, "--help")
	cmd.Stdout = file
	cmd.Stderr = file
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitChild(t, cmd, false)
	data, err := os.ReadFile(file.Name())
	if err != nil || !strings.Contains(string(data), "Usage:") {
		t.Fatalf("help=%q err=%v", data, err)
	}
}
func TestMergedOutputRestoresFlags(t *testing.T) {
	_, _, fd, original := fullBlockingPipe(t)
	out, err := ownOutputs(fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeOutputs(out); err != nil {
		t.Fatal(err)
	}
	if got := flags(t, fd); got&unix.O_NONBLOCK != original&unix.O_NONBLOCK {
		t.Fatalf("flags=%#x want=%#x", got, original)
	}
}
func TestSetupFailureRestoresFlags(t *testing.T) {
	_, _, fd, original := fullBlockingPipe(t)
	file, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if outputs, err := ownOutputs(fd, int(file.Fd())); err == nil {
		_ = closeOutputs(outputs)
		t.Fatal("unsupported nonregular output accepted")
	}
	if got := flags(t, fd); got&unix.O_NONBLOCK != original&unix.O_NONBLOCK {
		t.Fatalf("flags=%#x want=%#x", got, original)
	}
}
func regularSink(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "sink")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestPartialTextSurvivesBrokenReader(t *testing.T) {
	address, s := binaryServer(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := child(t, address, "2s")
	cmd.Stdout = w
	cmd.Stderr = regularSink(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != strings.Repeat("x", 4096) {
		t.Fatalf("partial=%q", buf)
	}
	_ = r.Close()
	waitChild(t, cmd, true)
	waitSignal(t, s.canceled)
	if cmd.ProcessState.ExitCode() < 0 {
		t.Fatal("child terminated by signal instead of cooperative EPIPE cleanup")
	}
}

func (s *binaryController) GetClusterStatus(ctx context.Context, _ *meshv1.GetClusterStatusRequest) (*meshv1.GetClusterStatusResponse, error) {
	if s.statusWait {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return &meshv1.GetClusterStatusResponse{}, nil
}
