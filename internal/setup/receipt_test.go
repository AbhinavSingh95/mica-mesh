package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePinnedInstallation(t *testing.T) (*Manager, string) {
	t.Helper()
	root, release := releaseFixture(t)
	m := New(Layout{ReleaseRoot: root, DataRoot: canonicalTempDir(t)}, release, nil)
	model := activeModel(m)
	if err := os.MkdirAll(filepath.Dir(model), 0700); err != nil {
		t.Fatal(err)
	}
	// Sparse fixture: read-only resolution checks metadata; startup hashes bytes.
	f, err := os.OpenFile(model, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(f.Truncate(491400032), f.Close()); err != nil {
		t.Fatal(err)
	}
	receipt := map[string]any{"schema": 1, "version": "0.1.0", "architecture": m.release.Architecture, "backend": m.release.Backend, "runtime_version": "0.5.0", "runtime_commit": "7fe450e19305b828c199d602c23a8337aaa1f03b", "files": m.release.Files, "model": map[string]any{"id": "qwen2.5-0.5b-instruct-q4_k_m", "size_bytes": 491400032, "sha256": "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db", "context_tokens": 2048}, "verified_at": time.Now().UTC().Format(time.RFC3339Nano)}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	name := activeReceipt(m)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return m, name
}
func TestLoadInstallationReadOnly(t *testing.T) {
	m, name := writePinnedInstallation(t)
	before, _ := os.Stat(name)
	got, err := LoadInstallation(context.Background(), m.layout, m.release)
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelPath != activeModel(m) || got.RuntimeBinary != filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server") {
		t.Fatalf("paths %+v", got)
	}
	after, _ := os.Stat(name)
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatal("read replaced receipt")
	}
	requireAbsent(t, filepath.Join(m.layout.DataRoot, "setup.lock"))
	requireAbsent(t, filepath.Join(m.layout.DataRoot, "staging"))
}
func TestLoadInstallationRejectsReceiptFields(t *testing.T) {
	for _, kind := range []string{"duplicate", "case", "unknown", "null", "model", "files", "architecture", "backend", "version", "time", "trailing", "oversize", "receipt_path", "model_path"} {
		t.Run(kind, func(t *testing.T) {
			m, name := writePinnedInstallation(t)
			data, _ := os.ReadFile(name)
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "duplicate":
				data = append([]byte(`{"schema":1,`), data[1:]...)
			case "case":
				data = []byte(strings.Replace(string(data), `"schema"`, `"Schema"`, 1))
			case "unknown":
				fields["url"] = "https://attacker.invalid/model"
			case "null":
				fields["files"] = nil
			case "model":
				fields["model"].(map[string]any)["sha256"] = strings.Repeat("a", 64)
			case "files":
				fields["files"].([]any)[0].(map[string]any)["path"] = "/tmp/executable"
			case "architecture":
				fields["architecture"] = "unsupported"
			case "backend":
				fields["backend"] = "other"
			case "version":
				fields["version"] = "other"
			case "time":
				fields["verified_at"] = ""
			case "trailing":
				data = append(data, []byte(` {}`)...)
			case "oversize":
				data = append(data, []byte(strings.Repeat(" ", 65537))...)
			case "receipt_path":
				fields["runtime_binary"] = "/tmp/executable"
			case "model_path":
				fields["model"].(map[string]any)["path"] = "/tmp/model"
			}
			switch kind {
			case "duplicate", "case", "trailing", "oversize":
			default:
				data, _ = json.Marshal(fields)
			}
			if err := os.WriteFile(name, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadInstallation(context.Background(), m.layout, m.release); err == nil {
				t.Fatal("accepted invalid receipt")
			}
		})
	}
}
func TestLoadInstallationRejectsUnsafeOrMissingFiles(t *testing.T) {
	for _, kind := range []string{"receipt_link", "model_link", "directory_link", "hardlink", "model_size", "runtime_size", "missing"} {
		t.Run(kind, func(t *testing.T) {
			m, receipt := writePinnedInstallation(t)
			model := activeModel(m)
			runtime := filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server")
			switch kind {
			case "receipt_link", "model_link":
				name := model
				if kind == "receipt_link" {
					name = receipt
				}
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.Rename(name, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, name); err != nil {
					t.Fatal(err)
				}
			case "directory_link":
				name := filepath.Join(m.layout.DataRoot, "models")
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.Rename(name, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, name); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(model, filepath.Join(t.TempDir(), "another")); err != nil {
					t.Fatal(err)
				}
			case "model_size":
				if err := os.Truncate(model, 3); err != nil {
					t.Fatal(err)
				}
			case "runtime_size":
				if err := os.Truncate(runtime, 3); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadInstallation(context.Background(), m.layout, m.release); err == nil {
				t.Fatal("accepted unsafe or missing file")
			}
		})
	}
}
