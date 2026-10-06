// Package terminal owns the guided terminal's descriptors and I/O lifetime.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

type descriptor struct {
	file      *os.File
	fd, flags int
	state     *term.State
}
type savedEnv struct {
	name, value string
	present     bool
}

// Console has exclusive ownership of these inherited open descriptions and the
// process trace environment until Close. Close follows run and all application
// joins. Concurrent consoles in one process are not supported.
type Console struct {
	descriptors  []descriptor
	input        *terminalInput
	output       *displayWriter
	diagnostics  diagnosticBuffer
	environment  []savedEnv
	closeOnce    sync.Once
	closeErr     error
	interrupts   chan os.Signal
	terminations chan os.Signal
	resizes      chan os.Signal
}

// Available inspects only the inherited descriptors. It never opens /dev/tty.
func Available(inputFD, outputFD, errorFD int) bool {
	for i, fd := range []int{inputFD, outputFD, errorFD} {
		if !term.IsTerminal(uintptr(fd)) {
			return false
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return false
		}
		mode := flags & unix.O_ACCMODE
		if i == 0 && mode == unix.O_WRONLY || i > 0 && mode == unix.O_RDONLY {
			return false
		}
	}
	return true
}

// Open captures all modes and flags before modifying any shared open description.
func Open(inputFD, outputFD, errorFD int) (*Console, error) {
	if !Available(inputFD, outputFD, errorFD) {
		return nil, errors.New("guided mode needs terminal input, output, and errors; use --plain")
	}
	fds := []int{inputFD, outputFD, errorFD}
	saved := make([]descriptor, len(fds))
	for i, fd := range fds {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return nil, err
		}
		state, err := term.GetState(uintptr(fd))
		if err != nil {
			return nil, err
		}
		saved[i] = descriptor{flags: flags, state: state}
	}
	c := &Console{
		interrupts:   make(chan os.Signal, 1),
		terminations: make(chan os.Signal, 1),
		resizes:      make(chan os.Signal, 1),
	}
	// Own signals before the first shared descriptor mutation, including parsing
	// before run. Separate slots keep resize/interrupt traffic from displacing TERM.
	signal.Notify(c.interrupts, os.Interrupt)
	signal.Notify(c.terminations, syscall.SIGTERM)
	signal.Notify(c.resizes, syscall.SIGWINCH)
	c.diagnostics.wake = make(chan struct{}, 1)
	fail := func(err error) (*Console, error) { return nil, errors.Join(err, c.Close()) }
	for i, fd := range fds {
		duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			return fail(err)
		}
		d := saved[i]
		d.fd = duplicate
		if _, err := unix.FcntlInt(uintptr(duplicate), unix.F_SETFL, d.flags|unix.O_NONBLOCK); err != nil {
			return fail(errors.Join(err, unix.Close(duplicate)))
		}
		d.file = os.NewFile(uintptr(duplicate), fmt.Sprintf("mica-terminal-%d", i))
		c.descriptors = append(c.descriptors, d)
		if i == 0 {
			err = d.file.SetReadDeadline(time.Time{})
		} else {
			err = d.file.SetWriteDeadline(time.Time{})
		}
		if err != nil {
			return fail(fmt.Errorf("terminal fd %d cannot support cancellation: %w", fd, err))
		}
	}
	c.input = newTerminalInput(c.descriptors[0].file, c.descriptors[0].fd, func() {
		fmt.Fprintln(&c.diagnostics, "Input rejected. Use at most 16 KiB per paste and shorter terminal sequences.")
	})
	c.output = &displayWriter{file: c.descriptors[1].file}
	for _, name := range []string{"TEA_TRACE", "TEA_DEBUG"} {
		value, present := os.LookupEnv(name)
		c.environment = append(c.environment, savedEnv{name, value, present})
		if err := os.Unsetenv(name); err != nil {
			return fail(err)
		}
	}
	return c, nil
}

// Close restores once, after all readers, renderer, signals and application work join.
func (c *Console) Close() error {
	c.closeOnce.Do(func() {
		// Keep interception active through the final restoration, even after run joins.
		defer signal.Stop(c.interrupts)
		defer signal.Stop(c.terminations)
		defer signal.Stop(c.resizes)
		for i := len(c.descriptors) - 1; i >= 0; i-- {
			d := c.descriptors[i]
			c.closeErr = errors.Join(c.closeErr, term.Restore(uintptr(d.fd), d.state))
			_, err := unix.FcntlInt(uintptr(d.fd), unix.F_SETFL, d.flags)
			c.closeErr = errors.Join(c.closeErr, err, d.file.Close())
		}
		for _, env := range c.environment {
			var err error
			if env.present {
				err = os.Setenv(env.name, env.value)
			} else {
				err = os.Unsetenv(env.name)
			}
			c.closeErr = errors.Join(c.closeErr, err)
		}
	})
	return c.closeErr
}

// Diagnostics receives logs without writing behind the renderer. Retention is
// separate from transcript text and is capped at 64 KiB.
func (c *Console) Diagnostics() io.Writer { return &c.diagnostics }

type interruptMsg struct{}
type diagnosticsMsg struct{}

