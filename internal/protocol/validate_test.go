package protocol_test

import (
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Wire validation must reject bad input before either hop admits runtime work.
func TestValidateRequest(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if got := status.Code(protocol.ValidateRequest(nil, "demo")); got != codes.InvalidArgument {
			t.Fatalf("status = %v, want InvalidArgument", got)
		}
	})
	for _, tc := range []struct {
		name, prompt, id, model string
		limit                   int32
		code                    codes.Code
	}{
		{"valid", "hello", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 128, codes.OK},
		{"empty prompt", "", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 128, codes.InvalidArgument},
		{"invalid UTF8", string([]byte{0xff}), "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 128, codes.InvalidArgument},
		{"oversized prompt", strings.Repeat("a", 16385), "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 128, codes.InvalidArgument},
		{"exact UTF8 boundary", strings.Repeat("é", 8192), "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 512, codes.OK},
		{"invalid UUID", "hello", "not-a-uuid", "demo", 128, codes.InvalidArgument},
		{"invalid UUID hex", "hello", "z47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 128, codes.InvalidArgument},
		{"unknown model", "hello", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "other", 128, codes.NotFound},
		{"zero output", "hello", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 0, codes.InvalidArgument},
		{"too much output", "hello", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 513, codes.InvalidArgument},
		{"minimum output", "hello", "d47ebc8b-a613-47bb-a891-4135f9e8ac35", "demo", 1, codes.OK},
	} {
		req := &meshv1.InferenceRequest{RequestId: tc.id, ModelId: tc.model, Prompt: tc.prompt, MaxOutputTokens: tc.limit}
		t.Run(tc.name, func(t *testing.T) {
			if got := status.Code(protocol.ValidateRequest(req, "demo")); got != tc.code {
				t.Fatalf("status = %v, want %v", got, tc.code)
			}
		})
	}
}
