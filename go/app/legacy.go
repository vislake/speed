package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// legacy.go carries the transition assembly: the thin delegation that keeps
// the pre-component outward surface (Option, New, Run, Application) working
// while the hosts migrate. It is explicitly temporary and is deleted once
// the hosts assemble through the new machinery themselves.
//
// The old eight stages run as the new machinery's drive plus one timing
// shift, never as a second implementation of anything:
//
//   - The configuration load (old stage 1) is the engine loader's beat --
//     the same pkgcore/config Loader over the same targets and options.
//   - The infrastructure and module construction (old stages 2 and 3) run as
//     the transition prelude: the platform cipher, the host's pre-database
//     callback, the database open and the host's WithModules callback.
//     They must precede the drive, because the host's callback receives the
//     open database -- the one place the transition shifts timing.
//   - The registry names, the wrapped module set, the host steps and the
//     HTTP face become components; their callbacks delegate where the old
//     stages delegated (module Register, Attach hooks, the handler
//     composition) and own what the old stages owned (migrations in the
//     database component's Verify, the handler drain in the HTTP component's
//     Stop and Close, the worker in its own component).
//   - The drive itself is the engine's one driver, over the same seven
//     stages, with the registry's own rollback semantics.

// Application is one assembled speed application: the host's bootstrap
// configuration resolved, the database opened and migrated, the module set
// declared through the component assembly's seats, the host's attach and
// wiring hooks run, the HTTP handler composed, and the background worker
// started.
//
// New returns one without serving it; Run builds one, serves it and drains it.
// Every accessor is safe to call once New has returned.
type Application struct {
	registry *pkgcore.Registry
	reg      *pkgcore.ComponentRegistry

	handler http.Handler
	db      *gorm.DB
	cipher  *dbkit.Cipher

	// server is the HTTP server the HTTP component composes (not yet
	// listening); Run serves it and Close drains it.
	server *http.Server

	// baseCtx is the context every served request inherits: Run sets it to
	// the context Run was called with -- deliberately not the signal-derived
	// context -- so a shutdown signal never cancels in-flight requests before
	// the graceful drain can finish (net/http's Server.BaseContext contract).
	baseCtx context.Context

	closeOnce sync.Once
	closeErr  error
}

// Handler returns the composed HTTP handler: the configured SPA outermost,
// then the host's middleware chain (or, for a host composing its own face
// through HTTPSpec.Compose, that composition), and the mux carrying the
// liveness routes, every module route and the host's extra routes. Run wraps
// it in obs.Middleware at serve time -- observe the served request path,
// never this value, for the instrumentation's position. It is nil until the
// HTTP component has composed the face (a PostBootstrap hook still sees nil;
// a PreServe hook does not).
func (a *Application) Handler() http.Handler { return a.handler }

// Registry returns the module Registry the composed modules declared into:
// every route, configuration schema, bootstrap key, feature flag, permission,
// job handler, notification type, event subscription, audit action, retention
// participant and periodic task the module set declared, plus the resolved
// infrastructure seams (EventBus, KVStore, Mailer, ObjectStore).
//
// The registry's declaration seats are the component assembly's own seats,
// so its contents are the same declarations the assembly planned, validated
// and drove.
func (a *Application) Registry() *pkgcore.Registry { return a.registry }

// Close drains the application and releases every resource the assembly
// built: the non-blocking Stop notification first (the HTTP server stops
// accepting, the drain begins), then the components' Close callbacks in
// reverse dependency order -- the worker's drain, the HTTP server's
// drain, the kernel's seam shutdown, the database close, and the
// observability flush last. It is idempotent: the first call runs the
// shutdown, and every later call returns the same result without repeating a
// step.
//
// Each component bounds its own drain (the serve timeouts) on top of the
// caller's context; every step is attempted even when an earlier one failed;
// the joined result is returned and cached.
func (a *Application) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		if a.reg == nil {
			a.closeErr = errors.New("app: the application carries no component assembly to close")
			return
		}
		a.closeErr = Shutdown(ctx, a.reg)
	})
	return a.closeErr
}

// transitionAssembly carries the state one legacy assembly builds: the
// configuration the host's options resolved, the resources the prelude
// built (tracked individually, so a failure before the drive can release
// exactly what the components do not yet own), and the component registry
// the drive runs over.
type transitionAssembly struct {
	cfg *engineConfig
	app *Application

	kernel *pkgcore.Kernel
	view   *pkgcore.Registry
	reg    *pkgcore.ComponentRegistry

	httpState *legacyHTTPState

	// dbConstructed and kernelConstructed record whether the drive ever
	// constructed the components owning the database and the kernel, which
	// is what decides whether a failed build releases them directly (the
	// prelude built them) or has already had them released (the registry's
	// rollback closed them).
	dbConstructed     bool
	kernelConstructed bool
}