// run is the U4 lifetime seam. owner must join its work before returning and
// honor its context. It may use Program.Send; a failed program cancels it directly.
// Model methods remain immediate. Application effects never run as tea.Cmd.
func (c *Console) run(ctx context.Context, model tea.Model, owner func(context.Context, *tea.Program) error) error {
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	c.input.onEOF = cancelSession
	// Keep rendering alive during application cleanup after external cancellation.
	// Each write remains bounded independently. Renderer failure cancels both owners.
	programCtx, cancelProgram := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelProgram()
	c.output.cancel = func() { cancelSession(); cancelProgram() }
	stopped := make(chan struct{})
	stopDeadline := context.AfterFunc(programCtx, func() { c.input.Cancel(); c.output.stop(); close(stopped) })
	defer func() {
		if !stopDeadline() {
			<-stopped
		}
	}()
	width, height, err := term.GetSize(uintptr(c.descriptors[1].fd))
	if err != nil {
		return err
	}
	profile := colorprofile.TrueColor
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		profile = colorprofile.Ascii
	}
	filter := func(_ tea.Model, msg tea.Msg) tea.Msg {
		if profile == colorprofile.Ascii {
			switch msg.(type) {
			case tea.CapabilityMsg:
				return nil
			case tea.ColorProfileMsg:
				return tea.ColorProfileMsg{Profile: colorprofile.Ascii}
			}
		}
		return msg
	}
	p := tea.NewProgram(model, tea.WithContext(programCtx), tea.WithInput(c.input), tea.WithOutput(c.output), tea.WithEnvironment(os.Environ()), tea.WithWindowSize(width, height), tea.WithFPS(20), tea.WithColorProfile(profile), tea.WithoutSignalHandler(), tea.WithFilter(filter))
	var bridgeErr error // Written by the bridge, read only after bridgeDone.
	bridgeDone, signalDone := make(chan struct{}), make(chan struct{})
	bridgeCtx, stopBridge := context.WithCancel(programCtx)
	interrupts, resizes := make(chan struct{}, 1), make(chan struct{}, 1)
	// Signal receipt never waits for the model. SIGTERM cancels application work
	// even when the one message bridge is blocked inside Program.Send.
	go func() {
		defer close(signalDone)
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case <-c.terminations:
				cancelSession()
			case <-c.interrupts:
				select {
				case interrupts <- struct{}{}:
				default:
				}
			case <-c.resizes:
				select {
				case resizes <- struct{}{}:
				default:
				}
			}
		}
	}()
	go func() {
		defer close(bridgeDone)
		for {
			select {
			case <-bridgeCtx.Done():
				return
			case <-interrupts:
				p.Send(interruptMsg{})
			case <-resizes:
				w, h, e := term.GetSize(uintptr(c.descriptors[1].fd))
				if e != nil {
					bridgeErr = fmt.Errorf("resize terminal: %w", e)
					cancelSession()
					cancelProgram()
					return
				}
				p.Send(tea.WindowSizeMsg{Width: w, Height: h})
			case <-c.diagnostics.wake:
				p.Send(diagnosticsMsg{})
			}
		}
	}()
	programDone := make(chan error, 1)
	go func() { _, err := p.Run(); cancelSession(); programDone <- err }()
	ownerErr := owner(sessionCtx, p)
	cancelSession()
	p.Quit()
	programErr := <-programDone
	stopBridge()
	<-signalDone
	<-bridgeDone
	// Program.Run has joined library I/O. The callback must also join before Close.
	if stopDeadline() {
		close(stopped)
	}
	<-stopped
	return errors.Join(ownerErr, programErr, bridgeErr, c.output.failure())
}

type displayWriter struct {
	file    *os.File
	cancel  context.CancelFunc
	mu      sync.Mutex
	err     error
	stopped bool
}

func (w *displayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return 0, context.Canceled
	}
	err := w.file.SetWriteDeadline(time.Now().Add(2 * time.Second))
	w.mu.Unlock()
	n := 0
	if err == nil {
		n, err = w.file.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
		w.cancel()
	}
	return n, err
}
func (w *displayWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	_ = w.file.SetWriteDeadline(time.Now())
}
func (w *displayWriter) failure() error { w.mu.Lock(); defer w.mu.Unlock(); return w.err }

type diagnosticBuffer struct {
	mu    sync.Mutex
	data  [64 * 1024]byte
	n     int
	wake  chan struct{}
	clean sanitizer
}

func (d *diagnosticBuffer) Write(p []byte) (int, error) {
	d.mu.Lock()
	size := len(p)
	// Sanitize in bounded chunks so even a large log write has bounded temporary storage.
	for len(p) > 0 {
		n := min(len(p), 4096)
		clean := d.clean.text(string(p[:n]))
		p = p[n:]
		if d.n+len(clean) > len(d.data) {
			drop := d.n + len(clean) - len(d.data)
			d.n = copy(d.data[:], d.data[drop:d.n])
		}
		d.n += copy(d.data[d.n:], clean)
	}
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return size, nil
}
func (d *diagnosticBuffer) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return string(d.data[:d.n])
}

// Report emits a final diagnostic after run has joined all display writers.
// It also handles a parse failure before run. It cannot block shutdown indefinitely.
func (c *Console) Report(err error) error {
	file := c.descriptors[2].file
	if deadlineErr := file.SetWriteDeadline(time.Now().Add(250 * time.Millisecond)); deadlineErr != nil {
		return deadlineErr
	}
	var clean sanitizer
	text := err.Error()
	if len(text) > escapeLimit {
		text = text[:escapeLimit]
	}
	_, writeErr := fmt.Fprintf(file, "mica-mesh: %s\n", clean.text(text))
	return writeErr
}
