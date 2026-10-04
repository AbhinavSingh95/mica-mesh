package main

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"testing"
	"time"
)

func TestOwnedInputCancellationAndFlags(t *testing.T) {
	for _, nonblocking := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocking", true: "nonblocking"}[nonblocking], func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			fd := int(r.Fd())
			original := flags(t, fd) &^ unix.O_NONBLOCK
			if nonblocking {
				original |= unix.O_NONBLOCK
			}
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, original); err != nil {
				t.Fatal(err)
			}
			input := captureInput(fd)
			if err := input.SetReadDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			callbackDone := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { defer close(callbackDone); _ = input.SetReadDeadline(time.Now()) })
			readDone := make(chan error, 1)
			go func() { var b [1]byte; _, err := input.Read(b[:]); readDone <- err }()
			cancel()
			select {
			case err := <-readDone:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("read error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("reader did not stop")
			}
			if !stop() {
				<-callbackDone
			}
			if err := input.close(); err != nil {
				t.Fatal(err)
			}
			if got := flags(t, fd); got != original {
				t.Fatalf("flags=%#x want=%#x", got, original)
			}
		})
	}
}
func TestUnusedClosedInput(t *testing.T) {
	input := captureInput(-1)
	if err := input.close(); err != nil {
		t.Fatalf("unused input error: %v", err)
	}
	if _, err := input.IsTerminal(); err == nil {
		t.Fatal("closed input accepted")
	}
}
func TestOwnedInputInitializationFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	fd := int(r.Fd())
	input := captureInput(fd)
	r.Close()
	if _, err := input.Read(make([]byte, 1)); err == nil {
		t.Fatal("closed descriptor read passed")
	}
	if err := input.close(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedInputUnsupportedDeadlineRestoresFlags(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd := int(file.Fd())
	original := flags(t, fd)
	input := captureInput(fd)
	if err := input.SetReadDeadline(time.Now()); err == nil {
		t.Fatal("null input unexpectedly supports a cancellation deadline")
	}
	if err := input.close(); err != nil {
		t.Fatal(err)
	}
	if got := flags(t, fd); got != original {
		t.Fatalf("partial initialization left flags=%#x, want=%#x", got, original)
	}
}