// assembleLegacy runs the transition assembly for one New or Run call: the
// prelude (configuration, infrastructure, module construction, the kernel
// bootstrap and the registry names), the component registration and the
// seven-stage drive. A failure releases exactly what the prelude built and
// no component owns.
func assembleLegacy(ctx context.Context, cfg *engineConfig) (*Application, error) {
	a := &Application{baseCtx: ctx}
	asm := &transitionAssembly{cfg: cfg, app: a}
	if err := asm.build(ctx); err != nil {
		if closeErr := asm.releasePrelude(context.WithoutCancel(ctx)); closeErr != nil {
			obs.FromContext(ctx).Error("the transition prelude teardown failed", "error", closeErr)
		}
		return nil, err
	}
	return a, nil
}

// build runs the prelude and then the drive.
func (asm *transitionAssembly) build(ctx context.Context) error {
	cfg := asm.cfg

	// Prelude, configuration: the same loader invocation the engine loader
	// performs, so the host's option values (which may depend on loaded
	// configuration) and the drive's own load agree by construction.
	if err := loadConfiguration(cfg); err != nil {
		return err
	}

	// Prelude, infrastructure: the platform cipher, the host's pre-database
	// callback, then the database itself.
	if err := asm.openInfrastructure(ctx); err != nil {
		return err
	}

	// Prelude, modules: the host's callback constructs the module set over
	// the open database and the platform cipher.
	modules, err := asm.constructModules(ctx)
	if err != nil {
		return err
	}

	// The kernel: one bootstrap over asset-carrying stand-ins, which resolves
	// the four seams, validates them against the deployment mode and merges
	// the module set's locale resources into the message catalog.
	if err := asm.bootstrapKernel(ctx, modules); err != nil {
		return err
	}

	// The component registry: seeded from the global registration (the
	// engine's own components included), then the seam values, the wrapped
	// module set, the host steps and the HTTP and worker components.
	if err := asm.buildRegistry(modules); err != nil {
		return err
	}

	// The drive: the engine loader (configuration targets, composition
	// configuration, bootstrap material) and the seven stages.
	return Assemble(ctx, asm.reg, LoadSpec{
		Host:     cfg.configSpec.Host,
		Platform: cfg.configSpec.Platform,
		Options:  cfg.configOptions,
	})
}

// releasePrelude releases the resources the prelude built that no component
// owns: the kernel's resolved seams and the database handle. Anything the
// drive already constructed was released by the registry's own rollback.
func (asm *transitionAssembly) releasePrelude(ctx context.Context) error {
	var errs []error
	if asm.kernel != nil && !asm.kernelConstructed {
		if err := asm.kernel.Shutdown(); err != nil {
			errs = append(errs, fmt.Errorf("app: shut the kernel down: %w", err))
		}
	}
	if asm.app.db != nil && !asm.dbConstructed {
		if err := closeDatabase(ctx, asm.app.db); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// openInfrastructure is the old infrastructure stage: the platform cipher,
// the host's pre-database callback, then the database itself.
func (asm *transitionAssembly) openInfrastructure(ctx context.Context) error {
	cfg := asm.cfg
	cipher, err := dbkit.NewCipher(cfg.configSpec.Platform.Config.Cipher_Key)
	if err != nil {
		return fmt.Errorf("app: build the platform cipher from the key material at %q: %w", platformCipherKeyPath, err)
	}
	asm.app.cipher = cipher

	if cfg.preDB != nil {
		if preDBErr := cfg.preDB(ctx, cipher); preDBErr != nil {
			return fmt.Errorf("app: pre-database callback: %w", preDBErr)
		}
	}

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect:     cfg.databaseSpec.Dialect,
		DSN:         cfg.databaseSpec.DSN,
		AuditBus:    cfg.databaseSpec.AuditBus,
		AuditModels: cfg.databaseSpec.AuditModels,
	})
	if err != nil {
		return fmt.Errorf("app: open the database: %w", err)
	}
	asm.app.db = db
	return nil
}

// constructModules runs the WithModules callback. No callback means no
// modules -- a legitimate set for a host that composes nothing.
func (asm *transitionAssembly) constructModules(ctx context.Context) ([]pkgcore.Module, error) {
	if asm.cfg.modules == nil {
		return nil, nil
	}
	modules, err := asm.cfg.modules(ctx, ModuleDeps{DB: asm.app.db, Cipher: asm.app.cipher})
	if err != nil {
		return nil, fmt.Errorf("app: construct the module set: %w", err)
	}
	return modules, nil
}

