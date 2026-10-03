package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type membershipTiming struct {
	wait   func(context.Context, time.Duration) error
	jitter func(time.Duration) time.Duration
}

// RunMembership independently registers and heartbeats this process. It owns one
// reusable controller connection, closes it on address change/shutdown, and keeps
// retrying until ctx is canceled. Controller failure never changes runtime ownership.
func RunMembership(ctx context.Context, svc *Service, endpoint string, resolve func(context.Context) (string, error)) error {
	return runMembership(ctx, svc, endpoint, resolve, membershipTiming{wait: wait, jitter: func(d time.Duration) time.Duration { return membershipJitter(d, rand.Int64N) }})
}
func runMembership(ctx context.Context, svc *Service, endpoint string, resolve func(context.Context) (string, error), timing membershipTiming) error {
	// The adapter verifies its version during Start, independently of membership.
	// Refresh only changed metadata through idempotent registration, preserving
	// this process identity and any controller-owned reservation.
	runtimeVersion := func() string {
		version := svc.rt.Capabilities().RuntimeVersion
		if version == "" {
			return "unknown"
		}
		return version
	}
	var conn *grpc.ClientConn
	var address string
	defer func() {
		if conn != nil {
			if err := conn.Close(); err != nil {
				slog.Warn("close controller connection", "worker_id", svc.id, "error", err)
			}
		}
	}()
	delay := time.Second
	for ctx.Err() == nil {
		// Leave route validation time after the resolver's full three-second browse.
		lookup, cancel := context.WithTimeout(ctx, 4*time.Second)
		target, err := resolve(lookup)
		cancel()
		if err == nil {
			if host, _, splitErr := net.SplitHostPort(target); splitErr != nil || host == "" {
				err = fmt.Errorf("controller resolver returned invalid address %q", target)
			}
		}
		if err == nil && (conn == nil || target != address) {
			if conn != nil {
				if closeErr := conn.Close(); closeErr != nil {
					slog.Warn("close previous controller connection", "worker_id", svc.id, "error", closeErr)
				}
				conn = nil
			}
			conn, err = grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64*1024), grpc.MaxCallSendMsgSize(64*1024)))
			if err == nil {
				address = target
			}
		}
		if err == nil {
			client := meshv1.NewControllerServiceClient(conn)
			for ctx.Err() == nil {
				rpc, cancel := context.WithTimeout(ctx, 2*time.Second)
				version := runtimeVersion()
				response, registerErr := client.RegisterWorker(rpc, &meshv1.RegisterWorkerRequest{WorkerId: svc.id, Endpoint: endpoint, ProtocolMajor: protocol.Major, Hardware: svc.hardware, RuntimeVersion: version, Backend: svc.cfg.Backend, Model: &meshv1.ModelDescriptor{Id: svc.cfg.Model.ID, Sha256: svc.cfg.Model.SHA256, ContextTokens: uint32(svc.cfg.Model.ContextTokens)}, Capacity: 1, Report: svc.Report()})
				cancel()
				if registerErr != nil {
					err = registerErr
					break
				}
				if response == nil || uuid.Validate(response.ControllerId) != nil || response.HeartbeatIntervalSeconds != 2 || response.MembershipExpirySeconds != 10 {
					err = fmt.Errorf("controller returned incompatible identity or membership settings")
					break
				}
				delay = time.Second
				for ctx.Err() == nil {
					if timing.wait(ctx, 2*time.Second) != nil {
						return nil
					}
					if runtimeVersion() != version {
						err = nil
						break
					}
					rpc, cancel := context.WithTimeout(ctx, 2*time.Second)
					_, err = client.Heartbeat(rpc, &meshv1.HeartbeatRequest{WorkerId: svc.id, Report: svc.Report()})
					cancel()
					if err != nil {
						break
					}
				}
				if err == nil || status.Code(err) == codes.NotFound {
					continue
				}
				break
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("worker membership retry", "worker_id", svc.id, "controller", target, "error", err, "retry_limit", delay)
		if timing.wait(ctx, timing.jitter(delay)) != nil {
			return nil
		}
		delay *= 2
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
	}
	return nil
}

// Randomness is private and controllable so both retry bounds can be verified.
func membershipJitter(delay time.Duration, random func(int64) int64) time.Duration {
	ceiling := delay + delay/2
	if ceiling > 10*time.Second {
		ceiling = 10 * time.Second
	}
	return delay + time.Duration(random(int64(ceiling-delay)+1))
}
