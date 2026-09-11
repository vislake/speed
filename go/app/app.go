package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"sync"
	"syscall"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// Option configures the application New assembles and Run assembles and
// serves. Every option is a plain setter; the host composes the option list
// its own policy resolves and hands it over unchanged. WithConfig and
// WithDatabase are required -- the engine ships no implicit defaults, so a
// missing one is a named startup error rather than a silently substituted
// value.
type Option func(*engineConfig)

// engineConfig is the option set one New or Run call assembles from. It is
// built once, before any stage runs, and read-only afterwards.
type engineConfig struct {
	configSpec    *ConfigSpec
	configOptions []ConfigOption

	databaseSpec *DatabaseSpec
	preDB        func(ctx context.Context, cipher *dbkit.Cipher) error
	modules      func(ctx context.Context, deps ModuleDeps) ([]pkgcore.Module, error)

	kernelOptions []pkgcore.KernelOption
	observability *ObservabilitySpec
	httpSpec      *HTTPSpec
	hooks         Hooks
	worker        Worker

	// withoutWorkers is WithoutBackgroundWorkers' flag: stage 8 skips
	// worker.Start. A registered worker is still closed by Close, mirroring
	// the worker contract jobs.StandaloneQueue's Close documents (safe to
	// call before Start).
	withoutWorkers bool

	// obsShutdown is the observability shutdown func Run recorded (nil when
	// Run had no ObservabilitySpec, or when the caller is New). Close runs
	// it last.
	obsShutdown func(context.Context) error
}

// Application is one assembled speed application: the host's bootstrap
// configuration resolved, the database opened and migrated, the module set
// registered through the kernel, the host's attach and wiring hooks run, the
// HTTP handler composed, and the background worker started.
//
// New returns one without serving it; Run builds one, serves it and drains it.
// Every accessor is safe to call once New has returned.
type Application struct {
	kernel   *pkgcore.Kernel
	registry *pkgcore.Registry
	handler  http.Handler
	db       *gorm.DB
	cipher   *dbkit.Cipher
	worker   Worker

	// server is the HTTP server the HTTP-face stage composes (not yet
	// listening); Run serves it and Close drains it.
	server *http.Server

	// baseCtx is the context every served request inherits: Run sets it to
	// the context Run was called with -- deliberately not the signal-derived
	// context -- so a shutdown signal never cancels in-flight requests before
	// the graceful drain can finish (net/http's Server.BaseContext contract).
	baseCtx context.Context

	// obsShutdown is Run's observability shutdown func; Close runs it after
	// every other step.
	obsShutdown func(context.Context) error

	closeOnce sync.Once
	closeErr  error
}

// New assembles an application: it resolves the host's bootstrap
// configuration, opens the database and applies every module's migrations,
// constructs and bootstraps the module set, runs the host's attach and wiring
// hooks, composes the HTTP handler, and starts the background worker. It does
// not listen; a caller that wants the engine to serve and drain calls Run
// instead.
//
// The stages run in one fixed order, and a failure at any stage tears the
// already-built resources down (the same ordered shutdown Close performs)
// before the error is returned:
//
//  1. configuration  -- load ConfigSpec.Host and ConfigSpec.Platform
//  2. infrastructure -- build the platform cipher from PlatformConfig,
//     run WithPreDB, open the database
//  3. modules        -- WithModules constructs the module set
//  4. kernel         -- apply migrations, bootstrap the kernel, verify the
//     bootstrap-key binding
//  5. attach         -- Hooks.PostBootstrap
//  6. wiring         -- Hooks.PostAttach
//  7. HTTP face      -- compose the handler
//  8. background     -- Hooks.PreServe, then Worker.Start
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
	return assemble(ctx, cfg)
}

