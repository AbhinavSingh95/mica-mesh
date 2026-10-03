package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime/llamacpp"
)

func TestShutdownReapsOwnedRuntime(t *testing.T) {
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
	t.Setenv("MICA_TEST_CONTROL", control.URL)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cfg := config.Default()
	cfg.Backend = "cpu"
	cfg.RuntimeBinary = filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("MICA_TEST_SERVER"))
	cfg.ModelPath = filepath.Join(t.TempDir(), "model.gguf")
	data := []byte("test gguf")
	if err := os.WriteFile(cfg.ModelPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	cfg.ModelDescriptor.SHA256 = hex.EncodeToString(digest[:])
	cfg.ControllerAddress = "127.0.0.1:1"
	runtimePort, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimePort = runtimePort.Addr().(*net.TCPAddr).Port
	_ = runtimePort.Close()
	wl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, config.RoleWorker, llamacpp.New(), nil, wl) }()
	var ownedPID int
	select {
	case ownedPID = <-pid:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("runtime child not launched")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runtime shutdown not joined")
	}
	process, err := os.FindProcess(ownedPID)
	if err != nil {
		t.Fatal(err)
	}
	defer process.Release()
	if err := process.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("owned runtime child %d still alive or unreaped", ownedPID)
	}
}
