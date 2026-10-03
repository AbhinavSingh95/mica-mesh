package protocol

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/tap"
)

type generationCancelKey struct{}

// GenerationServerOption installs the generation lifetime BEFORE gRPC captures
// the stream's cancellation channel for flow-control waits. Handlers must use
// the stream context directly and defer GenerationCancel's cancel function.
// This uses the public tap API of the pinned grpc-go version; an interceptor or
// handler-derived context cannot unblock a flow-control-stalled Send.
func GenerationServerOption() grpc.ServerOption { return generationServerOption(300 * time.Second) }

func generationServerOption(maxLifetime time.Duration) grpc.ServerOption {
	return grpc.InTapHandle(func(ctx context.Context, info *tap.Info) (context.Context, error) {
		switch info.FullMethodName {
		case "/mica.mesh.v1.WorkerService/Generate", "/mica.mesh.v1.ControllerService/RunInference":
			capped, cancel := context.WithTimeout(ctx, maxLifetime)
			return context.WithValue(capped, generationCancelKey{}, cancel), nil
		default:
			return ctx, nil
		}
	})
}

// GenerationCancel returns the transport's cancellation function. False means
// GenerationServerOption was not installed; generation must fail before admission.
func GenerationCancel(ctx context.Context) (context.CancelFunc, bool) {
	cancel, ok := ctx.Value(generationCancelKey{}).(context.CancelFunc)
	return cancel, ok
}