// Run assembles an application with New and serves it until ctx is done or
// the listener fails, then drains it in the ordered shutdown Application.Close
// performs. Signal handling is the platform's: Run overlays SIGINT and SIGTERM
// on ctx, so a caller passes the base context (the logger-carrying one) and
// never builds a signal context of its own.
//
// When the options carry an ObservabilitySpec, Run initializes observability
// before assembly (so the HTTP face's obs.Middleware binds the live providers)
// and shuts it down last during the drain. A host that manages observability
// itself omits the option and Run never touches the observability providers.
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

	if cfg.observability != nil {
		shutdown, initErr := obs.Init(ctx, cfg.observability.options()...)
		if initErr != nil {
			return fmt.Errorf("app: init observability: %w", initErr)
		}
		cfg.obsShutdown = shutdown
	}

	a, err := assemble(ctx, cfg)
	if err != nil {
		if cfg.obsShutdown != nil {
			// Assembly failed, but the providers Run just initialized are
			// live; flush them rather than leaving them for a process exit
			// that never runs Close.
			_ = cfg.obsShutdown(context.Background())
		}
		return err
	}
	a.baseCtx = baseCtx
	a.obsShutdown = cfg.obsShutdown
	return a.serve(ctx)
}

// Handler returns the composed HTTP handler: the configured SPA outermost,
// then the host's middleware chain (or, for a host composing its own face
// through HTTPSpec.Compose, that composition), and the mux carrying the
// liveness routes, every module route and the host's extra routes. Run wraps
// it in obs.Middleware at serve time -- observe the served request path,
// never this value, for the instrumentation's position. It is nil until the
// HTTP-face stage has run (a PostBootstrap hook still sees nil; a PreServe
// hook does not).
func (a *Application) Handler() http.Handler { return a.handler }

// Registry returns the Registry the kernel bootstrapped: every route,
// configuration schema, bootstrap key, feature flag, permission, job handler,
// notification type, event subscription, audit action, retention participant
// and periodic task the module set declared, plus the resolved infrastructure
// seams (EventBus, KVStore, Mailer, ObjectStore).
func (a *Application) Registry() *pkgcore.Registry { return a.registry }

// Kernel returns the Kernel that bootstrapped the registry. Its Shutdown runs
// as step ③ of Close.
func (a *Application) Kernel() *pkgcore.Kernel { return a.kernel }

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

// assemble runs the eight stages in order, tearing the built resources down
// when one fails.
func assemble(ctx context.Context, cfg *engineConfig) (*Application, error) {
	a := &Application{
		worker:  cfg.worker,
		baseCtx: ctx,
	}
	rollback := func(err error) (*Application, error) {
		if closeErr := a.Close(context.WithoutCancel(ctx)); closeErr != nil {
			obs.FromContext(ctx).Error("rollback after a failed assembly stage failed", "error", closeErr)
		}
		return nil, err
	}

	// Stage 1 -- configuration.
	if err := loadConfiguration(cfg); err != nil {
		return rollback(err)
	}
	// Stage 2 -- infrastructure: the platform cipher, the host's pre-database
	// callback, then the database itself.
	if err := a.openInfrastructure(ctx, cfg); err != nil {
		return rollback(err)
	}
	// Stage 3 -- module construction.
	modules, err := constructModules(ctx, cfg, a)
	if err != nil {
		return rollback(err)
	}
	// Stage 4 -- kernel bootstrap, with the bootstrap-key binding verified
	// against the host's configuration targets once the registry is complete.
	if err := a.bootstrapKernel(ctx, cfg, modules); err != nil {
		return rollback(err)
	}
	// Stage 5 -- the host's post-bootstrap attach.
	if cfg.hooks.PostBootstrap != nil {
		if err := cfg.hooks.PostBootstrap(ctx, a); err != nil {
			return rollback(fmt.Errorf("app: PostBootstrap hook: %w", err))
		}
	}
	// Stage 6 -- the host's wiring (seam bridges, platform credentials,
	// background wiring): everything that needs the bootstrapped registry.
	if cfg.hooks.PostAttach != nil {
		if err := cfg.hooks.PostAttach(ctx, a); err != nil {
			return rollback(fmt.Errorf("app: PostAttach hook: %w", err))
		}
	}
	// Stage 7 -- the HTTP face.
	if err := a.buildHandler(cfg); err != nil {
		return rollback(err)
	}
	// Stage 8 -- the host's pre-serve step, then the background worker, both
	// after the handler exists and before anything listens.
	if cfg.hooks.PreServe != nil {
		if err := cfg.hooks.PreServe(ctx, a); err != nil {
			return rollback(fmt.Errorf("app: PreServe hook: %w", err))
		}
	}
	if cfg.worker != nil && !cfg.withoutWorkers {
		if err := cfg.worker.Start(ctx); err != nil {
			return rollback(fmt.Errorf("app: start the background worker: %w", err))
		}
	}
	return a, nil
}

