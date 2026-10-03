// Package protocol validates shared mesh wire contracts at controller and worker boundaries.
package protocol

import (
	"unicode/utf8"

	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Major is the only supported protocol major, advertised and checked during registration.
const Major uint32 = 1

// ValidateRequest checks the untemplated wire request. The configured model is
// caller-owned; unsupported IDs return NotFound, malformed input InvalidArgument.
// Runtime template/token validation happens after worker admission.
func ValidateRequest(req *meshv1.InferenceRequest, modelID string) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is required")
	}
	if err := uuid.Validate(req.RequestId); err != nil {
		return status.Error(codes.InvalidArgument, "request_id must be a UUID")
	}
	if req.Prompt == "" || !utf8.ValidString(req.Prompt) || len(req.Prompt) > 16*1024 {
		return status.Error(codes.InvalidArgument, "prompt must be non-empty UTF-8 text of at most 16 KiB")
	}
	if req.MaxOutputTokens < 1 || req.MaxOutputTokens > 512 {
		return status.Error(codes.InvalidArgument, "max_output_tokens must be between 1 and 512")
	}
	if req.ModelId != modelID {
		return status.Error(codes.NotFound, "model is not supported")
	}
	return nil
}
