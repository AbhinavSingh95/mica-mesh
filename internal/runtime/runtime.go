// Package runtime defines the replaceable inference boundary owned by a worker.
package runtime

import (
	"context"
	"errors"
)

// Config describes one local runtime instance. Paths are absolute; Port is loopback HTTP.
type Config struct {
	BinaryPath, ModelPath, Backend string
	Port                           int
	Model                          Model
}

// Model identifies the configured artifact and its total input/output context budget.
type Model struct {
	ID            string `json:"id"`
	SHA256        string `json:"sha256"`
	ContextTokens int    `json:"context_tokens"`
}

// Request is one validated user turn. Template/context validation remains runtime-owned.
type Request struct {
	ID, ModelID, Prompt string
	MaxOutputTokens     int
}

// State describes runtime readiness independently of mesh membership and capacity.
type State int

const (
	StateStarting State = iota
	StateReady
	StateUnhealthy
)

// Health includes activity through cancellation cleanup. LastError is diagnostic text.
type Health struct {
	State     State
	Active    bool
	LastError string
}

// Capabilities describes the loaded runtime; the MVP capacity is one.
type Capabilities struct {
	Model                   Model
	RuntimeVersion, Backend string
	Capacity                int
}

// EventKind identifies the ordered runtime stream payload.
type EventKind int

const (
	EventStarted EventKind = iota
	EventTextDelta
	EventCompleted
)

// Event carries incremental text or terminal usage. Nil token counts mean unavailable.
// The callback may retain the value and pointed-to counts; implementations must not mutate them afterwards.
type Event struct {
	Kind                      EventKind
	Text, FinishReason        string
	InputTokens, OutputTokens *int64
}

var (
	// ErrInvalidInput covers non-transient Start configuration/artifact failures and
	// invalid requests, including template/context overflow rejected before Started.
	ErrInvalidInput = errors.New("invalid runtime input")
	// ErrUnavailable means the runtime cannot currently serve inference.
	ErrUnavailable = errors.New("runtime unavailable")
	// ErrMalformedResponse means the runtime broke its streaming response contract.
	ErrMalformedResponse = errors.New("malformed runtime response")
)

// Runtime owns its process and cleanup. Start waits for verified readiness; Stop reaps
// owned resources. All blocking operations honour their context. Capabilities returns a snapshot.
// Generate emits Started only after template/input validation, then deltas and Completed.
// A callback error cancels generation and returns promptly after bounded cleanup.
// Generate returns only after generation/cleanup ends; failed cleanup leaves it unhealthy.
// Context cancellation/deadline errors retain their identity rather than becoming sentinels.
// Callers serialize Start/Stop and permit one Generate at a time. Implementations
// support concurrent Health/Capabilities calls during startup and generation.
// Generate invokes emit synchronously to preserve downstream backpressure.
// Implementations never retain the callback after Generate returns.
type Runtime interface {
	Start(context.Context, Config) error
	Stop(context.Context) error
	Health(context.Context) (Health, error)
	Capabilities() Capabilities
	Generate(context.Context, Request, func(Event) error) error
}
