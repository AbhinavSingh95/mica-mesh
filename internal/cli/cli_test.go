package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/cli"
)

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main(context.Background(), []string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("help exit status = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "mica-mesh") {
		t.Errorf("help output = %q, want program name", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("help stderr = %q, want empty", errOut.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main(context.Background(), []string{"unknown-command"}, &out, &errOut); code == 0 {
		t.Error("unknown command exit status = 0, want failure")
	}
	if !strings.Contains(errOut.String(), "unknown-command") || !strings.Contains(errOut.String(), "--help") {
		t.Errorf("stderr = %q, want offending command and help guidance", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("unknown command stdout = %q, want empty", out.String())
	}
}
