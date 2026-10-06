// Package client shares the validated Controller transport between CLI modes.
package client

import (
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client owns one reusable Controller connection. The caller closes it after
// its calls end and supplies the deadlines for Generate and Status.
type Client struct {
	conn *grpc.ClientConn
	rpc  meshv1.ControllerServiceClient
}

// New creates a client for a resolved Controller address. It does not discover
// or probe the Controller; connection errors are reported by the first call.
func New(address string) (*Client, error) {
	conn, err := grpc.NewClient("passthrough:///"+address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes)))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, rpc: meshv1.NewControllerServiceClient(conn)}, nil
}

// Generate validates and delivers events synchronously, with no response queue.
// Event data is borrowed during emit; retain only copied data. The caller must
// not mutate req during the call. emit must return promptly or honor ctx.
// Completed is provisional until Generate returns nil after final gRPC OK.
// Consumer or validation failure cancels and finishes the local stream call;
// this does not prove that the remote runtime has finished its cleanup.
func (c *Client) Generate(ctx context.Context, req *meshv1.InferenceRequest, emit func(*meshv1.InferenceEvent) error) error {
	if err := protocol.ValidateRequest(req, req.GetModelId()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.RunInference(ctx, req)
	if err != nil {
		return err
	}
	finished := false
	defer func() {
		cancel()
		// Receive the terminal transport result after local failure. Cancellation
		// bounds this join and keeps the primary callback/validation error.
		for !finished {
			_, err := stream.Recv()
			finished = err != nil
		}
	}()
	started, completed := false, false
	for {
		event, err := stream.Recv()
		if err != nil {
			finished = true
			if err != io.EOF {
				return err
			}
			if !completed {
				return errors.New("stream ended without Completed")
			}
			return nil
		}
		if event == nil || completed {
			return errors.New("malformed inference event order")
		}
		switch p := event.Payload.(type) {
		case *meshv1.InferenceEvent_Started:
			s := p.Started
			if started || s == nil || s.RequestId != req.RequestId || s.ModelId != req.ModelId || uuid.Validate(s.WorkerId) != nil || s.WorkerHostname == "" || !utf8.ValidString(s.WorkerHostname) || len(s.WorkerHostname) > 1024 {
				return errors.New("malformed Started event")
			}
			started = true
		case *meshv1.InferenceEvent_TextDelta:
			if !started || p.TextDelta == nil || p.TextDelta.Text == "" || len(p.TextDelta.Text) > 4096 || !utf8.ValidString(p.TextDelta.Text) {
				return errors.New("malformed TextDelta event")
			}
		case *meshv1.InferenceEvent_Completed:
			v := p.Completed
			if !started || v == nil || (v.FinishReason != "stop" && v.FinishReason != "length") || (v.InputTokens != nil && *v.InputTokens < 0) || (v.OutputTokens != nil && *v.OutputTokens < 0) {
				return errors.New("malformed Completed event")
			}
			completed = true
		default:
			return errors.New("unknown inference event")
		}
		if err := emit(event); err != nil {
			return err
		}
	}
}

// Status returns a fresh Controller snapshot. It permits the larger status
// message bound without changing generation's bound or the caller's deadline.
func (c *Client) Status(ctx context.Context) (*meshv1.GetClusterStatusResponse, error) {
	response, err := c.rpc.GetClusterStatus(ctx, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
	if err != nil {
		return nil, err
	}
	if response == nil || uuid.Validate(response.ControllerId) != nil {
		return nil, errors.New("malformed controller status identity")
	}
	return response, nil
}

// Close releases the connection. It does not shut down the remote Controller.
func (c *Client) Close() error { return c.conn.Close() }
