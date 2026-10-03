// Package config loads shared defaults and validates configuration after CLI overrides.
package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/runtime"
)

// Config contains command settings, with roles supplied separately by start flags.
// Timeout's file representation is a duration string, decoded by Load.
type Config struct {
	ControllerAddress string        `json:"controller_address"`
	ControllerListen  string        `json:"controller_listen"`
	WorkerListen      string        `json:"worker_listen"`
	AdvertiseAddress  string        `json:"advertise_address"`
	RuntimeBinary     string        `json:"runtime_binary"`
	ModelPath         string        `json:"model_path"`
	Backend           string        `json:"backend"`
	Model             string        `json:"model"`
	RuntimePort       int           `json:"runtime_port"`
	MaxOutputTokens   int           `json:"max_output_tokens"`
	Timeout           time.Duration `json:"-"`
	ModelDescriptor   runtime.Model `json:"model_descriptor"`
}

// Role is a set of process roles. Client commands use RoleNone; start requires a role.
type Role uint8

const (
	RoleNone       Role = 0
	RoleController Role = 1 << 0
	RoleWorker     Role = 1 << 1
)

// Default returns independent defaults for the demo model and native backend.
func Default() Config {
	backend := "cpu"
	if goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64" {
		backend = "metal"
	}
	return Config{
		ControllerListen: "0.0.0.0:50051", WorkerListen: "0.0.0.0:50052",
		RuntimePort: 8080, Backend: backend, Model: "qwen2.5-0.5b-instruct-q4_k_m",
		MaxOutputTokens: 128, Timeout: 300 * time.Second,
		ModelDescriptor: runtime.Model{
			ID:            "qwen2.5-0.5b-instruct-q4_k_m",
			SHA256:        "74a4da8c9fdbcd15bd1f6d01d621410d31c6fc00986f5eb687824e7b93d7a9db",
			ContextTokens: 2048,
		},
	}
}

// This fixed schema needs little space; bound input before parsing to 64 KiB.
const maxConfigBytes = 64 * 1024

// Load overlays strictly typed JSON onto defaults. Only a missing optional file
// is ignored. Semantic validation is deferred until explicitly visited CLI flags
// have been applied, so typed file values (including zero) can be overridden.
func Load(path string, required bool) (Config, error) {
	cfg := Default()
	file, err := os.Open(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read configuration %q: %w", path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return Config{}, fmt.Errorf("read configuration %q: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("configuration %q exceeds the 64 KiB input limit", path)
	}
	// encoding/json accepts null for scalar fields without changing their value;
	// reject it explicitly so a mistyped setting cannot silently retain a default.
	if err := rejectNull(data); err != nil {
		return Config{}, fmt.Errorf("decode configuration %q: %w", path, err)
	}
	decoded := struct {
		*Config
		Timeout string `json:"timeout"`
	}{Config: &cfg, Timeout: cfg.Timeout.String()}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return Config{}, fmt.Errorf("decode configuration %q: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return Config{}, fmt.Errorf("decode configuration %q: %w", path, err)
	}
	cfg.Timeout, err = time.ParseDuration(decoded.Timeout)
	if err != nil {
		return Config{}, fmt.Errorf("decode configuration %q timeout: %w", path, err)
	}
	return cfg, nil
}

// Configuration has no nullable values. Scan each token rather than decoding
// a map so null in an earlier duplicate member cannot be discarded.
func rejectNull(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return errors.New("configuration must be an object")
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if token == nil {
			return errors.New("configuration values must not be null")
		}
	}
}

// Validate checks product constraints and role conflicts after overrides. It
// does not inspect local files or bind ports: worker startup owns those checks,
// so missing artifacts can produce a visible unhealthy worker.
func Validate(cfg Config, roles Role) error {
	for _, setting := range []struct {
		name, address string
	}{
		{"controller_listen", cfg.ControllerListen},
		{"worker_listen", cfg.WorkerListen},
	} {
		if err := validateAddress(setting.address, false); err != nil {
			return fmt.Errorf("%s: %w", setting.name, err)
		}
	}
	if cfg.ControllerAddress != "" {
		if err := validateAddress(cfg.ControllerAddress, true); err != nil {
			return fmt.Errorf("controller_address: %w", err)
		}
	}
	if cfg.AdvertiseAddress != "" {
		ip := net.ParseIP(cfg.AdvertiseAddress)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("advertise_address must be a concrete IPv4 address")
		}
	}
	if cfg.RuntimePort < 1 || cfg.RuntimePort > 65535 {
		return errors.New("runtime_port must be between 1 and 65535")
	}
	if cfg.Backend != "metal" && cfg.Backend != "cpu" {
		return errors.New("backend must be metal or cpu")
	}
	if cfg.Model == "" {
		return errors.New("model must not be empty")
	}
	if cfg.MaxOutputTokens < 1 || cfg.MaxOutputTokens > 512 {
		return errors.New("max_output_tokens must be between 1 and 512")
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 300*time.Second {
		return errors.New("timeout must be positive and at most 300s")
	}
	if cfg.ModelDescriptor.ID == "" {
		return errors.New("model_descriptor.id must not be empty")
	}
	digest, err := hex.DecodeString(cfg.ModelDescriptor.SHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("model_descriptor.sha256 must be a 64-digit hexadecimal SHA-256 digest")
	}
	// The wire descriptor uses uint32: reject narrowing instead of truncating.
	if cfg.ModelDescriptor.ContextTokens < 1 || uint64(cfg.ModelDescriptor.ContextTokens) > 1<<32-1 {
		return errors.New("model_descriptor.context_tokens must be between 1 and 4294967295")
	}
	if roles&RoleController != 0 && cfg.ControllerAddress != "" {
		return errors.New("controller role cannot use controller_address")
	}
	if roles&RoleWorker != 0 {
		if !filepath.IsAbs(cfg.RuntimeBinary) {
			return errors.New("worker runtime_binary must be an absolute path")
		}
		if !filepath.IsAbs(cfg.ModelPath) {
			return errors.New("worker model_path must be an absolute path")
		}
	}
	return nil
}

func validateAddress(address string, requireHost bool) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must be HOST:PORT: %w", err)
	}
	if requireHost && host == "" {
		return errors.New("host must not be empty")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}
