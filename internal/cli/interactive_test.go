package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
)

func TestGuidedPickerLoadsSavedConfigAndPresence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "mica-mesh")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"runtime_binary":"","max_output_tokens":42}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"ui"}} {
		o, err := guidedOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		if !o.Assets.RuntimeBinary || o.Config.MaxOutputTokens != 42 {
			t.Fatalf("saved settings lost: %+v", o)
		}
	}
}
func TestGuidedDirectRolePreservesLocalAndAssets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	o, err := guidedOptions([]string{"--verbose", "agent", "--local", "--runtime-binary", "/manual/runtime", "--model-path", "/manual/model"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Role != config.RoleWorker || o.Network != app.Local || o.Config.ControllerAddress != "127.0.0.1:50051" || !o.Assets.ModelPath {
		t.Fatalf("options %+v", o)
	}
}

func TestGuidedHelpDescribesFinishedFlows(t *testing.T) {
	for _, name := range []string{"ui", "controller", "agent"} {
		text := commandHelp(name)
		if !strings.Contains(text, "guided") || !strings.Contains(text, "--plain") {
			t.Errorf("%s help does not describe guided/plain paths", name)
		}
	}
	if !strings.Contains(usage, "mica-mesh ui") {
		t.Fatal("explicit UI command absent from usage")
	}
}
func TestUIHelpUsesPlainOutput(t *testing.T) {
	var out, errOut strings.Builder
	if code := Main(context.Background(), []string{"ui", "--help"}, strings.NewReader(""), &out, &errOut); code != 0 || out.Len() == 0 || errOut.Len() != 0 {
		t.Fatalf("ui help: code %d out %s err %s", code, out.String(), errOut.String())
	}
}
func TestGuidedInvalidSavedConfigReportsFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "mica-mesh")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := guidedOptions([]string{"ui"}); err == nil {
		t.Fatal("invalid saved configuration ignored")
	}
}
