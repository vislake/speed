package app

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/vislake/speed/go/pkgcore"
)

// Option configures the application New assembles and Run assembles and
// serves. Every option is a plain setter; the host composes the option list
// its own policy resolves and hands it over unchanged. WithConfig and
// WithDatabase are required -- the engine ships no implicit defaults, so a
// missing one is a named startup error rather than a silently substituted
// value.
//
// The option set is the pre-component surface: New and Run map it onto the
// component assembly (the engine loader's load spec, the transition
// components and the composition configuration's code-override layer), and
// the set retires together with the transition adapters once the hosts
// assemble through the new machinery themselves.
type Option func(*engineConfig)

// engineConfig is the option set one New or Run call assembles from. It is
// built once, before any stage runs, and read-only afterwards.
type engineConfig struct {
	configSpec    *ConfigSpec
	configOptions []ConfigOption

	databaseSpec *DatabaseSpec
	preDB        func(ctx context.Context, deps PreDBDeps) error
	modules      func(ctx context.Context, deps ModuleDeps) ([]pkgcore.Module, error)

	kernelOptions []pkgcore.KernelOption
	observability *ObservabilitySpec
	httpSpec      *HTTPSpec
	hooks         Hooks
	worker        Worker

	// withoutWorkers is WithoutBackgroundWorkers' flag: the transition worker
	// component's Start is skipped. A registered worker is still closed by
	// the shutdown, mirroring the worker contract jobs.StandaloneQueue's
	// Close documents (safe to call before Start).
	withoutWorkers bool
}

// New assembles an application: it resolves the host's bootstrap
// configuration, opens the database and applies every module's migrations,
// constructs and declares the module set through the component assembly's
// seats, runs the host's attach and wiring hooks, composes the HTTP handler,
// and starts the background worker. It does not listen; a caller that wants
// the engine to serve and drain calls Run instead.
//
// The assembly runs as one drive of the component lifecycle over the
// transition components the option set maps onto (legacy.go documents the
// mapping); a failure at any point tears the built resources down -- the
// registry's own rollback plus the prelude's release -- before the error is
// returned.
//
// ctx is the assembly context: it bounds the database open, the migrations
// and the hooks. It is also the default request base context of the composed
// server, so it must not be cancelled while requests are being served (Run
// overrides it with the context Run was called with).
func New(ctx context.Context, opts ...Option) (*Application, error) {
	cfg, err := resolveOptions(opts)
	if err != nil {
		return nil, err
	}
	return assembleLegacy(ctx, cfg)
}

// Run assembles an application with New and serves it until ctx is done or
// the listener fails, then drains it through the component lifecycle's
// two-phase shutdown. Signal handling is the platform's: Run overlays SIGINT
// and SIGTERM on ctx, so a caller passes the base context (the
// logger-carrying one) and never builds a signal context of its own.
//
// When the options carry an ObservabilitySpec, the assembly selects the
// engine's observability component with it: the component initializes
// observability during the assembly's Prepare stage and shuts it down and
// flushes as the last close step. A host that manages observability itself
// omits the option and the component is deselected.
//
// Serving requires WithHTTP with a non-empty Addr; assembly succeeds without
// one, and Run refuses to listen on an empty address rather than binding a
// random port.
func Run(ctx context.Context, opts ...Option) error {
	cfg, err := resolveOptions(opts)
	if err != nil {
		return err
	}
	if cfg.httpSpec == nil || cfg.httpSpec.Addr == "" {
		return errors.New("app: Run requires WithHTTP with a non-empty Addr: the engine refuses to bind a random port")
	}

	baseCtx := ctx
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := assembleLegacy(ctx, cfg)
	if err != nil {
		return err
	}
	a.baseCtx = baseCtx
	return a.serve(ctx)
}

// resolveOptions applies every option and refuses a set that cannot assemble
// an application.
func resolveOptions(opts []Option) (*engineConfig, error) {
	cfg := &engineConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if cfg.configSpec == nil {
		return nil, errors.New("app: WithConfig is required: the engine resolves the host's bootstrap configuration (and the platform key material) before anything else is wired")
	}
	if cfg.databaseSpec == nil {
		return nil, errors.New("app: WithDatabase is required: the engine opens the database and applies every module's migrations before the module set is constructed")
	}
	return cfg, nil
}

// WithKernelOptions appends the host's kernel options -- the deployment mode,
// the seam preset, and any WithEventBus/WithKVStore/WithMailer/WithObjectStore
// injection -- to the kernel the transition assembly bootstraps over the
// module set's asset stand-ins. They travel unchanged: which implementations
// a process runs is the assembler's decision, never the engine's.
func WithKernelOptions(opts ...pkgcore.KernelOption) Option {
	return func(c *engineConfig) { c.kernelOptions = append(c.kernelOptions, opts...) }
}

// ObservabilitySpec selects the engine's observability component and names
// its configuration: the service name every span and metric is tagged with,
// and the OTLP/gRPC endpoint traces and metrics are pushed to. Both are host
// policy values -- the engine passes them through and ships no default of its
// own, so an empty ServiceName leaves go/observability's own documented
// default and an empty OTLPEndpoint leaves its local exporters.
type ObservabilitySpec struct {
	// ServiceName becomes the service.name resource attribute.
	ServiceName string
	// OTLPEndpoint is the "host:port" OTLP/gRPC collector target; empty
	// keeps the local exporters.
	OTLPEndpoint string
}

// WithObservability selects the engine's observability component with spec
// and deselects it otherwise: without this option the assembly never touches
// the observability providers -- a host that initializes them itself (or
// wants none) omits it.
func WithObservability(spec ObservabilitySpec) Option {
	return func(c *engineConfig) {
		s := spec
		c.observability = &s
	}
}

// Worker is the background process the assembled application runs: the job
// queue a host constructs and wires, or any other long-running component with
// the same start/stop shape. Start runs during the assembly's Start stage,
// after the pre-serve step and before anything listens; Close runs in the
// shutdown's reverse-order close, bounded by ShutdownTimeout, whether or not
// Start ever ran -- the same contract jobs.StandaloneQueue.Close documents
// (idempotent and safe before Start).
type Worker interface {
	// Start begins the worker's background work. It is called with the
	// assembly context.
	Start(ctx context.Context) error
	// Close stops the worker and releases its resources, bounded by the
	// caller's context.
	Close(ctx context.Context) error
}

// WithWorker registers w as the application's background worker.
func WithWorker(w Worker) Option {
	return func(c *engineConfig) { c.worker = w }
}

// WithoutBackgroundWorkers makes the worker component's Start a no-op: the
// worker is still registered (and still closed by the shutdown), but this
// process's own background goroutines never launch -- the shape a replica that
// must never claim or execute a job composes, and the library-level form of
// the reference app's DisableQueueWorker switch.
func WithoutBackgroundWorkers() Option {
	return func(c *engineConfig) { c.withoutWorkers = true }
}
