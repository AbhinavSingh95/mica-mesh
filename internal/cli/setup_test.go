package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSetupRequiresConsentBeforeEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, diag bytes.Buffer
	if code := Main(context.Background(), []string{"setup"}, nil, &out, &diag); code == 0 || !strings.Contains(diag.String(), "--yes") {
		t.Fatalf("setup without terminal: code=%d diagnostic=%q", code, diag.String())
	}
}
func TestCommandSpecificHelp(t *testing.T) {
	for _, command := range []string{"setup", "doctor", "start", "run", "status"} {
		t.Run(command, func(t *testing.T) {
			var out, diag bytes.Buffer
			if code := Main(context.Background(), []string{command, "--help"}, nil, &out, &diag); code != 0 || !strings.HasPrefix(out.String(), "Usage: mica-mesh "+command) {
				t.Fatalf("help: code=%d output=%q diagnostic=%q", code, out.String(), diag.String())
			}
		})
	}
}

type terminalReader struct{ *strings.Reader }

func (terminalReader) IsTerminal() (bool, error) { return true, nil }

func TestSetupDeclineHasNoEffects(t *testing.T) {
	for _, answer := range []string{"no\n", "", "yes", strings.Repeat("x", 130) + "\n"} {
		t.Run(answer, func(t *testing.T) {
			var diag bytes.Buffer
			called := false
			err := prepareSetup(context.Background(), false, terminalReader{strings.NewReader(answer)}, &diag, setup.Layout{DataRoot: t.TempDir()}, func(context.Context, func(setup.Progress) error) (setup.Installation, error) {
				called = true
				return setup.Installation{}, nil
			})
			if err == nil || called || !strings.Contains(diag.String(), "491400032") {
				t.Fatalf("err=%v prepared=%v diagnostic=%q", err, called, diag.String())
			}
		})
	}
}
func TestSetupYesWorksWithoutTTY(t *testing.T) {
	var diag bytes.Buffer
	called := false
	err := prepareSetup(context.Background(), true, nil, &diag, setup.Layout{DataRoot: t.TempDir()}, func(context.Context, func(setup.Progress) error) (setup.Installation, error) {
		called = true
		return setup.Installation{}, nil
	})
	if err != nil || !called {
		t.Fatalf("err=%v prepared=%v", err, called)
	}
}
func TestSetupProgressUsesStderr(t *testing.T) {
	var diag bytes.Buffer
	err := prepareSetup(context.Background(), false, terminalReader{strings.NewReader("yes\n")}, &diag, setup.Layout{DataRoot: t.TempDir()}, func(ctx context.Context, emit func(setup.Progress) error) (setup.Installation, error) {
		return setup.Installation{}, emit(setup.Progress{Phase: setup.Downloading, CompletedBytes: 12, TotalBytes: 20})
	})
	if err != nil || !strings.Contains(diag.String(), "12/20") || !strings.Contains(diag.String(), "prepared") {
		t.Fatalf("err=%v diagnostic=%q", err, diag.String())
	}
}
func TestSetupFailedPromptHasNoEffects(t *testing.T) {
	called := false
	err := prepareSetup(context.Background(), false, terminalReader{strings.NewReader("yes\n")}, brokenSetupWriter{}, setup.Layout{}, func(context.Context, func(setup.Progress) error) (setup.Installation, error) {
		called = true
		return setup.Installation{}, nil
	})
	if err == nil || called {
		t.Fatalf("err=%v prepared=%v", err, called)
	}
}

type brokenSetupWriter struct{}

func (brokenSetupWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStartMissingAssetsGivesSetupCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out, diag bytes.Buffer
	code := Main(context.Background(), []string{"start", "--worker"}, nil, &out, &diag)
	if code == 0 || !strings.Contains(diag.String(), "mica-mesh setup") {
		t.Fatalf("code=%d diagnostic=%q", code, diag.String())
	}
}
func TestDoctorCommandScopeAndProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, test := range []struct {
		args    []string
		success bool
	}{{[]string{"doctor", "--role", "client"}, true}, {[]string{"doctor", "--role", "client", "--probe"}, false}, {[]string{"doctor", "--role", "controller", "--probe"}, false}, {[]string{"doctor", "--role", "worker"}, false}, {[]string{"doctor", "--runtime-binary=", "--model-path="}, false}} {
		var out, diag bytes.Buffer
		code := Main(context.Background(), test.args, nil, &out, &diag)
		if (code == 0) != test.success || out.Len() != 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", test.args, code, out.String(), diag.String())
		}
	}
}

// This cooperative reader models a deadline callback that is still finishing
// after Read has stopped. Consent must join it before clearing the deadline.
type joinedConsentInput struct {
	entered, readStarted, readReturned, finishCallback chan struct{}
}

func (i *joinedConsentInput) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		close(i.entered)
		<-i.finishCallback
	}
	return nil
}
func (i *joinedConsentInput) Read([]byte) (int, error) {
	close(i.readStarted)
	<-i.entered
	close(i.readReturned)
	return 0, os.ErrDeadlineExceeded
}
func TestConsentJoinsDeadlineCallback(t *testing.T) {
	input := &joinedConsentInput{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- readConsent(ctx, input) }()
	select {
	case <-input.readStarted:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case <-input.readReturned:
	case <-time.After(time.Second):
		t.Fatal("read did not stop")
	}
	select {
	case <-done:
		t.Fatal("returned before callback finished")
	default:
	}
	close(input.finishCallback)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback was not joined")
	}
}
func TestSetupRejectsIgnoredOptions(t *testing.T) {
	for _, option := range []string{"--runtime-binary=/tmp/server", "--config=/tmp/config", "--model-path=/tmp/model"} {
		if _, err := parse([]string{"setup", "--yes", option}); err == nil {
			t.Errorf("ignored %s", option)
		}
	}
}
