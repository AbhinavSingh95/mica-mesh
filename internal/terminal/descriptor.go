package terminal

import (
	"os"
	"sync"
	"time"

	"github.com/muesli/cancelreader"
)

// terminalInput exposes a captured descriptor, never os.File.Fd during polling.
// Close is borrowed: Console closes the descriptor after the parser has joined.
type terminalInput struct {
	file     *os.File
	fd       int
	guard    inputGuard
	mu       sync.Mutex
	canceled bool
}

func newTerminalInput(file *os.File, fd int, rejected func()) *terminalInput {
	r := &terminalInput{file: file, fd: fd}
	r.guard = inputGuard{source: inputSource{r}, rejected: rejected, ambiguity: r.deadline}
	return r
}
func (r *terminalInput) Read(p []byte) (int, error)  { return r.guard.Read(p) }
func (r *terminalInput) Write(p []byte) (int, error) { return r.file.Write(p) }
func (r *terminalInput) Name() string                { return r.file.Name() }
func (r *terminalInput) Fd() uintptr                 { return uintptr(r.fd) }
func (r *terminalInput) Close() error                { r.Cancel(); return nil }
func (r *terminalInput) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled = true
	return r.file.SetReadDeadline(time.Now()) == nil
}
func (r *terminalInput) deadline(escape bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.canceled {
		return cancelreader.ErrCanceled
	}
	var deadline time.Time
	if escape {
		deadline = time.Now().Add(50 * time.Millisecond)
	}
	return r.file.SetReadDeadline(deadline)
}

type inputSource struct{ input *terminalInput }

func (s inputSource) Read(p []byte) (int, error) {
	r := s.input
	r.mu.Lock()
	canceled := r.canceled
	r.mu.Unlock()
	if canceled {
		return 0, cancelreader.ErrCanceled
	}
	n, err := r.file.Read(p)
	r.mu.Lock()
	canceled = r.canceled
	r.mu.Unlock()
	if canceled {
		return 0, cancelreader.ErrCanceled
	}
	return n, err
}
