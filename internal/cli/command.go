package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/discovery"
	"github.com/AbhinavSingh95/mica-mesh/internal/protocol"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type command struct {
	name, prompt string
	cfg          config.Config
	roles        config.Role
}

func parse(args []string) (command, error) {
	c := command{name: args[0]}
	if c.name != "start" && c.name != "status" && c.name != "run" {
		return c, fmt.Errorf("unknown command %q; use --help for usage", c.name)
	}
	f := flag.NewFlagSet(c.name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	values := config.Default()
	var path string
	var controllerRole, workerRole bool
	f.StringVar(&path, "config", "", "configuration file")
	f.StringVar(&values.ControllerAddress, "controller-address", values.ControllerAddress, "controller HOST:PORT")
	f.StringVar(&values.ControllerListen, "controller-listen", values.ControllerListen, "controller bind address")
	f.StringVar(&values.WorkerListen, "worker-listen", values.WorkerListen, "worker bind address")
	f.StringVar(&values.AdvertiseAddress, "advertise-address", values.AdvertiseAddress, "local IPv4 address")
	f.StringVar(&values.RuntimeBinary, "runtime-binary", values.RuntimeBinary, "absolute llama-server path")
	f.StringVar(&values.ModelPath, "model-path", values.ModelPath, "absolute GGUF path")
	f.StringVar(&values.Backend, "backend", values.Backend, "cpu or metal")
	f.StringVar(&values.Model, "model", values.Model, "model ID")
	f.IntVar(&values.RuntimePort, "runtime-port", values.RuntimePort, "loopback runtime port")
	f.IntVar(&values.MaxOutputTokens, "max-output-tokens", values.MaxOutputTokens, "output limit")
	f.DurationVar(&values.Timeout, "timeout", values.Timeout, "total request budget")
	if c.name == "start" {
		f.BoolVar(&controllerRole, "controller", false, "controller role")
		f.BoolVar(&workerRole, "worker", false, "worker role")
	}
	if err := f.Parse(args[1:]); err != nil {
		return c, err
	}
	if c.name == "run" {
		if f.NArg() != 1 {
			return c, errors.New("run requires exactly one quoted prompt")
		}
		c.prompt = f.Arg(0)
	} else if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	if controllerRole {
		c.roles |= config.RoleController
	}
	if workerRole {
		c.roles |= config.RoleWorker
	}
	if c.name == "start" && c.roles == 0 {
		return c, errors.New("start requires --controller or --worker")
	}
	required := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "config" {
			required = true
		}
	})
	if !required {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, fmt.Errorf("find configuration home: %w", err)
		}
		path = filepath.Join(home, ".config", "mica-mesh", "config.json")
	}
	cfg, err := config.Load(path, required)
	if err != nil {
		return c, err
	}
	f.Visit(func(v *flag.Flag) {
		switch v.Name {
		case "controller-address":
			cfg.ControllerAddress = values.ControllerAddress
		case "controller-listen":
			cfg.ControllerListen = values.ControllerListen
		case "worker-listen":
			cfg.WorkerListen = values.WorkerListen
		case "advertise-address":
			cfg.AdvertiseAddress = values.AdvertiseAddress
		case "runtime-binary":
			cfg.RuntimeBinary = values.RuntimeBinary
		case "model-path":
			cfg.ModelPath = values.ModelPath
		case "backend":
			cfg.Backend = values.Backend
		case "model":
			cfg.Model = values.Model
		case "runtime-port":
			cfg.RuntimePort = values.RuntimePort
		case "max-output-tokens":
			cfg.MaxOutputTokens = values.MaxOutputTokens
		case "timeout":
			cfg.Timeout = values.Timeout
		}
	})
	c.cfg = cfg
	return c, config.Validate(cfg, c.roles)
}

func resolveController(address string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) { return discovery.Resolve(ctx, address) }
}
func connect(address string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(protocol.GenerationMessageBytes), grpc.MaxCallSendMsgSize(protocol.GenerationMessageBytes)))
}