// bootstrapKernel bootstraps the kernel over the module set's asset
// stand-ins and re-points the returned registry's declaration seats at the
// component assembly's seats.
func (asm *transitionAssembly) bootstrapKernel(ctx context.Context, modules []pkgcore.Module) error {
	kernel := pkgcore.NewKernel(asm.cfg.kernelOptions...)
	standIns := make([]pkgcore.Module, 0, len(modules))
	for _, m := range modules {
		standIns = append(standIns, assetStandIn{module: m})
	}
	base, err := kernel.Bootstrap(ctx, standIns...)
	if err != nil {
		return fmt.Errorf("app: bootstrap the kernel: %w", err)
	}
	asm.kernel = kernel
	asm.view = base
	return nil
}

// httpComponent returns the transition HTTP component and records its state,
// which the Application accessors read once the drive has run.
func (asm *transitionAssembly) httpComponent() pkgcore.Component {
	component, state := legacyHTTPComponent(asm.cfg, asm.app.registry, asm.app)
	asm.httpState = state
	return component
}

// buildRegistry creates the component registry and registers everything the
// transition assembly contributes: the resolved seam values into the by-type
// context, the database and kernel components, the wrapped module set, the
// host steps and the HTTP and worker components, plus the composition
// override that selects exactly this component set.
func (asm *transitionAssembly) buildRegistry(modules []pkgcore.Module) error {
	reg := pkgcore.NewComponentRegistry()

	// The resolved seams join the by-type context so the assembly's seats
	// (the Events seat's bus lookup, in particular) reach the same values the
	// module Registry's accessors answer, and a declaration body reading the
	// accessors off the assembly reaches them under the same names.
	putSeamValue(reg, asm.view.EventBus())
	putSeamValue(reg, asm.view.KVStore())
	putSeamValue(reg, asm.view.Mailer())
	putSeamValue(reg, asm.view.ObjectStore())
	putSeamValue(reg, asm.view.Locales())

	asm.reg = reg
	asm.app.reg = reg
	asm.app.registry = buildLegacyRegistryView(asm.view, reg)

	wrapped, err := wrapLegacyModules(modules, asm.app.registry, reg)
	if err != nil {
		return err
	}

	// The registration order is the plan order (ties between independent
	// components resolve by it), and it reproduces the old stages' order: the
	// database and kernel first, the wrapped module set next (their Init
	// callbacks register and declare), then the host steps and the HTTP and
	// worker components -- post-bootstrap (whose binding verification reads
	// the module declarations) after the modules, the HTTP face between the
	// attach step and the pre-serve step, the worker last. The reverse of
	// this order is the close order: worker, pre-serve, HTTP drain, the rest,
	// the database, the observability component (planned first, so it closes
	// last and flushes what everything above emitted).
	components := make([]pkgcore.Component, 0, len(wrapped)+7)
	components = append(components, asm.databaseComponent(modules), asm.kernelComponent())
	components = append(components, wrapped...)
	components = append(components, asm.postBootstrapComponent(), asm.postAttachComponent())
	components = append(components, asm.httpComponent(), asm.preServeComponent())
	if worker, ok := asm.workerComponent(); ok {
		components = append(components, worker)
	}

	selected := make([]string, 0, len(components))
	for _, component := range components {
		if err := reg.Register(component); err != nil {
			return fmt.Errorf("app: register the transition component %q: %w", component.Name, err)
		}
		selected = append(selected, component.Name)
	}

	reg.Put(asm.compositionOverride(selected))
	return nil
}

// databaseComponent owns the prelude-opened database: its product is the
// handle the prelude opened, its Verify applies every module's migration set,
// and its Close closes the handle.
func (asm *transitionAssembly) databaseComponent(modules []pkgcore.Module) pkgcore.Component {
	dialect := asm.cfg.databaseSpec.Dialect
	return pkgcore.Component{
		Name:     legacyComponentPrefix + "db",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			asm.dbConstructed = true
			return asm.app.db, nil
		},
		Verify: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			db, ok := instance.(*gorm.DB)
			if !ok {
				return fmt.Errorf("app: the database component holds an instance of type %T", instance)
			}
			return applyMigrations(ctx, db, dialect, modules)
		},
		Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			db, ok := instance.(*gorm.DB)
			if !ok {
				return fmt.Errorf("app: the database component holds an instance of type %T", instance)
			}
			return closeDatabase(ctx, db)
		},
	}
}

// kernelComponent owns the kernel's seam lifecycle: its Close shuts the
// kernel down (the reverse of its bootstrap), releasing every seam
// implementation the bootstrap resolved from the preset.
func (asm *transitionAssembly) kernelComponent() pkgcore.Component {
	return pkgcore.Component{
		Name: legacyComponentPrefix + "kernel",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			asm.kernelConstructed = true
			return &transitionMarker{name: "kernel"}, nil
		},
		Close: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			return asm.kernel.Shutdown()
		},
	}
}

