package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
)

func TestExplicitEmptyPathDoesNotUseManagedDefault(t *testing.T) {
	m, _ := writePinnedInstallation(t)
	for _, key := range []string{"runtime_binary", "model_path", "RUNTIME_BINARY", "MODEL_PATH"} {
		t.Run(key, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(name, []byte(`{"`+key+`":""}`), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.Load(name, true)
			if err != nil {
				t.Fatal(err)
			}
			if strings.EqualFold(key, "runtime_binary") && !loaded.Assets.RuntimeBinary || strings.EqualFold(key, "model_path") && !loaded.Assets.ModelPath {
				t.Fatal("empty field has no presence")
			}
			cfg, err := ResolveManaged(context.Background(), loaded.Config, loaded.Assets, m.layout)
			if err == nil {
				err = config.Validate(cfg, config.RoleWorker)
			}
			var unavailable *ErrNotPrepared
			if err == nil || errors.As(err, &unavailable) {
				t.Fatalf("explicit empty path error = %v", err)
			}
		})
	}
}

func TestManualPathsWorkWithoutReceipt(t *testing.T) {
	cfg := config.Default()
	cfg.RuntimeBinary, cfg.ModelPath = "/manual/server", "/manual/model"
	for _, backend := range []bool{false, true} {
		got, err := ResolveManaged(context.Background(), cfg, config.AssetFields{RuntimeBinary: true, ModelPath: true, Backend: backend}, Layout{})
		if err != nil || got != cfg {
			t.Fatalf("manual config = %+v, err = %v", got, err)
		}
	}
}

func TestManagedPathsFillOnlyOmittedSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields config.AssetFields
	}{{"all", config.AssetFields{}}, {"manual_runtime", config.AssetFields{RuntimeBinary: true}}, {"manual_model", config.AssetFields{ModelPath: true}}, {"explicit_backend", config.AssetFields{Backend: true}}} {
		t.Run(test.name, func(t *testing.T) {
			fields := test.fields
			m, receipt := writePinnedInstallation(t)
			cfg := config.Default()
			cfg.Backend = m.release.Backend
			if fields.RuntimeBinary {
				cfg.RuntimeBinary = "/manual/server"
			}
			if fields.ModelPath {
				cfg.ModelPath = "/manual/model"
			}
			before, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ResolveManaged(context.Background(), cfg, fields, m.layout)
			if err != nil {
				t.Fatal(err)
			}
			wantRuntime := filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server")
			if fields.RuntimeBinary {
				wantRuntime = "/manual/server"
			}
			wantModel := filepath.Join(m.layout.DataRoot, "models", "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db", "model.gguf")
			if fields.ModelPath {
				wantModel = "/manual/model"
			}
			if got.RuntimeBinary != wantRuntime || got.ModelPath != wantModel || got.Backend != m.release.Backend || got.ModelDescriptor != cfg.ModelDescriptor {
				t.Fatalf("config = %+v", got)
			}
			after, err := os.ReadFile(receipt)
			if err != nil || string(before) != string(after) {
				t.Fatalf("resolution changed receipt: %v", err)
			}
			requireAbsent(t, filepath.Join(m.layout.DataRoot, "setup.lock"))
		})
	}
}

func TestExplicitBackendNeedsMatchingRuntime(t *testing.T) {
	m, _ := writePinnedInstallation(t)
	cfg := config.Default()
	cfg.Backend = "cpu"
	if m.release.Backend == "cpu" {
		cfg.Backend = "metal"
	}
	_, err := ResolveManaged(context.Background(), cfg, config.AssetFields{Backend: true}, m.layout)
	var unavailable *ErrNotPrepared
	if !errors.As(err, &unavailable) {
		t.Fatalf("unmatched runtime = %v", err)
	}
	cfg.RuntimeBinary = "/manual/server"
	got, err := ResolveManaged(context.Background(), cfg, config.AssetFields{RuntimeBinary: true, Backend: true}, m.layout)
	if err != nil || got.Backend != cfg.Backend || got.RuntimeBinary != cfg.RuntimeBinary || got.ModelPath == "" {
		t.Fatalf("manual backend = %+v, %v", got, err)
	}
}

func TestReceiptCannotRedirectExecution(t *testing.T) {
	m, name := writePinnedInstallation(t)
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["runtime_binary"] = "/outside/server"
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveManaged(context.Background(), config.Default(), config.AssetFields{}, m.layout)
	var unavailable *ErrNotPrepared
	if !errors.As(err, &unavailable) || got.RuntimeBinary == "/outside/server" {
		t.Fatalf("redirect = %+v, %v", got, err)
	}
}

func TestManagedResolutionRejectsUnpreparedFiles(t *testing.T) {
	for _, failure := range []string{"release", "receipt", "model_size", "runtime_link", "descriptor", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			m, receipt := writePinnedInstallation(t)
			cfg := config.Default()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var err error
			switch failure {
			case "release":
				err = os.Remove(filepath.Join(m.layout.ReleaseRoot, "release.json"))
			case "receipt":
				err = os.Remove(receipt)
			case "model_size":
				err = os.Truncate(activeModel(m), 1)
			case "runtime_link":
				server := filepath.Join(m.layout.ReleaseRoot, "runtime", m.release.Architecture, "llama-server")
				outside := filepath.Join(t.TempDir(), "server")
				if err = os.Rename(server, outside); err == nil {
					err = os.Symlink(outside, server)
				}
			case "descriptor":
				cfg.ModelDescriptor.ContextTokens++
			case "canceled":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveManaged(ctx, cfg, config.AssetFields{}, m.layout)
			var unavailable *ErrNotPrepared
			if !errors.As(err, &unavailable) {
				t.Fatalf("unprepared error = %v", err)
			}
			if failure == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
