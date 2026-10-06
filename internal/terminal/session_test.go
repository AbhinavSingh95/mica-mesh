package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
)

func TestTextQueueBackpressureAndCancellation(t *testing.T) {
	wake := make(chan struct{}, 1)
	q := newTextQueue(wake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 8 {
		if err := q.put(ctx, strings.Repeat("a", 4096)); err != nil {
			t.Fatal(err)
		}
	}
	q.mu.Lock()
	n := q.n
	q.mu.Unlock()
	if n != 32768 {
		t.Fatalf("accepted bytes %d", n)
	}
	done := make(chan error, 1)
	go func() { done <- q.put(ctx, "b") }()
	select {
	case err := <-done:
		t.Fatalf("full queue did not block: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not join")
	}
	if got := q.drain(); len(got) != 32768 || strings.Contains(got, "b") {
		t.Fatalf("partial text not retained: %d", len(got))
	}
}
func TestStatusCapacityExpires(t *testing.T) {
	now := time.Now()
	if !capacityFresh(now, now.Add(2999*time.Millisecond)) || capacityFresh(now, now.Add(3*time.Second)) {
		t.Fatal("status expiry boundary")
	}
}
func TestControllerPromptIsIndependentAndFrozenWhileBusy(t *testing.T) {
	now := time.Now()
	m := newScreen(Options{Config: config.Default(), Role: config.RoleController})
	w := idleWorker()
	w.Model.Id = m.state.cfg.Model
	m.state.phase = running
	m.state.status = &meshv1.GetClusterStatusResponse{Workers: []*meshv1.WorkerInfo{w}}
	m.state.statusAt = now
	m.state.now = now
	m.prompt.SetValue("first")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a := <-m.controls.actions
	if a.kind != submit || a.value != "first" {
		t.Fatalf("submission %+v", a)
	}
	m.Update(tea.PasteMsg{Content: "hidden"})
	if m.prompt.Value() != "first" {
		t.Fatal("edit admitted while busy")
	}
	m.state.request = requestView{id: "first", joined: true}
	m.records = []record{{id: "first", prompt: "first", response: "answer", done: true}}
	m.prompt.SetValue("second")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = <-m.controls.actions
	if a.value != "second" {
		t.Fatal("history became context")
	}
}
func TestHistoryBoundsKeepCleanupAndUTF8(t *testing.T) {
	m := newScreen(Options{Role: config.RoleController})
	for i := 0; i < 40; i++ {
		m.records = append(m.records, record{prompt: strings.Repeat("p", 100), response: strings.Repeat("界", 400), done: true, cleanup: cleanupUnconfirmed})
		m.trimHistory()
	}
	if len(m.records) > 32 || !m.forgottenCleanup {
		t.Fatal("record limit erased cleanup")
	}
	m.records = append(m.records, record{prompt: strings.Repeat("p", 16384), response: strings.Repeat("界", 20000)})
	m.trimHistory()
	n := 0
	for _, r := range m.records {
		n += len(r.prompt) + len(r.response)
		if r.omitted {
			n += len(omission)
		}
	}
	if n > 32768 || !strings.Contains(m.View().Content, "Cleanup unconfirmed") {
		t.Fatalf("history bound/notice lost: %d", n)
	}
}
func TestTinyViewKeepsCancelAndExit(t *testing.T) {
	m := newScreen(Options{})
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 8})
	if !strings.Contains(m.View().Content, "Resize") {
		t.Fatal("no resize recovery")
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case <-m.controls.interrupt:
	default:
		t.Fatal("tiny view lost cancellation")
	}
}
