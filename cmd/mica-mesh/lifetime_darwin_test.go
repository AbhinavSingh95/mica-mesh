package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"golang.org/x/sys/unix"
)

// terminalChild keeps its session leader alive until flags and termios have
// been checked. One bounded reader drains the display; no test opens /dev/tty.
type terminalChild struct {
	t                   *testing.T
	master, slave       *os.File
	fd, originalFlags   int
	original            unix.Termios
	cmd                 *exec.Cmd
	child               *os.Process
	status              *bufio.Reader
	statusFile, release *os.File
	done                chan error
	readerDone          chan struct{}
	wake                chan struct{}
	mu                  sync.Mutex
	output              string
	screen              testScreen
	readErr             error
	finished            bool
}

func stockCLI(t *testing.T) string {
	t.Helper()
	binary, err := runfiles.Rlocation("_main/cmd/mica-mesh/mica-mesh_/mica-mesh")
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

func terminalEnvironment(extra ...string) []string {
	// Tests select all proxy and terminal behavior, independent of the host shell.
	blocked := map[string]bool{"TERM": true, "NO_COLOR": true, "HOME": true, "HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true, "TEA_TRACE": true, "TEA_DEBUG": true, "MICA_TEST_CONTROL": true, "MICA_TEST_IGNORE_TERM": true}
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] && !strings.HasPrefix(name, "MICA_TEST_") {
			env = append(env, entry)
		}
	}
	env = append(env, "TERM=xterm-256color", "NO_COLOR=1", "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	return append(env, extra...)
}

func startTerminal(t *testing.T, binary string, args, env []string) *terminalChild {
	t.Helper()
	master, slave, fd := openPTY(t)
	if _, err := slave.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: 120, Row: 40}); err != nil {
		t.Fatal(err)
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	releaseR, releaseW, err := os.Pipe()
	if err != nil {
		statusR.Close()
		statusW.Close()
		t.Fatal(err)
	}
	p := &terminalChild{t: t, master: master, slave: slave, fd: fd, status: bufio.NewReaderSize(statusR, 128), statusFile: statusR, release: releaseW, done: make(chan error, 1), readerDone: make(chan struct{}), wake: make(chan struct{}, 1)}
	script := `trap '' INT
printf 'ready\n' >&3
read start <&4
"$@" <&0 &
child=$!
printf '%s\n' "$child" >&3
wait "$child"
printf '%s\n' "$?" >&3
read release <&4
`
	p.cmd = exec.Command("/bin/sh", append([]string{"-c", script, "session-owner", binary}, args...)...)
	p.cmd.Env = env
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = slave, slave, slave
	p.cmd.ExtraFiles = []*os.File{statusW, releaseR}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := p.cmd.Start(); err != nil {
		statusR.Close()
		statusW.Close()
		releaseR.Close()
		releaseW.Close()
		t.Fatal(err)
	}
	statusW.Close()
	releaseR.Close()
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		if !p.finished {
			if p.child == nil {
				// Startup failed before the PID handoff. Do not release a
				// keeper still waiting to launch an untracked child.
				_ = p.cmd.Process.Kill()
			} else {
				_ = p.child.Signal(syscall.SIGTERM)
				_ = p.statusFile.SetReadDeadline(time.Now().Add(8 * time.Second))
				if _, err := p.status.ReadString('\n'); err != nil {
					_ = p.child.Kill()
				}
				_, _ = fmt.Fprintln(p.release, "done")
			}
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				_ = p.cmd.Process.Kill()
				<-p.done
			}
		}
		_ = p.master.SetReadDeadline(time.Now())
		<-p.readerDone
		p.statusFile.Close()
		p.release.Close()
		if p.child != nil {
			p.child.Release()
		}
	})
	go func() {
		defer close(p.readerDone)
		var buf [4096]byte
		for {
			n, err := p.master.Read(buf[:])
			p.mu.Lock()
			p.output += string(buf[:n])
			p.screen.write(string(buf[:n]))
			if len(p.output) > 128*1024 {
				p.output = p.output[len(p.output)-128*1024:]
			}
			p.readErr = err
			p.mu.Unlock()
			select {
			case p.wake <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	if line := p.statusLine(); line != "ready" {
		t.Fatalf("session barrier %q", line)
	}
	p.originalFlags = flags(t, fd)
	state, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	p.original = *state
	fmt.Fprintln(releaseW, "start")
	pid, err := strconv.Atoi(p.statusLine())
	if err != nil {
		t.Fatal(err)
	}
	p.child, err = os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *terminalChild) statusLine() string {
	p.t.Helper()
	if err := p.statusFile.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		p.t.Fatal(err)
	}
	line, err := p.status.ReadString('\n')
	if err != nil {
		p.t.Fatalf("terminal owner: %v\n%s", err, p.text())
	}
	return strings.TrimSpace(line)
}
func (p *terminalChild) text() string { p.mu.Lock(); defer p.mu.Unlock(); return p.output }
func (p *terminalChild) view() string { p.mu.Lock(); defer p.mu.Unlock(); return p.screen.text() }
func (p *terminalChild) clear()       { p.mu.Lock(); p.output = ""; p.mu.Unlock() }
func (p *terminalChild) await(text string) {
	p.t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		if strings.Contains(p.view(), text) {
			return
		}
		select {
		case <-p.wake:
		case <-timer.C:
			p.t.Fatalf("missing terminal text %q:\n%s\nraw:\n%q", text, p.view(), p.text())
		}
	}
}
func (p *terminalChild) send(text string) {
	p.t.Helper()
	if err := p.master.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		p.t.Fatal(err)
	}
	if _, err := io.WriteString(p.master, text); err != nil {
		p.t.Fatal(err)
	}
}
func (p *terminalChild) resize(columns, rows uint16) {
	p.t.Helper()
	if err := unix.IoctlSetWinsize(p.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: columns, Row: rows}); err != nil {
		p.t.Fatal(err)
	}
	if err := p.child.Signal(syscall.SIGWINCH); err != nil {
		p.t.Fatal(err)
	}
}
func (p *terminalChild) finish(signal os.Signal, input string) {
	p.t.Helper()
	if signal != nil {
		if err := p.child.Signal(signal); err != nil {
			p.t.Fatal(err)
		}
	} else {
		p.send(input)
	}
	if code := p.statusLine(); code != "0" {
		p.t.Fatalf("guided exit=%s\n%s", code, p.text())
	}
	state, err := unix.IoctlGetTermios(p.fd, unix.TIOCGETA)
	if err != nil || *state != p.original || flags(p.t, p.fd) != p.originalFlags {
		p.t.Fatalf("terminal flags/echo/mode not restored: %v", err)
	}
	fmt.Fprintln(p.release, "done")
	select {
	case err := <-p.done:
		p.finished = true
		if err != nil {
			p.t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		p.t.Fatal("session keeper did not join")
	}
}

func freeEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

type terminalRuntime struct {
	server                 *httptest.Server
	model, config, address string
	launches               atomic.Int32
	pid                    atomic.Int64
	streams                atomic.Int32
	processing             atomic.Bool
	canceled, partial      chan struct{}
	prompts                chan string
}

func manualTerminalRuntime(t *testing.T) *terminalRuntime {
	t.Helper()
	f := &terminalRuntime{canceled: make(chan struct{}), partial: make(chan struct{}), prompts: make(chan string, 8)}
	f.model = filepath.Join(t.TempDir(), "tiny model.gguf")
	data := []byte("declared tiny runtime fixture model")
	if err := os.WriteFile(f.model, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/launch":
			var launch struct{ PID int }
			if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&launch); err != nil {
				t.Error(err)
				return
			}
			f.launches.Add(1)
			f.pid.Store(int64(launch.PID))
		case "/health":
			io.WriteString(w, `{"status":"ok"}`)
		case "/props":
			json.NewEncoder(w).Encode(map[string]any{"model_path": f.model, "total_slots": 1, "build_info": "version: 0.5.0 (build 1, commit 7fe450e)", "is_sleeping": false})
		case "/slots":
			fmt.Fprintf(w, `[{"id":0,"n_ctx":2048,"is_processing":%t}]`, f.processing.Load())
		case "/apply-template":
			var body struct {
				Messages []struct{ Role, Content string }
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if len(body.Messages) != 2 {
				t.Errorf("message count=%d", len(body.Messages))
				return
			}
			select {
			case f.prompts <- body.Messages[1].Content:
			default:
				t.Error("unbounded or replayed prompts")
			}
			io.WriteString(w, `{"prompt":"formatted prompt"}`)
		case "/tokenize":
			io.WriteString(w, `{"tokens":[1,2,3]}`)
		case "/completion":
			var body struct{ Stream bool }
			if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if !body.Stream {
				io.WriteString(w, `{"content":"warm","stop":true,"tokens_predicted":1}`)
				return
			}
			n := f.streams.Add(1)
			if n == 1 {
				f.processing.Store(true)
				for _, text := range []string{"partial 🌏", "\x1b]52;c;hostile", "payload\a", "\x1b[", "31m safe"} {
					data, _ := json.Marshal(map[string]any{"content": text, "stop": false})
					fmt.Fprintf(w, "data: %s\n\n", data)
					w.(http.Flusher).Flush()
				}
				close(f.partial)
				<-r.Context().Done()
				close(f.canceled)
				return
			}
			io.WriteString(w, "data: {\"content\":\"reused answer 🌏\",\"stop\":false}\n\ndata: {\"content\":\"\",\"stop\":true,\"stop_type\":\"eos\",\"tokens_evaluated\":3,\"tokens_predicted\":3}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		f.server.CloseClientConnections()
		f.server.Close()
		if pid := int(f.pid.Load()); pid != 0 {
			_ = unix.Kill(pid, syscall.SIGKILL)
		}
	})
	runtimeBinary, err := runfiles.Rlocation(os.Getenv("MICA_TEST_SERVER"))
	if err != nil {
		t.Fatal(err)
	}
	_, portText, _ := net.SplitHostPort(freeEndpoint(t))
	port, _ := strconv.Atoi(portText)
	f.address = freeEndpoint(t)
	f.config = filepath.Join(t.TempDir(), "config.json")
	digest := sha256.Sum256(data)
	cfg := map[string]any{"runtime_binary": runtimeBinary, "model_path": f.model, "backend": "cpu", "runtime_port": port, "model": "terminal-model", "model_descriptor": map[string]any{"id": "terminal-model", "sha256": fmt.Sprintf("%x", digest), "context_tokens": 2048}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, b, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func waitTerminalEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("runtime event missing")
	}
}
