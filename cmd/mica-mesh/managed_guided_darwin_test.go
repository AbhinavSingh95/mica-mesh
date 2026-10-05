package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
)

// This proxy only observes CONNECT. It cannot forward, terminate TLS, or serve
// model bytes. The stock CLI retains the actual URL, size, and digest pins.
type heldSetupProxy struct {
	server   *httptest.Server
	requests chan string
	ended    chan struct{}
	reject   atomic.Bool
}

func newHeldSetupProxy(t *testing.T) *heldSetupProxy {
	t.Helper()
	p := &heldSetupProxy{requests: make(chan string, 8), ended: make(chan struct{}, 8)}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case p.ended <- struct{}{}:
			default:
			}
		}()
		if r.Method != http.MethodConnect {
			t.Errorf("unexpected proxy method %s", r.Method)
			http.Error(w, "CONNECT required", 405)
			return
		}
		select {
		case p.requests <- r.Host:
		default:
			t.Error("unexpected setup request count")
		}
		if p.reject.Load() {
			http.Error(w, "controlled proxy refusal", http.StatusBadGateway)
			return
		}
		<-r.Context().Done()
	}))
	p.server.Config.ReadHeaderTimeout = 2 * time.Second
	p.server.Config.MaxHeaderBytes = 4096
	p.server.Start()
	t.Cleanup(func() { p.server.CloseClientConnections(); p.server.Close() })
	return p
}
func (p *heldSetupProxy) env(t *testing.T, home string) []string {
	// Darwin's test root can start with /var, a symlink. Managed storage
	// deliberately requires physical, no-follow directory components.
	physical, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	home = physical
	return terminalEnvironment("HOME="+home, "HTTPS_PROXY="+p.server.URL, "HTTP_PROXY="+p.server.URL, "NO_PROXY=", "https_proxy="+p.server.URL, "http_proxy="+p.server.URL, "no_proxy=")
}
func (p *heldSetupProxy) await(t *testing.T) {
	t.Helper()
	select {
	case host := <-p.requests:
		if host != "huggingface.co:443" {
			t.Fatalf("production publisher changed or proxy target unexpected: %s", host)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("managed setup did not reach controlled CONNECT")
	}
}
func noActivatedSetup(t *testing.T, home string) {
	t.Helper()
	root := filepath.Join(home, "Library", "Application Support", "Mica Mesh")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if strings.HasPrefix(rel, "staging/") || strings.HasPrefix(rel, "models/") || strings.HasPrefix(rel, "installations/") {
				t.Errorf("interrupted setup left activated or staged file: %s", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStockGuidedAgentConsentHasNoEffects(t *testing.T) {
	for _, ending := range []string{"decline", "interrupt", "term"} {
		t.Run(ending, func(t *testing.T) {
			binary, home, root := consentBundle(t)
			proxy := newHeldSetupProxy(t)
			p := startTerminal(t, binary, []string{"agent", "--local"}, proxy.env(t, home))
			p.await("Y Prepare")
			for _, text := range []string{"qwen2.5-0.5b-instruct-q4_k_m", "491400032", "https://huggingface.co/", "Store:"} {
				if !strings.Contains(p.text(), text) {
					t.Fatalf("consent missing %q:\n%s", text, p.text())
				}
			}
			if ending == "term" {
				p.finish(syscall.SIGTERM, "")
			} else {
				if ending == "decline" {
					p.send("n")
				} else {
					p.send("\x03")
				}
				p.await("Choose a role")
				p.finish(nil, "\x03")
			}
			select {
			case host := <-proxy.requests:
				t.Fatalf("download before consent: %s", host)
			default:
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("consent changed home: %v %v", entries, err)
			}
			if _, err := setup.LoadRelease(context.Background(), root); err != nil {
				t.Fatalf("consent changed release: %v", err)
			}
		})
	}
}

func TestStockGuidedSetupCancelReleasesLockAndStaging(t *testing.T) {
	for _, ending := range []string{"interrupt", "term"} {
		t.Run(ending, func(t *testing.T) {
			binary, home, _ := consentBundle(t)
			proxy := newHeldSetupProxy(t)
			p := startTerminal(t, binary, []string{"agent", "--local"}, proxy.env(t, home))
			p.await("Y Prepare")
			p.send("y")
			proxy.await(t)
			if ending == "term" {
				p.finish(syscall.SIGTERM, "")
			} else {
				p.send("\x03")
				p.await("Choose a role")
				p.finish(nil, "\x03")
			}
			waitTerminalEvent(t, proxy.ended)
			noActivatedSetup(t, home)
			// A second real process must pass the same non-waiting lock and reach
			// the proxy. Receipt inspection alone would not prove lock release.
			retry := startTerminal(t, binary, []string{"agent", "--local"}, proxy.env(t, home))
			retry.await("Y Prepare")
			retry.send("y")
			proxy.await(t)
			retry.finish(syscall.SIGTERM, "")
			waitTerminalEvent(t, proxy.ended)
			noActivatedSetup(t, home)
		})
	}
}

func TestStockGuidedSetupFailureKeepsRecoveryScreen(t *testing.T) {
	binary, home, _ := consentBundle(t)
	proxy := newHeldSetupProxy(t)
	proxy.reject.Store(true)
	p := startTerminal(t, binary, []string{"agent", "--local"}, proxy.env(t, home))
	p.await("Y Prepare")
	p.send("y")
	proxy.await(t)
	p.await("Failed")
	p.await("/retry")
	noActivatedSetup(t, home)
	p.clear()
	p.send("\x1b[15~") // F5: retry re-checks files and obtains consent again.
	p.await("Y Prepare")
	p.send("y")
	proxy.await(t)
	p.await("Failed")
	p.finish(syscall.SIGTERM, "")
	noActivatedSetup(t, home)
	if strings.Contains(p.text(), fmt.Sprintf("%s[31m", "\x1b")) {
		t.Fatal("NO_COLOR produced colored failure output")
	}
}
