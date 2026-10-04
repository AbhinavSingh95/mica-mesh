package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/cli"
	"golang.org/x/sys/unix"
)

// The helper pauses the existing plain entrypoint's ownership sequence at both
// sides of Main. Only this test binary recognizes the helper environment key.
func TestPlainSignalWindowHelper(t *testing.T) {
	window := os.Getenv("MICA_PLAIN_SIGNAL_WINDOW")
	if window == "" {
		return
	}
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	ack := make(chan os.Signal, 1)
	signal.Notify(ack, syscall.SIGUSR1)
	defer signal.Stop(ack)
	status := os.NewFile(3, "status")
	defer status.Close()
	release := os.NewFile(4, "release")
	defer release.Close()
	barrier := func() {
		t.Helper()
		var b [1]byte
		if _, err := io.ReadFull(release, b[:]); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Fprintln(status, "ready")
	barrier()
	plain, err := openPlain(context.Background())
	defer plain.close()
	if err != nil {
		t.Fatal(err)
	}
	if window == "after-main" {
		if code := cli.Main(plain.ctx, []string{"--help"}, plain.input, plain.outputs[0].file, plain.outputs[1].file); code != 0 {
			t.Fatalf("Main returned %d", code)
		}
	}
	fmt.Fprintln(status, "window")
	select {
	case <-ack:
	case <-time.After(3 * time.Second):
		t.Fatal("signal dispatch acknowledgment missing")
	}
	select {
	case <-plain.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("plain owner did not cancel its context")
	}
	fmt.Fprintln(status, "received")
	barrier()
	if err := plain.close(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(status, "restored")
	barrier()
}

func TestPlainSignalsCoverDescriptorLifetime(t *testing.T) {
	for _, window := range []string{"before-main", "after-main"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(fmt.Sprintf("%s-%s", window, sig), func(t *testing.T) {
				master, slave, fd := openPTY(t)
				drained := make(chan struct{})
				go func() { defer close(drained); _, _ = io.Copy(io.Discard, master) }()
				defer func() { _ = master.SetReadDeadline(time.Now()); <-drained }()
				if _, err := slave.Write([]byte("\n")); err != nil {
					t.Fatal(err)
				}
				statusR, statusW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer statusR.Close()
				defer statusW.Close()
				releaseR, releaseW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer releaseR.Close()
				defer releaseW.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlainSignalWindowHelper$")
				cmd.Env = append(os.Environ(), "MICA_PLAIN_SIGNAL_WINDOW="+window, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				cmd.ExtraFiles = []*os.File{statusW, releaseR}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waited := false
				defer func() {
					if !waited {
						cancel()
						_ = cmd.Wait()
					}
				}()
				statusW.Close()
				releaseR.Close()
				if err := statusR.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				reader := bufio.NewReaderSize(statusR, 128)
				read := func(want string) {
					t.Helper()
					line, err := reader.ReadString('\n')
					if err != nil || line != want+"\n" {
						t.Fatalf("%s barrier: %q %v", want, line, err)
					}
				}
				read("ready")
				originalFlags := flags(t, fd)
				original, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprint(releaseW, "x")
				read("window")
				if flags(t, fd)&unix.O_NONBLOCK == 0 {
					t.Fatal("window did not hold mutated output flags")
				}
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Process.Signal(syscall.SIGUSR1); err != nil {
					t.Fatal(err)
				}
				read("received")
				fmt.Fprint(releaseW, "x")
				read("restored")
				state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
				if err != nil || *state != *original || flags(t, fd) != originalFlags {
					t.Fatalf("plain terminal not restored: %v", err)
				}
				fmt.Fprint(releaseW, "x")
				if err := cmd.Wait(); err != nil {
					waited = true
					t.Fatal(err)
				}
				waited = true
			})
		}
	}
}