func runInference(ctx context.Context, cfg config.Config, prompt string, stdout, stderr io.Writer, resolve func(context.Context) (string, error)) (err error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	release, err := bindOutput(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	begin := time.Now()
	firstText := "unknown"
	defer func() {
		cancel()
		release()
		if err != nil {
			finalDiagnostic(ctx, stderr, "request_id=%s time_to_first_text=%s duration=%s error=%v\n", id, firstText, time.Since(begin), err)
		}
	}()
	req := &meshv1.InferenceRequest{RequestId: id, ModelId: cfg.Model, Prompt: prompt, MaxOutputTokens: int32(cfg.MaxOutputTokens)}
	if err := protocol.ValidateRequest(req, cfg.Model); err != nil {
		return err
	}
	target, err := resolve(ctx)
	if err != nil {
		return err
	}
	conn, err := connect(target)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	stream, err := meshv1.NewControllerServiceClient(conn).RunInference(ctx, req)
	if err != nil {
		return err
	}
	started, terminal := false, false
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			if !terminal {
				return errors.New("stream ended without Completed")
			}
			return nil
		}
		if recvErr != nil {
			return recvErr
		}
		if event == nil || terminal {
			return errors.New("malformed inference event order")
		}
		switch p := event.Payload.(type) {
		case *meshv1.InferenceEvent_Started:
			s := p.Started
			if started || s == nil || s.RequestId != id || s.ModelId != cfg.Model || uuid.Validate(s.WorkerId) != nil || s.WorkerHostname == "" || !utf8.ValidString(s.WorkerHostname) || len(s.WorkerHostname) > 1024 {
				return errors.New("malformed Started event")
			}
			started = true
			if _, err := fmt.Fprintf(stderr, "request_id=%s worker_id=%s worker_hostname=%q\n", id, s.WorkerId, s.WorkerHostname); err != nil {
				return fmt.Errorf("write diagnostics: %w", err)
			}
		case *meshv1.InferenceEvent_TextDelta:
			if !started || p.TextDelta == nil || p.TextDelta.Text == "" || len(p.TextDelta.Text) > 4096 || !utf8.ValidString(p.TextDelta.Text) {
				return errors.New("malformed TextDelta event")
			}
			text := p.TextDelta.Text
			n, err := io.WriteString(stdout, text)
			if err != nil {
				return fmt.Errorf("write generated text: %w", err)
			}
			if n != len(text) {
				return io.ErrShortWrite
			}
			if firstText == "unknown" {
				firstText = time.Since(begin).String()
			}
		case *meshv1.InferenceEvent_Completed:
			c := p.Completed
			if !started || c == nil || (c.FinishReason != "stop" && c.FinishReason != "length") || (c.InputTokens != nil && *c.InputTokens < 0) || (c.OutputTokens != nil && *c.OutputTokens < 0) {
				return errors.New("malformed Completed event")
			}
			terminal = true
			if _, err := fmt.Fprintf(stderr, "request_id=%s time_to_first_text=%s duration=%s finish_reason=%s\n", id, firstText, time.Since(begin), c.FinishReason); err != nil {
				return fmt.Errorf("write completion diagnostics: %w", err)
			}
		default:
			return errors.New("unknown inference event")
		}
	}
}

// bindOutput gives each invocation exclusive write/deadline ownership of supplied
// files. Caller writers must return promptly or cooperate with cancellation.
// Regular filesystem writes remain synchronous and cannot be hard-interrupted.
func bindOutput(ctx context.Context, writers ...io.Writer) (func(), error) {
	var files []*os.File
	for _, w := range writers {
		file, ok := w.(*os.File)
		if !ok {
			continue
		}
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() || isNullFile(info) {
			continue
		}
		if err := file.SetWriteDeadline(time.Time{}); err != nil {
			return nil, fmt.Errorf("output does not support cancellation: %w", err)
		}
		files = append(files, file)
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		for _, file := range files {
			_ = file.SetWriteDeadline(time.Now())
		}
	})
	return func() {
		if !stop() {
			<-done
		}
		for _, file := range files {
			_ = file.SetWriteDeadline(time.Time{})
		}
	}, nil
}

