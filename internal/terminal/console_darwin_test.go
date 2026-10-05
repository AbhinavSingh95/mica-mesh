package terminal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"golang.org/x/sys/unix"
)

func openPTY(t *testing.T) (*os.File, *os.File, int) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "pty-master")
	t.Cleanup(func() { master.Close() })
	for _, op := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, op, 0); err != nil {
			t.Fatal(err)
		}
	}
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		t.Fatal(errno)
	}
	slaveFD, err := unix.Open(string(bytes.TrimRight(name[:], "\x00")), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.IoctlSetWinsize(slaveFD, unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 24})
	slave := os.NewFile(uintptr(slaveFD), "pty-slave")
	t.Cleanup(func() { slave.Close() })
	slave.Write([]byte("\n"))
	return master, slave, slaveFD
}
func fdFlags(t *testing.T, fd int) int {
	t.Helper()
	n, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestConsolePreservesNonblockingFlags(t *testing.T) { testConsoleRestoration(t, true) }
func TestConsoleRestoresMergedDescriptors(t *testing.T) { testConsoleRestoration(t, false) }
func testConsoleRestoration(t *testing.T, nonblock bool) {
	_, _, fd := openPTY(t)
	if nonblock {
		unix.SetNonblock(fd, true)
	}
	flags := fdFlags(t, fd)
	state, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if fdFlags(t, fd) != flags || !reflect.DeepEqual(state, got) {
		t.Fatal("terminal state was not restored")
	}
}

type testModel struct {
	ready     chan struct{}
	seen      chan tea.Msg
	panicView bool
}

func (m testModel) Init() tea.Cmd { return nil }
func (m testModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.seen != nil {
		select {
		case m.seen <- msg:
		default:
		}
	}
	return m, nil
}
func (m testModel) View() tea.View {
	if m.panicView {
		panic("private-prompt")
	}
	if m.ready != nil {
		select {
		case <-m.ready:
		default:
			close(m.ready)
		}
	}
	v := tea.NewView(lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("Agent: waiting"))
	v.AltScreen = true
	return v
}
func runPTY(t *testing.T, c *Console, master *os.File, model tea.Model, owner func(context.Context, *tea.Program) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	master.SetReadDeadline(time.Now().Add(4 * time.Second))
	done := make(chan struct{})
	go func() { defer close(done); io.Copy(io.Discard, master) }()
	err := c.run(ctx, model, owner)
	master.SetReadDeadline(time.Now())
	<-done
	return err
}
func TestResizeRestoresTerminal(t *testing.T) {
	master, _, fd := openPTY(t)
	state, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	flags := fdFlags(t, fd)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	seen := make(chan tea.Msg, 16)
	err = runPTY(t, c, master, testModel{ready: ready, seen: seen}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: 83, Row: 24})
		syscall.Kill(os.Getpid(), syscall.SIGWINCH)
		for {
			select {
			case msg := <-seen:
				if sz, ok := msg.(tea.WindowSizeMsg); ok && sz.Width == 83 {
					return nil
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if fdFlags(t, fd) != flags || !reflect.DeepEqual(state, got) {
		t.Fatal("resize changed terminal state")
	}
}
func TestTraceEnvironmentRestoredAfterOwnerJoin(t *testing.T) {
	t.Setenv("TEA_TRACE", "secret-trace")
	t.Setenv("TEA_DEBUG", "true")
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	err = runPTY(t, c, master, testModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if _, ok := os.LookupEnv("TEA_TRACE"); ok {
			return errors.New("trace remained enabled")
		}
		if _, ok := os.LookupEnv("TEA_DEBUG"); ok {
			return errors.New("debug remained enabled")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TEA_TRACE") != "secret-trace" || os.Getenv("TEA_DEBUG") != "true" {
		t.Fatal("environment not restored")
	}
}
func TestColorDisabledKeepsStateLabels(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan struct{})
	data := make(chan string, 1)
	master.SetReadDeadline(time.Now().Add(3 * time.Second))
	go func() {
		var b [8192]byte
		var out strings.Builder
		for {
			n, err := master.Read(b[:])
			out.Write(b[:n])
			if strings.Contains(out.String(), "Agent: waiting") || err != nil {
				data <- out.String()
				return
			}
		}
	}()
	var output string
	err = c.run(ctx, testModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
		select {
		case output = <-data:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Agent: waiting") || strings.Contains(output, "[31m") || strings.Contains(output, "38;2;") {
		t.Fatalf("invalid no-color output: %q", output)
	}
}

func TestRoleScreenColorPolicyThroughPTY(t *testing.T) {
	for _, role := range []config.Role{config.RoleController, config.RoleWorker} {
		for _, noColor := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/noColor=%v", role, noColor), func(t *testing.T) {
				t.Setenv("TERM", "xterm-256color")
				t.Setenv("NO_COLOR", "")
				if !noColor {
					if err := os.Unsetenv("NO_COLOR"); err != nil {
						t.Fatal(err)
					}
				}
				master, _, fd := openPTY(t)
				c, err := Open(fd, fd, fd)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				data := make(chan string, 1)
				master.SetReadDeadline(time.Now().Add(3 * time.Second))
				go func() {
					var buf [8192]byte
					var out strings.Builder
					for out.Len() < 128*1024 {
						n, err := master.Read(buf[:])
						out.Write(buf[:n])
						if strings.Contains(out.String(), "F3 Roles") || err != nil {
							break
						}
					}
					data <- out.String()
				}()
				var output string
				received := false
				m := newScreen(Options{Config: config.Default(), Role: role})
				label := "Checking"
				if role == config.RoleController {
					m = readyScreen()
					label = "1 ready"
				}
				err = c.run(ctx, m, func(ctx context.Context, p *tea.Program) error {
					select {
					case output = <-data:
						received = true
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				if !received {
					master.SetReadDeadline(time.Now())
					output = <-data // Join the owned reader even after a failure.
				}
				if err != nil {
					t.Fatal(err)
				}
				colored := regexp.MustCompile(`\x1b\[[0-9;:]*[34]8[;:]`).MatchString(output)
				if colored == noColor || !strings.Contains(output, "Mica Mesh") || !strings.Contains(output, label) || (noColor && strings.Contains(output, "\x1b]12;")) {
					t.Fatalf("role labels or color policy lost (NO_COLOR=%v): %q", noColor, output)
				}
			})
		}
	}
}
func TestInputCancelJoinsReader(t *testing.T) {
	for _, input := range []string{"\x1b[200~unterminated", "\x1b]unterminated", "\x1b"} {
		master, _, fd := openPTY(t)
		c, err := Open(fd, fd, fd)
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		err = runPTY(t, c, master, testModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := master.Write([]byte(input))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestBrokenOutputCancelsOwner(t *testing.T) {
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ready := make(chan struct{})
	joined := false
	err = c.run(ctx, testModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		master.Close()
		<-ctx.Done()
		joined = true
		return nil
	})
	if err == nil || !joined {
		t.Fatalf("output failure did not cancel and join owner: %v joined=%v", err, joined)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("output failure waited for parent timeout: %v", err)
	}
}
func TestLoneEscapeAndRejectedPasteRemainUsable(t *testing.T) {
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ready := make(chan struct{})
	seen := make(chan tea.Msg, 32)
	err = runPTY(t, c, master, testModel{ready: ready, seen: seen}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if _, err := master.Write([]byte("\x1b")); err != nil {
			return err
		}
		for {
			select {
			case msg := <-seen:
				if k, ok := msg.(tea.KeyPressMsg); ok && k.Code == tea.KeyEscape {
					goto paste
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	paste:
		if _, err := master.Write([]byte(pasteStart + strings.Repeat("x", promptLimit+1) + pasteEnd + "z")); err != nil {
			return err
		}
		notice, valid := false, false
		for !notice || !valid {
			select {
			case msg := <-seen:
				switch m := msg.(type) {
				case diagnosticsMsg:
					notice = strings.Contains(c.diagnostics.text(), "Input rejected")
				case tea.PasteMsg:
					return errors.New("rejected paste reached model")
				case tea.KeyPressMsg:
					if m.Text == "z" {
						valid = true
					} else if m.Text != "" {
						return errors.New("paste prefix reached model")
					}
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestInputCancellationInterruptsBlockedGuard(t *testing.T) {
	_, slave, fd := openPTY(t)
	_ = slave
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	done := make(chan error, 1)
	go func() { var b [16]byte; _, err := c.input.Read(b[:]); done <- err }()
	c.input.Cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation missing")
		}
	case <-time.After(time.Second):
		t.Fatal("input read did not join")
	}
}

type blockedModel struct{ entered, release chan struct{} }

func (m blockedModel) Init() tea.Cmd  { return nil }
func (m blockedModel) View() tea.View { return tea.NewView("Blocked test") }
func (m blockedModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	select {
	case <-m.entered:
	default:
		close(m.entered)
		<-m.release
	}
	return m, nil
}
func TestSIGTERMCancelsOwnerWhileBridgeBlocked(t *testing.T) {
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	canceled := false
	err = runPTY(t, c, master, blockedModel{entered, release}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			return ctx.Err()
		}
		c.Diagnostics().Write([]byte("message bridge may block\n"))
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-ctx.Done():
			canceled = true
			close(release)
			return nil
		case <-time.After(time.Second):
			close(release)
			return errors.New("SIGTERM receiver blocked behind model")
		}
	})
	if err != nil || !canceled {
		t.Fatalf("termination failed: %v canceled=%v", err, canceled)
	}
}
func TestStartupPanicRestoresTerminalAndEnvironment(t *testing.T) {
	t.Setenv("TEA_DEBUG", "true")
	master, _, fd := openPTY(t)
	state, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	flags := fdFlags(t, fd)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	err = runPTY(t, c, master, testModel{panicView: true}, func(ctx context.Context, p *tea.Program) error { <-ctx.Done(); joined = true; return nil })
	if !errors.Is(err, tea.ErrProgramPanic) || !joined {
		t.Fatalf("panic not joined: %v", err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if fdFlags(t, fd) != flags || !reflect.DeepEqual(state, got) || os.Getenv("TEA_DEBUG") != "true" {
		t.Fatal("panic restoration failed")
	}
}
func TestDiagnosticRetentionAndSanitation(t *testing.T) {
	d := diagnosticBuffer{wake: make(chan struct{}, 1)}
	d.Write([]byte("\x1b]52;c;"))
	d.Write([]byte("secret\aok\n"))
	if got := d.text(); got != "ok\n" {
		t.Fatalf("unsafe diagnostic: %q", got)
	}
	d.Write([]byte(strings.Repeat("z", 128*1024)))
	if len(d.text()) != 64*1024 {
		t.Fatal("diagnostic bound failed")
	}
}

type profileModel struct {
	ready   chan struct{}
	changed bool
}

func (m profileModel) Init() tea.Cmd { return nil }
func (m profileModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(string); ok {
		m.changed = true
	}
	return m, nil
}
func (m profileModel) View() tea.View {
	select {
	case <-m.ready:
	default:
		close(m.ready)
	}
	label := "waiting"
	if m.changed {
		label = "changed"
	}
	return tea.NewView(lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("Agent: " + label))
}
func TestNoColorSurvivesCapabilityReply(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan struct{})
	readDone := make(chan string, 1)
	master.SetReadDeadline(time.Now().Add(3 * time.Second))
	go func() {
		var buf [2048]byte
		var out strings.Builder
		for {
			n, err := master.Read(buf[:])
			out.Write(buf[:n])
			if strings.Contains(out.String(), "changed") || err != nil {
				readDone <- out.String()
				return
			}
		}
	}()
	var output string
	err = c.run(ctx, profileModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		p.Send(tea.CapabilityMsg{Content: "RGB"})
		p.Send("change")
		select {
		case output = <-readDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "[31m") || strings.Contains(output, "38;2;") || !strings.Contains(output, "changed") {
		t.Fatalf("color capability bypassed NO_COLOR: %q", output)
	}
}
func TestTraceEnvironmentRestoresAbsentAndEmpty(t *testing.T) {
	t.Setenv("TEA_TRACE", "temporary")
	if err := os.Unsetenv("TEA_TRACE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEA_DEBUG", "")
	_, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TEA_TRACE", "TEA_DEBUG"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("%s remains enabled", name)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("TEA_TRACE"); ok {
		t.Fatal("absent trace variable was recreated")
	}
	if value, ok := os.LookupEnv("TEA_DEBUG"); !ok || value != "" {
		t.Fatal("empty debug variable was lost")
	}
}
func TestCloseReportsRestorationFailure(t *testing.T) {
	_, _, fd := openPTY(t)
	flags := fdFlags(t, fd)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.descriptors[1].file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err == nil {
		t.Fatal("lost descriptor restore failure")
	}
	if fdFlags(t, fd) != flags {
		t.Fatal("other descriptor was not restored")
	}
}
func TestStalledOutputIsBoundedAndCancelsOwner(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	canceled := make(chan struct{})
	var once sync.Once
	writer := &displayWriter{file: write, cancel: func() { once.Do(func() { close(canceled) }) }}
	done := make(chan error, 1)
	go func() { _, err := writer.Write([]byte(strings.Repeat("x", 1024*1024))); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("stall error=%v", err)
		}
	case <-time.After(3 * time.Second):
		write.Close()
		<-done
		t.Fatal("stalled output did not stop")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("write failure did not cancel owner")
	}
}

func TestResizeFailureRetainsCauseAfterJoin(t *testing.T) {
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	pipeFD := int(write.Fd())
	if err := unix.SetNonblock(pipeFD, true); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	canceled := false
	err = runPTY(t, c, master, testModel{ready: ready}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if e := unix.Dup2(pipeFD, c.descriptors[1].fd); e != nil {
			return e
		}
		if e := syscall.Kill(os.Getpid(), syscall.SIGWINCH); e != nil {
			return e
		}
		<-ctx.Done()
		canceled = true
		return nil
	})
	if e := unix.Dup2(fd, c.descriptors[1].fd); e != nil {
		t.Fatal(e)
	}
	if !canceled || !errors.Is(err, unix.ENOTTY) || !strings.Contains(err.Error(), "resize terminal") {
		t.Fatalf("canceled=%v err=%v", canceled, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedInputCancelsSessionBeforeBodyCommands(t *testing.T) {
	master, _, fd := openPTY(t)
	c, err := Open(fd, fd, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ready := make(chan struct{})
	seen := make(chan tea.Msg, 32)
	joined := false
	err = runPTY(t, c, master, testModel{ready: ready, seen: seen}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-ready:
		case <-ctx.Done():
			return ctx.Err()
		}
		if e := master.SetWriteDeadline(time.Now().Add(time.Second)); e != nil {
			return e
		}
		if _, e := master.Write([]byte(win32Records(pasteStart) + "q\x03" + pasteEnd + "z")); e != nil {
			return e
		}
		<-ctx.Done()
		joined = true
		return nil
	})
	if !joined || err == nil || !strings.Contains(err.Error(), "unsupported terminal input") {
		t.Fatalf("joined=%v err=%v", joined, err)
	}
	for len(seen) > 0 {
		if key, ok := (<-seen).(tea.KeyPressMsg); ok {
			t.Fatalf("post-error key: %#v", key)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

type interruptQuitModel struct{ testModel }

func (m interruptQuitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(interruptMsg); ok {
		return m, tea.Quit
	}
	return m, nil
}

func TestConsoleSignalLifetimeHelper(t *testing.T) {
	window := os.Getenv("MICA_CONSOLE_SIGNAL_HELPER")
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
	fmt.Fprintln(status, "ready")
	var barrier [1]byte
	if _, err := io.ReadFull(release, barrier[:]); err != nil {
		t.Fatal(err)
	}
	c, err := Open(0, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if window == "after-run" {
		if err := c.run(ctx, testModel{}, func(context.Context, *tea.Program) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Fprintln(status, "window")
	// The parent sends the terminal signal, then a separately registered signal
	// to acknowledge signal dispatch while this helper stays outside run/Close.
	select {
	case <-ack:
	case <-ctx.Done():
		t.Fatal("signal acknowledgment missing")
	}
	fmt.Fprintln(status, "received")
	if _, err := io.ReadFull(release, barrier[:]); err != nil {
		t.Fatal(err)
	}
	if window == "before-run" {
		if err := c.run(ctx, interruptQuitModel{}, func(ctx context.Context, _ *tea.Program) error { <-ctx.Done(); return nil }); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatal("queued termination or interrupt did not end run")
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(status, "closed")
	// Keep the child alive while the parent inspects the shared PTY flags.
	if _, err := io.ReadFull(release, barrier[:]); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleSignalsCoverOpenThroughClose(t *testing.T) {
	for _, window := range []string{"before-run", "after-run"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(fmt.Sprintf("%s-%s", window, sig), func(t *testing.T) {
				_, slave, fd := openPTY(t)
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
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConsoleSignalLifetimeHelper$")
				cmd.Env = append(os.Environ(), "MICA_CONSOLE_SIGNAL_HELPER="+window)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
				cmd.ExtraFiles = []*os.File{statusW, releaseR}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				waited := false
				defer func() {
					if !waited {
						cancel()
						cmd.Wait()
					}
				}()
				statusW.Close()
				releaseR.Close()
				statusR.SetReadDeadline(time.Now().Add(4 * time.Second))
				status := bufio.NewReader(statusR)
				read := func(want string) {
					t.Helper()
					line, err := status.ReadString('\n')
					if err != nil || line != want+"\n" {
						t.Fatalf("barrier %s: %q %v", want, line, err)
					}
				}
				read("ready")
				flags := fdFlags(t, fd)
				state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprint(releaseW, "x")
				read("window")
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Process.Signal(syscall.SIGUSR1); err != nil {
					t.Fatal(err)
				}
				read("received")
				fmt.Fprint(releaseW, "x")
				read("closed")
				restored, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
				if err != nil || !reflect.DeepEqual(state, restored) || fdFlags(t, fd) != flags {
					t.Fatalf("signal window did not restore terminal: %v", err)
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
