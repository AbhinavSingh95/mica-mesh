// Package app composes foreground mesh services and owns their resources.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/controller"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
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

// discoveryEffects is the private boundary for multicast and interface I/O.
type discoveryEffects struct {
	resolve   func(context.Context, string) (string, error)
	advertise func(context.Context, discovery.ControllerInfo) (func(), error)
	localIPv4 func(string, net.IP) (string, error)
}

// run takes ownership of supplied active-role listeners and the runtime.
func run(ctx context.Context, cfg config.Config, roles config.Role, rt runtime.Runtime, cl, wl net.Listener) error {
	return runWithDiscovery(ctx, cfg, roles, rt, cl, wl, discoveryEffects{discovery.Resolve, discovery.Advertise, discovery.LocalIPv4})
}
func runWithDiscovery(ctx context.Context, cfg config.Config, roles config.Role, rt runtime.Runtime, cl, wl net.Listener, discover discoveryEffects) (err error) {
	if roles&config.RoleWorker != 0 && cfg.AdvertiseAddress != "" {
		if err := discovery.ValidateLocalIPv4(cfg.AdvertiseAddress, wl.Addr().(*net.TCPAddr).IP, roles&config.RoleController != 0); err != nil {
			err = errors.Join(err, wl.Close())
			if cl != nil {
				err = errors.Join(err, cl.Close())
			}
			return err
		}
	}
	var advertisement discovery.ControllerInfo
	if roles&config.RoleController != 0 {
		address := cl.Addr().(*net.TCPAddr)
		ip, selectErr := discover.localIPv4(cfg.AdvertiseAddress, address.IP)
		hostname, hostErr := os.Hostname()
		if err = errors.Join(selectErr, hostErr); err != nil {
			err = errors.Join(err, cl.Close())
			if wl != nil {
				err = errors.Join(err, wl.Close())
			}
			return err
		}
		advertisement = discovery.ControllerInfo{InstanceID: uuid.NewString(), Hostname: hostname, IPv4: ip, Port: address.Port, ProtocolMajor: protocol.Major}
	}

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
		id := advertisement.InstanceID
		controllerSvc = controller.New(id, controller.NewRegistry(cfg.ModelDescriptor))
		g := server()
		meshv1.RegisterControllerServiceServer(g, controllerSvc)
		launch(func() error { return g.Serve(cl) })
		launch(func() error { return controllerSvc.Run(ctx) })
		target = controllerEndpoint(cl.Addr())
		slog.Info("controller listening", "controller_id", id, "controller_address", cl.Addr().String())
		stop, advertiseErr := discover.advertise(ctx, advertisement)
		if advertiseErr != nil {
			slog.Warn("controller discovery unavailable; use --controller-address", "controller_address", net.JoinHostPort(advertisement.IPv4, fmt.Sprint(advertisement.Port)), "error", advertiseErr)
		} else {
			defer stop()
		}

	}
	if roles&config.RoleWorker != 0 {
		rc := runtime.Config{BinaryPath: cfg.RuntimeBinary, ModelPath: cfg.ModelPath, Backend: cfg.Backend, Port: cfg.RuntimePort, Model: cfg.ModelDescriptor}
		id := uuid.NewString()
		svc := worker.New(id, rc, hardware.Profile(ctx), rt)
		g := server()
		meshv1.RegisterWorkerServiceServer(g, svc)
		launch(func() error { return g.Serve(wl) })
		launch(func() error { return svc.RunRuntime(ctx) })
		launch(func() error {
			return joinWorker(ctx, svc, cfg.AdvertiseAddress, target, wl.Addr(), roles&config.RoleController != 0, discover.resolve)
		})
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
func joinWorker(ctx context.Context, svc *worker.Service, override, explicit string, listen net.Addr, local bool, resolve func(context.Context, string) (string, error)) error {
	delay := time.Second
	for ctx.Err() == nil {
		lookup, cancel := context.WithTimeout(ctx, 4*time.Second)
		target, err := resolve(lookup, explicit)
		endpoint := ""
		if err == nil {
			endpoint, err = workerEndpointFor(lookup, override, target, listen, local)
		}
		cancel()
		if err == nil {
			first := true
			return worker.RunMembership(ctx, svc, endpoint, func(ctx context.Context) (string, error) {
				if first {
					first = false
					return target, nil
				}
				next, err := resolve(ctx, explicit)
				if err != nil {
					return "", err
				}
				nextEndpoint, err := workerEndpointFor(ctx, override, next, listen, local)
				if err != nil {
					return "", err
				}
				if nextEndpoint != endpoint {
					return "", errors.New("worker route changed; restart with --advertise-address matching the new route")
				}
				return next, nil
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
func workerEndpointFor(ctx context.Context, override, target string, listen net.Addr, local bool) (string, error) {
	bound := listen.(*net.TCPAddr)
	ip := override
	if ip == "" {
		// A UDP route lookup sends no application traffic. Its ephemeral port is
		// never advertised; the actual listener's port is authoritative.
		conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", target)
		if err != nil {
			return "", fmt.Errorf("select worker route: %w", err)
		}
		ip = conn.LocalAddr().(*net.UDPAddr).IP.String()
		if err := conn.Close(); err != nil {
			return "", err
		}
	}
	if err := discovery.ValidateLocalIPv4(ip, bound.IP, local); err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, fmt.Sprint(bound.Port)), nil
}

func controllerEndpoint(address net.Addr) string {
	host, port, _ := net.SplitHostPort(address.String())
	if net.ParseIP(host).IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