func clusterStatus(ctx context.Context, address string, stdout io.Writer) (err error) {
	return clusterStatusWithResolver(ctx, stdout, resolveController(address))
}
func clusterStatusWithResolver(ctx context.Context, stdout io.Writer, resolve func(context.Context) (string, error)) (err error) {
	target, err := resolve(ctx)
	if err != nil {
		return err
	}
	conn, err := connect(target)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	rpc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := meshv1.NewControllerServiceClient(conn).GetClusterStatus(rpc, &meshv1.GetClusterStatusRequest{}, grpc.MaxCallRecvMsgSize(protocol.StatusMessageBytes))
	if err != nil {
		return err
	}
	if response == nil || uuid.Validate(response.ControllerId) != nil {
		return errors.New("malformed controller status identity")
	}
	if _, err := fmt.Fprintf(stdout, "controller_id=%s\n", response.ControllerId); err != nil {
		return err
	}
	for _, row := range response.Workers {
		state := row.GetState()
		switch state {
		case meshv1.WorkerState_WORKER_STATE_STARTING, meshv1.WorkerState_WORKER_STATE_READY, meshv1.WorkerState_WORKER_STATE_BUSY, meshv1.WorkerState_WORKER_STATE_UNHEALTHY, meshv1.WorkerState_WORKER_STATE_UNAVAILABLE:
		default:
			state = meshv1.WorkerState_WORKER_STATE_UNAVAILABLE
		}
		if _, err := fmt.Fprintf(stdout, "worker_id=%q %s endpoint=%q state=%s runtime_state=%s active=%t active_request_id=%q reserved_request_id=%q capacity=%d model_id=%q backend=%q runtime_version=%q heartbeat_age_ms=%d error=%q\n", row.GetWorkerId(), hardwareStatus(row.GetHardware()), row.GetEndpoint(), state, row.GetReport().GetRuntimeState(), row.GetReport().GetActive(), row.GetReport().GetActiveRequestId(), row.GetReservedRequestId(), row.GetCapacity(), row.GetModel().GetId(), row.GetBackend(), row.GetRuntimeVersion(), row.GetHeartbeatAgeMilliseconds(), row.GetLastError()); err != nil {
			return err
		}
	}
	return nil
}

func isNullFile(info os.FileInfo) bool {
	if info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat("/dev/null")
	return err == nil && os.SameFile(info, null)
}

const finalDiagnosticBudget = 250 * time.Millisecond

// finalDiagnostic runs only after work cancellation and the original output
// callback have been joined. This detached cleanup writes once under a strict
// bound; a full/broken diagnostic destination never replaces the primary error.
// As with Main, regular filesystem writes and arbitrary writers must cooperate.
func finalDiagnostic(ctx context.Context, stderr io.Writer, format string, args ...any) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalDiagnosticBudget)
	defer cancel()
	release, err := bindOutput(cleanup, stderr)
	if err != nil {
		return
	}
	defer release()
	_, _ = fmt.Fprintf(stderr, format, args...)
}

func hardwareStatus(h *meshv1.HardwareInfo) string {
	if h == nil {
		h = &meshv1.HardwareInfo{}
	}
	cpu, gpu, cores, ram, vram, unified := "unknown", "unknown", "unknown", "unknown", "unknown", "unknown"
	if h.Cpu != nil {
		cpu = *h.Cpu
	}
	if h.Gpu != nil {
		gpu = *h.Gpu
	}
	if h.CpuCores != nil {
		cores = strconv.FormatUint(uint64(*h.CpuCores), 10)
	}
	if h.RamBytes != nil {
		ram = strconv.FormatUint(*h.RamBytes, 10)
	}
	if h.GpuMemoryBytes != nil {
		vram = strconv.FormatUint(*h.GpuMemoryBytes, 10)
	}
	if h.UnifiedMemory != nil {
		unified = strconv.FormatBool(*h.UnifiedMemory)
	}
	return fmt.Sprintf("hostname=%q architecture=%q cpu=%q cpu_cores=%s ram_bytes=%s gpu=%q dedicated_gpu_memory_bytes=%s unified_memory=%s", h.Hostname, h.Architecture, cpu, cores, ram, gpu, vram, unified)
}
