package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshruntime "github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

func configFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Defaults protect omitted fields in every command and keep the model identity shared.
func TestDefaults(t *testing.T) {
	got := config.Default()
	backend := "cpu"
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		backend = "metal"
	}
	want := config.Config{
		ControllerListen: "0.0.0.0:50051", WorkerListen: "0.0.0.0:50052",
		RuntimePort: 8080, Backend: backend, Model: "qwen2.5-0.5b-instruct-q4_k_m",
		MaxOutputTokens: 128, Timeout: 300 * time.Second,
		ModelDescriptor: meshruntime.Model{ID: "qwen2.5-0.5b-instruct-q4_k_m", SHA256: "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db", ContextTokens: 2048},
	}
	if got != want {
		t.Fatalf("Default() = %+v, want %+v", got, want)
	}
	if err := config.Validate(got, config.RoleNone); err != nil {
		t.Fatal(err)
	}
}

// Misspellings and wrong JSON types must never silently become defaults.
func TestStrictConfig(t *testing.T) {
	syntaxCases := []string{
		`{"unknown":1}`, `{"model_descriptor":{"unknown":1}}`, `{"timeout":300}`,
		`{"runtime_port":"8080"}`, `{"max_output_tokens":1.5}`, `{"backend":null}`,
		`{"model_descriptor":null}`, `{"model_descriptor":{"context_tokens":null}}`,
		`{"MODEL_DESCRIPTOR":{"context_tokens":null}}`,
		`{"model_descriptor":{"context_tokens":null},"model_descriptor":{}}`,
		`{"max_output_tokens":null,"max_output_tokens":128}`,
		`null`, `[]`, `{"timeout":"not-a-duration"}`,
	}
	for _, contents := range syntaxCases {
		t.Run(contents, func(t *testing.T) {
			if _, err := config.Load(configFile(t, contents), true); err == nil {
				t.Fatal("Load accepted invalid JSON configuration")
			}
		})
	}
	if _, err := config.Load(configFile(t, `{} {}`), true); err == nil {
		t.Error("Load accepted trailing JSON")
	}
	for _, contents := range []string{
		`{"model_descriptor":{"sha256":"bad"}}`, `{"model_descriptor":{"sha256":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}}`,
		`{"runtime_port":0}`, `{"runtime_port":65536}`, `{"controller_listen":"0.0.0.0:0"}`,
		`{"worker_listen":"0.0.0.0:65536"}`, `{"controller_address":"host"}`, `{"backend":"cuda"}`,
		`{"max_output_tokens":0}`, `{"max_output_tokens":513}`, `{"timeout":"301s"}`, `{"timeout":"0s"}`,
		`{"advertise_address":"0.0.0.0"}`, `{"advertise_address":"::1"}`, `{"model_descriptor":{"context_tokens":0}}`,
		`{"model_descriptor":{"id":""}}`, `{"model":""}`,
		`{"model_descriptor":{"context_tokens":4294967296}}`,
	} {
		t.Run(contents, func(t *testing.T) {
			cfg, err := config.Load(configFile(t, contents), true)
			if err != nil {
				t.Fatalf("typed JSON must load before overrides: %v", err)
			}
			if err := config.Validate(cfg, config.RoleNone); err == nil {
				t.Fatal("Validate accepted invalid configuration")
			}
		})
	}
	for _, contents := range []string{`{"max_output_tokens":1,"timeout":"1s","runtime_port":1}`, `{"max_output_tokens":512,"runtime_port":65535,"model_descriptor":{"context_tokens":4294967295}}`} {
		cfg, err := config.Load(configFile(t, contents), true)
		if err != nil {
			t.Fatal(err)
		}
		if err := config.Validate(cfg, config.RoleNone); err != nil {
			t.Fatal(err)
		}
	}
}

// Only explicitly visited CLI flags replace file values; Load preserves zero for validation.
func TestConfigPrecedence(t *testing.T) {
	cfg, err := config.Load(configFile(t, `{"controller_address":"mesh.local:6000","runtime_port":9000,"max_output_tokens":0,"timeout":"10s","backend":"cpu","model_descriptor":{"context_tokens":4096}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerAddress != "mesh.local:6000" || cfg.RuntimePort != 9000 || cfg.MaxOutputTokens != 0 || cfg.Timeout != 10*time.Second || cfg.ModelDescriptor.ContextTokens != 4096 || cfg.ModelDescriptor.ID != "qwen2.5-0.5b-instruct-q4_k_m" {
		t.Errorf("file values/defaults lost: %+v", cfg)
	}
	cfg.MaxOutputTokens = 42
	if err := config.Validate(cfg, config.RoleNone); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	got, err := config.Load(missing, false)
	if err != nil || got != config.Default() {
		t.Errorf("optional missing file: %+v, %v", got, err)
	}
	if _, err := config.Load(missing, true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("required missing file: %v", err)
	}
	if _, err := config.Load(t.TempDir(), false); err == nil {
		t.Error("non-missing read failure was ignored")
	}
}

// Structural validation allows startup to expose missing files as an unhealthy worker.
func TestRoleValidation(t *testing.T) {
	cfg := config.Default()
	if err := config.Validate(cfg, config.RoleController); err != nil {
		t.Fatal(err)
	}
	cfg.ControllerAddress = "mesh.local:50051"
	if err := config.Validate(cfg, config.RoleController); err == nil {
		t.Error("controller joined another controller")
	}
	cfg.ControllerAddress = ""
	for _, paths := range []struct{ binary, model string }{
		{"", "/missing/model"},
		{"/missing/server", ""},
		{"relative/server", "/missing/model"},
		{"/missing/server", "relative/model"},
	} {
		cfg.RuntimeBinary = paths.binary
		cfg.ModelPath = paths.model
		if err := config.Validate(cfg, config.RoleWorker); err == nil {
			t.Errorf("worker accepted paths %q, %q", paths.binary, paths.model)
		}
	}
	cfg.RuntimeBinary = "/missing/server"
	cfg.ModelPath = "/missing/model"
	if err := config.Validate(cfg, config.RoleController|config.RoleWorker); err != nil {
		t.Fatal(err)
	}
}

// File size is bounded before JSON parsing, including otherwise valid padding.
func TestConfigSize(t *testing.T) {
	contents := "{}" + strings.Repeat(" ", 64*1024-2)
	cfg, err := config.Load(configFile(t, contents), true)
	if err != nil || cfg != config.Default() {
		t.Fatalf("64 KiB boundary: %+v, %v", cfg, err)
	}
	if _, err := config.Load(configFile(t, contents+" "), true); err == nil {
		t.Fatal("Load accepted a configuration larger than 64 KiB")
	}
}
