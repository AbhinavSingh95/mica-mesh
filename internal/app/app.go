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
	p, err := Start(ctx, cfg, roles, Options{Network: LAN})
	if err != nil {
		return err
	}
	return p.Wait()
}

// Start establishes listener ownership. Runtime readiness is asynchronous.
func Start(ctx context.Context, cfg config.Config, roles config.Role, options Options) (*Process, error) {
	if roles == 0 || roles&^(config.RoleController|config.RoleWorker) != 0 {
		return nil, errors.New("invalid process roles")
	}
	var err error
	switch options.Network {
	case LAN:
		err = config.Validate(cfg, roles)
	case Local:
		err = config.ValidateLocal(cfg, roles)
	default:
		return nil, errors.New("invalid network mode")
	}
	if err != nil {
		return nil, err
	}
	var cl, wl net.Listener
	if roles&config.RoleController != 0 {
		cl, err = net.Listen("tcp4", cfg.ControllerListen)
		if err != nil {
			return nil, fmt.Errorf("listen Controller: %w; choose another --controller-listen port", err)
		}
	}
	if roles&config.RoleWorker != 0 {
		wl, err = net.Listen("tcp4", cfg.WorkerListen)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("listen Agent: %w; choose another --worker-listen port", err), closeListeners(cl))
		}
	}
	return startWithDiscovery(ctx, cfg, roles, llamacpp.New(), cl, wl, options, discoveryEffects{discovery.Resolve, discovery.Advertise, discovery.LocalIPv4})
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
func runWithDiscovery(ctx context.Context, cfg config.Config, roles config.Role, rt runtime.Runtime, cl, wl net.Listener, discover discoveryEffects) error {
	p, err := startWithDiscovery(ctx, cfg, roles, rt, cl, wl, Options{Network: LAN}, discover)
	if err != nil {
		return err
	}
	return p.Wait()
}

// startWithDiscovery takes ownership even when synchronous preparation fails.
func startWithDiscovery(ctx context.Context, cfg config.Config, roles config.Role, rt runtime.Runtime, cl, wl net.Listener, options Options, discover discoveryEffects) (p *Process, err error) {
	if cl != nil {
		cl = &ownedListener{Listener: cl}
	}
	if wl != nil {
		wl = &ownedListener{Listener: wl}
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, closeListeners(cl, wl))
		}
	}()
	if options.Network == Local {
		if err := config.ValidateLocal(cfg, roles); err != nil {
			return nil, err
		}
	} else if roles&config.RoleWorker != 0 && cfg.AdvertiseAddress != "" {
		if err := discovery.ValidateLocalIPv4(cfg.AdvertiseAddress, wl.Addr().(*net.TCPAddr).IP, roles&config.RoleController != 0); err != nil {
			return nil, err
		}
	}
	var advertisement discovery.ControllerInfo
	controllerID := ""
	if roles&config.RoleController != 0 {
		controllerID = uuid.NewString()
		if options.Network == LAN {
			address := cl.Addr().(*net.TCPAddr)
			ip, selectErr := discover.localIPv4(cfg.AdvertiseAddress, address.IP)
			hostname, hostErr := os.Hostname()
			if err := errors.Join(selectErr, hostErr); err != nil {
				return nil, err
			}
			advertisement = discovery.ControllerInfo{InstanceID: controllerID, Hostname: hostname, IPv4: ip, Port: address.Port, ProtocolMajor: protocol.Major}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	p = &Process{cancel: cancel, done: make(chan struct{})}
	results := make(chan error, 5)
	owners := 0
	launch := func(fn func() error) { owners++; go func() { results <- fn() }() }
	var servers []*grpc.Server
	server := func() *grpc.Server {
		g := grpc.NewServer(protocol.GenerationServerOption(), grpc.MaxRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxSendMsgSize(protocol.StatusMessageBytes))
		servers = append(servers, g)
		return g
	}
	var controllerSvc *controller.Service
	var stopAdvertisement func()
	target := cfg.ControllerAddress
	if roles&config.RoleController != 0 {
		controllerSvc = controller.New(controllerID, controller.NewRegistry(cfg.ModelDescriptor))
		g := server()
		meshv1.RegisterControllerServiceServer(g, controllerSvc)
		launch(func() error { return serve(ctx, g, cl) })
		launch(func() error { return controllerSvc.Run(ctx) })
		p.controllerAddress = controllerEndpoint(cl.Addr())
		target = p.controllerAddress
		slog.Info("controller listening", "controller_id", controllerID, "controller_address", cl.Addr().String())
		if options.Network == LAN {
			var advertiseErr error
			stopAdvertisement, advertiseErr = discover.advertise(ctx, advertisement)
			if advertiseErr != nil {
				slog.Warn("controller discovery unavailable; use --controller-address", "controller_address", net.JoinHostPort(advertisement.IPv4, fmt.Sprint(advertisement.Port)), "error", advertiseErr)
			}
		}
	}
	if roles&config.RoleWorker != 0 {
		rc := runtime.Config{BinaryPath: cfg.RuntimeBinary, ModelPath: cfg.ModelPath, Backend: cfg.Backend, Port: cfg.RuntimePort, Model: cfg.ModelDescriptor}
		id := uuid.NewString()
		svc := worker.New(id, rc, hardware.Profile(ctx), rt)
		p.worker = svc
		p.workerID = id
		p.endpoint = wl.Addr().String()
		g := server()
		meshv1.RegisterWorkerServiceServer(g, svc)
		launch(func() error { return serve(ctx, g, wl) })
		launch(func() error { return svc.RunRuntime(ctx) })
		resolve := discover.resolve
		if options.ResolveController != nil {
			resolve = options.ResolveController
		}
		launch(func() error {
			if options.Network == Local {
				return worker.RunMembership(ctx, svc, wl.Addr().String(), func(context.Context) (string, error) { return target, nil }, p.observeMembership)
			}
			return joinWorker(ctx, svc, cfg.AdvertiseAddress, target, wl.Addr(), roles&config.RoleController != 0, resolve, p.observeMembership, p.setEndpoint)
		})
		slog.Info("Agent listening", "worker_id", id, "worker_address", wl.Addr().String())
	}
	go p.finish(ctx, results, owners, servers, []net.Listener{cl, wl}, controllerSvc, stopAdvertisement)
	return p, nil
}
func joinWorker(ctx context.Context, svc *worker.Service, override, explicit string, listen net.Addr, local bool, resolve func(context.Context, string) (string, error), observe func(worker.MembershipStatus), endpointReady func(string)) error {
	delay := time.Second
	for ctx.Err() == nil {
		lookup, cancel := context.WithTimeout(ctx, 4*time.Second)
		target, err := resolve(lookup, explicit)
		endpoint := ""
		if err == nil {
			endpoint, err = workerEndpointFor(lookup, override, target, listen, local)
		}
		cancel()
		if target != "" {
			observe(worker.MembershipStatus{ControllerAddress: target})
		}
		if err == nil {
			endpointReady(endpoint)
			first := true
			return worker.RunMembership(ctx, svc, endpoint, func(ctx context.Context) (string, error) {
				if first {
					first = false
					return target, nil
				}
				next, err := resolve(ctx, explicit)
				if err != nil {
					return next, err
				}
				nextEndpoint, err := workerEndpointFor(ctx, override, next, listen, local)
				if err != nil {
					return next, err
				}
				if nextEndpoint != endpoint {
					return next, errors.New("worker route changed; restart with --advertise-address matching the new route")
				}
				return next, nil
			}, observe)
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
