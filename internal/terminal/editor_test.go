package terminal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestUnicodeInputLimitUsesBytes(t *testing.T) {
	m := newPrompt()
	m.SetValue(strings.Repeat("a", promptLimit-2))
	before := m.Value()
	var ok bool
	m, ok = updatePrompt(m, tea.PasteMsg{Content: "界"})
	if ok || m.Value() != before {
		t.Fatal("oversized UTF-8 paste changed prompt")
	}
	m.SelectAll()
	m, ok = updatePrompt(m, tea.PasteMsg{Content: "界"})
	if !ok || m.Value() != "界" {
		t.Fatalf("replacement failed %v %q", ok, m.Value())
	}
	m.SetValue(strings.Repeat("a", promptLimit))
	m, ok = updatePrompt(m, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	if ok || len(m.Value()) != promptLimit {
		t.Fatal("newline exceeded byte limit")
	}
}
func TestClipboardBindingsDoNotLaunchPrograms(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	for _, name := range []string{"pbcopy", "pbpaste"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	m := newPrompt()
	m.SetValue("selected")
	m.SelectAll()
	for _, k := range []tea.KeyPressMsg{{Code: 'v', Mod: tea.ModCtrl}, {Code: 'c', Mod: tea.ModCtrl | tea.ModShift}} {
		next, cmd := m.Update(k)
		m = next
		if cmd != nil {
			cmd()
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("clipboard command ran")
	}
	if m.KeyMap.Paste.Enabled() || m.KeyMap.CopySelection.Enabled() {
		t.Fatal("clipboard binding enabled")
	}
	if m.VirtualCursor() {
		t.Fatal("blink commands enabled")
	}
}

func TestPromptChecksNormalizedWholeInsertion(t *testing.T) {
	m := newPrompt()
	m.SetValue(strings.Repeat("a", promptLimit-1))
	before := m.Value()
	next, ok := updatePrompt(m, tea.PasteMsg{Content: "\t"})
	if ok || next.Value() != before {
		t.Errorf("tab accepted=%v bytes=%d", ok, len(next.Value()))
	}
	m.SelectAll()
	next, ok = updatePrompt(m, tea.PasteMsg{Content: strings.Repeat("\t", promptLimit/4+1)})
	if ok || next.Value() != before || next.SelectedText() != before {
		t.Error("rejected replacement changed selection/value")
	}
	next, ok = updatePrompt(m, tea.PasteMsg{Content: strings.Repeat("\t", promptLimit/4)})
	if !ok || next.Value() != strings.Repeat(" ", promptLimit) {
		t.Error("valid normalized replacement rejected/truncated")
	}
	m = newPrompt()
	payload := strings.Repeat("\n", promptLimit)
	next, ok = updatePrompt(m, tea.PasteMsg{Content: payload})
	if !ok || next.Value() != payload {
		t.Errorf("valid newlines accepted=%v bytes=%d", ok, len(next.Value()))
	}
	next, ok = updatePrompt(next, tea.PasteMsg{Content: "\n"})
	if ok || next.Value() != payload {
		t.Error("newline overflow changed value")
	}
	m = newPrompt()
	next, ok = updatePrompt(m, tea.PasteMsg{Content: "a\r\n\t\x01\ufffdb"})
	if !ok || next.Value() != "a\n\n    b" {
		t.Errorf("normalization %v %q", ok, next.Value())
	}
}
