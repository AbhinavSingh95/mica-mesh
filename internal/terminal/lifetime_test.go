package terminal

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

func TestEmptyResponseTurnsKeepTranscriptBounded(t *testing.T) {
	for _, runes := range []int{341, 5461} {
		t.Run(fmt.Sprint(runes), func(t *testing.T) {
			m := newScreen(Options{Role: config.RoleController})
			for i := 0; i < 200; i++ {
				cleanup := cleanupNone
				if i == 0 {
					cleanup = cleanupUnconfirmed
				}
				m.state.request = requestView{id: fmt.Sprint(i), prompt: strings.Repeat("界", runes), joined: true, finish: "stop", cleanup: cleanup}
				m.consume()
				bytes := 0
				for _, r := range m.records {
					bytes += len(r.prompt) + len(r.response)
					if r.omitted {
						bytes += len(omission)
					}
					if !utf8.ValidString(r.prompt) || !utf8.ValidString(r.response) {
						t.Fatal("retention broke UTF-8")
					}
				}
				if len(m.records) > 32 || bytes > 32*1024 {
					t.Fatalf("turn %d retained %d records and %d bytes", i, len(m.records), bytes)
				}
			}
			if len(m.records) != min(32, 32768/(runes*3)) || m.records[len(m.records)-1].id != "199" || !m.forgottenCleanup {
				t.Fatal("empty responses lost the newest request or old cleanup uncertainty")
			}

		})
	}
}

func TestRoundedHeartbeatAgeCannotConfirmOldCleanup(t *testing.T) {
	canceled := time.Unix(100, 0)
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		age     uint64
		want    bool
	}{
		{"sub-millisecond", time.Millisecond - time.Nanosecond, 0, false},
		{"exact boundary", time.Millisecond, 0, true},
		{"rounded older heartbeat", 1999 * time.Microsecond, 1, false},
		{"provably newer heartbeat", 2 * time.Millisecond, 1, true},
		{"untrusted age overflow", time.Second, math.MaxUint64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := idleWorker()
			w.HeartbeatAgeMilliseconds = tc.age
			if got := confirmsCleanup(w, w.WorkerId, canceled, canceled.Add(tc.elapsed)); got != tc.want {
				t.Fatalf("cleanup confirmed=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestGuidedPreparationProgressAndResultStayOwned(t *testing.T) {
	e := testEffects()
	e.check = func(ctx context.Context, o Options) (Options, bool, error) { return o, true, nil }
	release := make(chan struct{})
	var once atomic.Bool
	t.Cleanup(func() {
		if once.CompareAndSwap(false, true) {
			close(release)
		}
	})
	var prepared, started atomic.Int32
	e.prepare = func(ctx context.Context, o Options, report func(setup.Progress) error) (config.Config, error) {
		prepared.Add(1)
		if err := report(setup.Progress{Phase: setup.Downloading, CompletedBytes: 4, TotalBytes: 8}); err != nil {
			return o.Config, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return o.Config, ctx.Err()
		}
		if err := report(setup.Progress{Phase: setup.Complete, CompletedBytes: 8, TotalBytes: 8}); err != nil {
			return o.Config, err
		}
		o.Config.RuntimeBinary, o.Config.ModelPath = "/prepared/runtime", "/prepared/model"
		return o.Config, nil
	}
	e.start = func(ctx context.Context, cfg config.Config, role config.Role, options app.Options) (*roleHandle, error) {
		if cfg.RuntimeBinary != "/prepared/runtime" || cfg.ModelPath != "/prepared/model" {
			t.Error("role did not consume preparation result")
		}
		started.Add(1)
		return testEffects().start(ctx, cfg, role, options)
	}
	h := startSession(t, Options{Config: config.Default(), Role: config.RoleWorker, Network: app.Local}, e)
	h.await(t, func(s sessionSnapshot) bool { return s.phase == awaitingConsent })
	if prepared.Load() != 0 || started.Load() != 0 {
		t.Fatal("effect began before consent")
	}
	h.act(action{kind: consent})
	s := h.await(t, func(s sessionSnapshot) bool { return s.progress.CompletedBytes == 4 })
	if s.phase != preparing || started.Load() != 0 {
		t.Fatal("progress claimed completed preparation")
	}
	if once.CompareAndSwap(false, true) {
		close(release)
	}
	h.await(t, func(s sessionSnapshot) bool { return s.phase == running && s.agent.Report != nil })
	if prepared.Load() != 1 || started.Load() != 1 {
		t.Fatal("preparation or startup was replayed")
	}
}

func TestUnknownAgentMetadataCannotEnablePrompt(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController, Config: config.Default()})
	m.state.phase = running
	m.state.now = time.Now()
	m.state.statusAt = m.state.now
	w := idleWorker()
	w.WorkerId = "unknown-agent"
	w.State = 999
	w.Report.RuntimeState = 999
	w.Model.Id = m.state.cfg.Model
	m.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{nil, w}}
	if m.canSubmit() {
		t.Fatal("unknown state enabled inference")
	}
	m.activateCommand("agents")
	text := m.View().Content
	if !strings.Contains(text, "unknown-agent") || !strings.Contains(text, "Unavailable") {
		t.Fatal("unknown metadata has no readable fallback")
	}
}

func TestTinyTerminalRequestCancelCannotExitRole(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController})
	m.state.request = requestView{id: "active"}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
	if !strings.Contains(m.View().Content, "Resize") {
		t.Fatal("tiny terminal has no recovery action")
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case <-m.controls.cancelRequest:
	default:
		t.Fatal("tiny terminal dropped request cancel")
	}
	select {
	case <-m.controls.interrupt:
		t.Fatal("tiny terminal turned request cancel into role exit")
	default:
	}
}
