// Package app composes foreground mesh services and owns their resources.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/hardware"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime"
	"github.com/AbhinavSingh95/mica-mesh/internal/runtime/llamacpp"
	"github.com/AbhinavSingh95/mica-mesh/internal/worker"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

// Run binds configured listeners and owns services, connections and runtime until
// shutdown completes. Combined mode registers over its local controller's RPCs.
func Run(ctx context.Context, cfg config.Config, roles config.Role) error {
	if roles == 0 || roles&^(config.RoleController|config.RoleWorker) != 0 {
		return errors.New("invalid process roles")
	}
	if err := config.Validate(cfg, roles); err != nil {
		return err
	}
	var cl, wl net.Listener
	var err error
	if roles&config.RoleController != 0 {
		cl, err = net.Listen("tcp4", cfg.ControllerListen)
		if err != nil {
			return fmt.Errorf("listen controller: %w", err)
		}
	}
	if roles&config.RoleWorker != 0 {
		wl, err = net.Listen("tcp4", cfg.WorkerListen)
		if err != nil {
			if cl != nil {
				err = errors.Join(err, cl.Close())
			}
			return fmt.Errorf("listen worker: %w", err)
		}
	}
	return run(ctx, cfg, roles, llamacpp.New(), cl, wl)
}

// run takes ownership of supplied active-role listeners and the runtime. Tests
// supply the designed fake; there is no user-facing fake mode.
func run(ctx context.Context, cfg config.Config, roles config.Role, rt runtime.Runtime, cl, wl net.Listener) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 5)
	owners := 0
	launch := func(fn func() error) { owners++; go func() { results <- fn() }() }
	var servers []*grpc.Server
	var controllerSvc *controller.Service
	server := func() *grpc.Server {
		g := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
		servers = append(servers, g)
		return g
	}
	target := cfg.ControllerAddress
	if roles&config.RoleController != 0 {
		id := uuid.NewString()
		controllerSvc = controller.New(id, controller.NewRegistry(cfg.ModelDescriptor))
		g := server()
		meshv1.RegisterControllerServiceServer(g, controllerSvc)
		launch(func() error { return g.Serve(cl) })
		launch(func() error { return controllerSvc.Run(ctx) })
		target = controllerEndpoint(cl.Addr())
		slog.Info("controller listening", "controller_id", id, "controller_address", cl.Addr().String())
	}
	if roles&config.RoleWorker != 0 {
		rc := runtime.Config{BinaryPath: cfg.RuntimeBinary, ModelPath: cfg.ModelPath, Backend: cfg.Backend, Port: cfg.RuntimePort, Model: cfg.ModelDescriptor}
		id := uuid.NewString()
		svc := worker.New(id, rc, hardware.Profile(ctx), rt)
		g := server()
		meshv1.RegisterWorkerServiceServer(g, svc)
		launch(func() error { return g.Serve(wl) })
		launch(func() error { return svc.RunRuntime(ctx) })
		launch(func() error { return joinWorker(ctx, svc, cfg.AdvertiseAddress, target, wl.Addr()) })
		slog.Info("worker listening", "worker_id", id, "worker_address", wl.Addr().String())
	}
	select {
	case <-ctx.Done():
	case failure := <-results:
		owners--
		err = failure
		if ctx.Err() != nil && errors.Is(err, grpc.ErrServerStopped) {
			err = nil
		}
		if err == nil && ctx.Err() == nil {
			err = errors.New("mesh owner stopped unexpectedly")
		}
	}
	cancel()
	// Stop transports first to unblock handlers/flow-control; the runtime owner
	// then confirms cleanup/reaps the child. Every launched owner is joined below.
	for _, g := range servers {
		g.Stop()
	}
	for owners > 0 {
		failure := <-results
		owners--
		if !errors.Is(failure, grpc.ErrServerStopped) {
			err = errors.Join(err, failure)
		}
	}
	if controllerSvc != nil {
		err = errors.Join(err, controllerSvc.Close())
	}
	return err
}
func joinWorker(ctx context.Context, svc *worker.Service, override, target string, listen net.Addr) error {
	delay := time.Second
	for ctx.Err() == nil {
		lookup, cancel := context.WithTimeout(ctx, 3*time.Second)
		endpoint, err := workerEndpoint(lookup, override, target, listen)
		cancel()
		if err == nil {
			return worker.RunMembership(ctx, svc, endpoint, func(ctx context.Context) (string, error) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				return target, nil
			})
		}
		slog.Warn("prepare worker membership", "controller", target, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay < 10*time.Second {
			delay *= 2
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
		}
	}
	return nil
}
func workerEndpoint(ctx context.Context, override, target string, listen net.Addr) (string, error) {
	if target == "" {
		return "", errors.New("controller discovery is pending; provide --controller-address HOST:PORT")
	}
	_, port, err := net.SplitHostPort(listen.String())
	if err != nil {
		return "", err
	}
	if override != "" {
		return net.JoinHostPort(override, port), nil
	}
	// A UDP route probe selects the local source IP without sending application
	// traffic. Its ephemeral source port is never the advertised service port.
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", target)
	if err != nil {
		return "", fmt.Errorf("select worker route: %w", err)
	}
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	err = conn.Close()
	if err != nil {
		return "", err
	}
	if ip.To4() == nil || ip.IsUnspecified() {
		return "", errors.New("route has no concrete IPv4 source")
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func controllerEndpoint(address net.Addr) string {
	host, port, _ := net.SplitHostPort(address.String())
	if net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