// postBootstrapComponent maps the host's post-bootstrap hook onto a step
// component. It verifies the bootstrap-key binding first -- the wrapped
// modules declared on the binding seat during their Init callbacks, which
// have all run by this component's turn, and the host's hook is the first
// consumer -- and then runs the hook.
func (asm *transitionAssembly) postBootstrapComponent() pkgcore.Component {
	cfg := asm.cfg
	return pkgcore.Component{
		Name: legacyComponentPrefix + "post_bootstrap",
		New:  newTransitionMarker,
		Init: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			if err := verifyBinding(cfg.configSpec, asm.app.registry); err != nil {
				return err
			}
			if cfg.hooks.PostBootstrap != nil {
				if err := cfg.hooks.PostBootstrap(ctx, asm.app); err != nil {
					return fmt.Errorf("app: PostBootstrap hook: %w", err)
				}
			}
			return nil
		},
	}
}

// postAttachComponent maps the host's wiring hook onto a step component.
func (asm *transitionAssembly) postAttachComponent() pkgcore.Component {
	cfg := asm.cfg
	return pkgcore.Component{
		Name: legacyComponentPrefix + "post_attach",
		New:  newTransitionMarker,
		Init: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			if cfg.hooks.PostAttach != nil {
				if err := cfg.hooks.PostAttach(ctx, asm.app); err != nil {
					return fmt.Errorf("app: PostAttach hook: %w", err)
				}
			}
			return nil
		},
	}
}

// preServeComponent maps the host's pre-serve hook onto a step component.
// The hook runs as an Init callback -- the plan places the component after
// the HTTP face's compose and before the worker's start, the old stage
// order -- because the steps it carries (imperative schema creation, demo
// seeds through the composed handler, subscriptions installed for serving)
// are declarations and wiring: they need the seats open, and they must
// complete before anything listens, which the assembly's own Start stage is
// the signal for.
func (asm *transitionAssembly) preServeComponent() pkgcore.Component {
	cfg := asm.cfg
	return pkgcore.Component{
		Name: legacyComponentPrefix + "pre_serve",
		New:  newTransitionMarker,
		Init: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			if cfg.hooks.PreServe != nil {
				if err := cfg.hooks.PreServe(ctx, asm.app); err != nil {
					return fmt.Errorf("app: PreServe hook: %w", err)
				}
			}
			return nil
		},
	}
}

// workerComponent maps the host's Worker onto a component: Start launches it
// (skipped under WithoutBackgroundWorkers), Close drains it bounded by the
// shutdown timeout -- safe before Start, by the worker contract.
func (asm *transitionAssembly) workerComponent() (pkgcore.Component, bool) {
	worker := asm.cfg.worker
	if worker == nil {
		return pkgcore.Component{}, false
	}
	return pkgcore.Component{
		Name: legacyComponentPrefix + "worker",
		New:  newTransitionMarker,
		Start: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			if asm.cfg.withoutWorkers {
				return nil
			}
			if err := worker.Start(ctx); err != nil {
				return fmt.Errorf("app: start the background worker: %w", err)
			}
			return nil
		},
		Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			closeCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
			defer cancel()
			if err := worker.Close(closeCtx); err != nil {
				return fmt.Errorf("app: close the background worker: %w", err)
			}
			return nil
		},
	}, true
}

// compositionOverride builds the code-override layer that selects exactly
// the component set the transition assembly registered: the engine's
// observability component first (selected with the host's observability
// specification, or explicitly deselected when the host declared none), then
// every registered transition component and wrapped module in plan order.
//
// The deployment mode deliberately stays unset: the kernel is the
// transition's mode authority (it resolved the seams and validated them
// against the host's WithDeploymentMode), and the wrapped modules carry no
// component capability bits for the assembly to re-judge. A host that
// configures the composition itself (file, environment, flags) is merged
// underneath this layer, so the adapter's own selections stand.
func (asm *transitionAssembly) compositionOverride(selected []string) CompositionOverrides {
	block := pkgcore.ComponentConfig{}
	if spec := asm.cfg.observability; spec != nil {
		block = block.With("observability", pkgcore.ComponentConfig{}.
			With("service_name", spec.ServiceName).
			With("otlp_endpoint", spec.OTLPEndpoint))
	} else {
		block = block.With("observability", false)
	}
	for _, name := range selected {
		block = block.With(name, nil)
	}
	return CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components", block)}
}

// putSeamValue publishes one resolved seam value into the component
// registry's by-type context when it is non-nil.
func putSeamValue(reg *pkgcore.ComponentRegistry, value any) {
	if value == nil {
		return
	}
	reg.Put(value)
}

// closeDatabase closes the database handle. It is the database component's
// Close body and, on the failure path, the prelude's own release.
func closeDatabase(_ context.Context, db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("app: reach the database handle for closing: %w", err)
	}
	if sqlDB == nil {
		return nil
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("app: close the database: %w", err)
	}
	return nil
}
