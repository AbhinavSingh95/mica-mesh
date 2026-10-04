package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/muesli/cancelreader"
)

type controlledReader struct {
	entered  chan struct{}
	canceled chan struct{}
	once     sync.Once
	enter    sync.Once
	initial  string
}

func (r *controlledReader) Read(p []byte) (int, error) {
	r.enter.Do(func() { close(r.entered) })
	if r.initial != "" {
		n := copy(p, r.initial)
		r.initial = r.initial[n:]
		return n, nil
	}
	<-r.canceled
	return 0, cancelreader.ErrCanceled
}
func (r *controlledReader) Cancel() bool { r.once.Do(func() { close(r.canceled) }); return true }
func (r *controlledReader) Close() error { r.Cancel(); return nil }
func TestParserCancellationJoinsBlockedDelivery(t *testing.T) {
	r := &controlledReader{entered: make(chan struct{}), canceled: make(chan struct{}), initial: "a"}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan uv.Event)
	done := make(chan error, 1)
	go func() { done <- uv.NewTerminalReader(r, "xterm").StreamEvents(ctx, events) }()
	<-r.entered
	cancel()
	r.Cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Error("parser send failed to join")
		go func() {
			for {
				select {
				case <-events:
				case <-done:
					return
				}
			}
		}()
	}
}

type commandModel struct{ command tea.Cmd }

func (m commandModel) Init() tea.Cmd                           { return m.command }
func (m commandModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m commandModel) View() tea.View                          { return tea.NewView("") }
func TestLibraryJoinsAcceptedCommand(t *testing.T) {
	started, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p := tea.NewProgram(commandModel{func() tea.Msg { close(started); <-release; close(joined); return nil }}, tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("command never started")
	}
	killed := make(chan struct{})
	go func() { p.Kill(); close(killed) }()
	select {
	case <-killed:
		t.Error("Kill returned before command joined")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not join")
	}
	<-joined
	<-killed
}
func TestLibraryCommandPanicDoesNotSelfJoin(t *testing.T) {
	p := tea.NewProgram(commandModel{func() tea.Msg { panic("private prompt") }}, tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, tea.ErrProgramPanic) {
			t.Fatalf("panic error lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("panic self-joined")
	}
}

type cleanupWriter struct{ failure error }

func (w cleanupWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "\x1b[?2004l") {
		return 0, w.failure
	}
	return len(p), nil
}
func TestLibraryReportsRendererCleanupFailure(t *testing.T) {
	failure := errors.New("cleanup write failed")
	p := tea.NewProgram(commandModel{tea.Quit}, tea.WithInput(strings.NewReader("")), tea.WithOutput(cleanupWriter{failure}), tea.WithWindowSize(80, 24), tea.WithoutSignalHandler())
	_, err := p.Run()
	if !errors.Is(err, failure) {
		t.Fatalf("cleanup failure lost: %v", err)
	}
}
func TestPasteControlsDoNotHideEndMarker(t *testing.T) {
	for _, payload := range []string{"hello\x1b]52;c;hidden", "hello\x1bPunterminated", "hello\x1b[31mworld"} {
		ctx, cancel := context.WithCancel(context.Background())
		reader := uv.NewTerminalReader(strings.NewReader(pasteStart+payload+pasteEnd+"z"), "xterm")
		events := make(chan uv.Event, 16)
		done := make(chan error, 1)
		go func() { done <- reader.StreamEvents(ctx, events) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			cancel()
			t.Fatal("paste parser failed to stop")
		}
		cancel()
		var paste string
		valid := false
		for len(events) > 0 {
			switch msg := (<-events).(type) {
			case uv.PasteEvent:
				paste = msg.Content
			case uv.KeyPressEvent:
				valid = valid || msg.Text == "z"
			}
		}
		if paste != payload || !valid {
			t.Errorf("payload=%q paste=%q subsequent key=%v", payload, paste, valid)
		}
	}
}

type countedInput struct {
	source io.Reader
	bytes  atomic.Int64
}

func (r *countedInput) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}
func TestContinuousUnknownInputCannotGrowParserBuffer(t *testing.T) {
	r := &countedInput{source: strings.NewReader(strings.Repeat("\xff", 40000))}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan uv.Event)
	done := make(chan error, 1)
	go func() { done <- uv.NewTerminalReader(r, "xterm").StreamEvents(ctx, events) }()
	select {
	case <-events:
		if n := r.bytes.Load(); n > 3*4096 {
			t.Errorf("parser read %d bytes before processing bounded state", n)
		}
	case <-time.After(time.Second):
		t.Error("parser did not emit bounded input")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parser failed to join")
	}
}

// win32Records serializes raw Unicode key-downs, including terminal controls.
func win32Records(text string) string {
	var out strings.Builder
	for _, r := range text {
		fmt.Fprintf(&out, "\x1b[0;0;%d;1;0;1_", r)
	}
	return out.String()
}