// constructModules runs the WithModules callback over the opened database and
// the platform cipher. No callback means no modules -- a legitimate set for a
// host that composes nothing.
func constructModules(ctx context.Context, cfg *engineConfig, a *Application) ([]pkgcore.Module, error) {
	if cfg.modules == nil {
		return nil, nil
	}
	modules, err := cfg.modules(ctx, ModuleDeps{DB: a.db, Cipher: a.cipher})
	if err != nil {
		return nil, fmt.Errorf("app: construct the module set: %w", err)
	}
	return modules, nil
}

// bootstrapKernel applies every module's migrations to the opened database,
// bootstraps the kernel over the module set, and proves the host's
// configuration targets bind every bootstrap key the modules declared.
func (a *Application) bootstrapKernel(ctx context.Context, cfg *engineConfig, modules []pkgcore.Module) error {
	if err := applyMigrations(ctx, a.db, cfg.databaseSpec.Dialect, modules); err != nil {
		return err
	}
	kernel := pkgcore.NewKernel(cfg.kernelOptions...)
	reg, err := kernel.Bootstrap(ctx, modules...)
	if err != nil {
		return fmt.Errorf("app: bootstrap the kernel: %w", err)
	}
	a.kernel = kernel
	a.registry = reg
	if err := verifyBinding(cfg.configSpec, reg); err != nil {
		return err
	}
	return nil
}

// WithKernelOptions appends the host's kernel options -- the deployment mode,
// the seam preset, and any WithEventBus/WithKVStore/WithMailer/WithObjectStore
// injection -- to the kernel the engine bootstraps in stage 4. They travel
// unchanged: which implementations a process runs is the assembler's decision,
// never the engine's.
func WithKernelOptions(opts ...pkgcore.KernelOption) Option {
	return func(c *engineConfig) { c.kernelOptions = append(c.kernelOptions, opts...) }
}

// ObservabilitySpec names the observability wiring Run initializes: the
// service name every span and metric is tagged with, and the OTLP/gRPC
// endpoint traces and metrics are pushed to. Both are host policy values --
// the engine passes them through and ships no default of its own, so an empty
// ServiceName leaves go/observability's own documented default and an empty
// OTLPEndpoint leaves its local exporters.
type ObservabilitySpec struct {
	// ServiceName becomes the service.name resource attribute.
	ServiceName string
	// OTLPEndpoint is the "host:port" OTLP/gRPC collector target; empty
	// keeps the local exporters.
	OTLPEndpoint string
}

// options renders the spec as go/observability options, omitting each option
// whose value is empty exactly as the package documents an unset value.
func (s ObservabilitySpec) options() []obs.Option {
	var opts []obs.Option
	if s.ServiceName != "" {
		opts = append(opts, obs.WithServiceName(s.ServiceName))
	}
	if s.OTLPEndpoint != "" {
		opts = append(opts, obs.WithOTLPEndpoint(s.OTLPEndpoint))
	}
	return opts
}

// WithObservability makes Run initialize observability from spec before
// assembly and shut it down last during the drain. Without this option Run
// never touches the observability providers: a host that initializes them
// itself (or wants none) omits it.
func WithObservability(spec ObservabilitySpec) Option {
	return func(c *engineConfig) {
		s := spec
		c.observability = &s
	}
}

// Worker is the background process the assembled application runs: the job
// queue a host constructs and wires, or any other long-running component with
// the same start/stop shape. Start is called in stage 8, after the PreServe
// hook and before anything listens; Close is called by the ordered shutdown,
// bounded by ShutdownTimeout, whether or not Start ever ran -- the same
// contract jobs.StandaloneQueue.Close documents (idempotent and safe before
// Start).
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

// WithoutBackgroundWorkers makes stage 8 skip Worker.Start: the worker is
// still registered (and still closed by the ordered shutdown), but this
// process's own background goroutines never launch -- the shape a replica that
// must never claim or execute a job composes, and the library-level form of
// the reference app's DisableQueueWorker switch.
func WithoutBackgroundWorkers() Option {
	return func(c *engineConfig) { c.withoutWorkers = true }
}
