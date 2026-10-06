package terminal

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"golang.org/x/sys/unix"
)

func TestInputEOFJoinsRunningRoleAndRestoresTerminal(t *testing.T) {
	master, _, fd := openPTY(t)
	flags := fdFlags(t, fd)
	original, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Keep the real descriptor/mode owner and parser, but close its private
	// reader boundary only after the role is running. Unlike closing a PTY
	// master, this isolates input EOF from simultaneous display failure.
	input, source := io.Pipe()
	defer input.Close()
	defer source.Close()
	c.input.guard.source = input
	o := Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}
	m := newScreen(o)
	box := newMailbox()
	e := testEffects()
	start := e.start
	var closed atomic.Bool
	e.start = func(ctx context.Context, cfg config.Config, role config.Role, opts app.Options) (*roleHandle, error) {
		h, err := start(ctx, cfg, role, opts)
		closeRole := h.close
		h.close = func() error { closed.Store(true); return closeRole() }
		return h, err
	}
	s := newSession(o, e, m.controls, box)
	var parentExpired bool
	err = runPTY(t, c, master, m, func(ctx context.Context, _ *tea.Program) error {
		done := make(chan error, 1)
		go func() { done <- s.run(ctx, nil) }()
		for box.snapshot().phase != running {
			select {
			case <-box.wake:
			case <-ctx.Done():
				source.Close()
				<-done
				return ctx.Err()
			}
		}
		if err := source.Close(); err != nil {
			t.Error(err)
		}
		result := <-done
		parentExpired = errors.Is(ctx.Err(), context.DeadlineExceeded)
		return result
	})
	if parentExpired || !closed.Load() || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EOF failed to join role promptly: expired=%v closed=%v error=%v", parentExpired, closed.Load(), err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil || *got != *original || fdFlags(t, fd) != flags {
		t.Fatalf("EOF did not restore terminal: %v", err)
	}
}