func readGuardedEvents(t *testing.T, input string) ([]uv.Event, int, error) {
	t.Helper()
	rejections := 0
	guard := &inputGuard{source: byteReader{strings.NewReader(input)}, rejected: func() { rejections++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events := make(chan uv.Event, 32)
	err := uv.NewTerminalReader(guard, "xterm").StreamEvents(ctx, events)
	if ctx.Err() != nil {
		t.Fatalf("reader did not finish: %v %v", err, ctx.Err())
	}
	var got []uv.Event
	for len(events) > 0 {
		got = append(got, <-events)
	}
	return got, rejections, err
}

func TestReaderRejectsWin32InputBeforeDecoding(t *testing.T) {
	inputs := []string{win32Records("a"), "\x1b[65;0;97;1;0;1_", "\x9b0;0;97;1;0;1_", "\x1b[" + strings.Repeat("0", 10000) + ";0;97;1;0;1_"}
	for _, size := range []int{promptLimit, promptLimit + 1} {
		inputs = append(inputs, win32Records(pasteStart)+strings.Repeat("q", size)+win32Records(pasteEnd))
		inputs = append(inputs, win32Records(pasteStart+strings.Repeat("q", size)+pasteEnd))
	}
	for _, input := range inputs {
		events, _, err := readGuardedEvents(t, input+pasteStart+"later"+pasteEnd+"z")
		if err == nil || !strings.Contains(err.Error(), "unsupported terminal input") || !strings.Contains(err.Error(), "--plain") || len(events) != 0 {
			t.Fatalf("input bytes=%d events=%#v error=%v", len(input), events, err)
		}
	}
}

func TestReaderPasteKeepsLiteralWin32Records(t *testing.T) {
	for _, start := range []string{pasteStart, "\x9b0200;1~"} {
		for _, payload := range []string{win32Records("a"), win32Records(pasteEnd) + "after", win32Records(pasteStart) + "body", strings.Repeat("x", 4093) + win32Records("a") + "after"} {
			events, rejected, err := readGuardedEvents(t, start+payload+pasteEnd+"z")
			var paste, keys string
			for _, event := range events {
				switch event := event.(type) {
				case uv.PasteEvent:
					paste = event.Content
				case uv.KeyPressEvent:
					keys += event.Text
				}
			}
			if err != nil || rejected != 0 || paste != payload || keys != "z" {
				t.Fatalf("literal changed: rejected=%d bytes=%d want=%d keys=%q err=%v", rejected, len(paste), len(payload), keys, err)
			}
		}
	}
}

func TestReaderSupportedPasteLimitKeepsFollowingInput(t *testing.T) {
	for _, size := range []int{promptLimit, promptLimit + 1} {
		payload := strings.Repeat("q", size)
		events, rejected, err := readGuardedEvents(t, "\x9b0200;1~"+payload+pasteEnd+pasteStart+"valid"+pasteEnd+"z")
		var pastes []string
		var keys string
		for _, event := range events {
			switch event := event.(type) {
			case uv.PasteEvent:
				pastes = append(pastes, event.Content)
			case uv.KeyPressEvent:
				keys += event.Text
			}
		}
		want := []string{payload, "valid"}
		count := 0
		if size > promptLimit {
			want = []string{"valid"}
			count = 1
		}
		if err != nil || rejected != count || !reflect.DeepEqual(pastes, want) || keys != "z" {
			t.Fatalf("size=%d rejected=%d paste count=%d keys=%q err=%v", size, rejected, len(pastes), keys, err)
		}
	}
}

type guardedCancelableReader struct {
	guard  inputGuard
	source *controlledReader
}

func (r *guardedCancelableReader) Read(p []byte) (int, error) { return r.guard.Read(p) }
func (r *guardedCancelableReader) Cancel() bool               { return r.source.Cancel() }
func (r *guardedCancelableReader) Close() error               { return r.source.Close() }
func TestReaderCancellationJoinsRejectedPaste(t *testing.T) {
	source := &controlledReader{entered: make(chan struct{}), canceled: make(chan struct{}), initial: pasteStart + strings.Repeat("x", 1024*1024)}
	rejected := make(chan struct{})
	r := &guardedCancelableReader{source: source, guard: inputGuard{source: source, rejected: func() { close(rejected) }}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan uv.Event, 4)
	done := make(chan error, 1)
	go func() { done <- uv.NewTerminalReader(r, "xterm").StreamEvents(ctx, events) }()
	select {
	case <-rejected:
	case <-time.After(2 * time.Second):
		t.Fatal("oversized paste not rejected")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader failed to join")
	}
	select {
	case <-source.canceled:
	default:
		t.Fatal("source was not canceled")
	}
	if len(events) != 0 {
		t.Fatal("rejected paste emitted events")
	}
}
