package app

// This file carries the reference app's own components: the application
// component, which composes and serves the host's HTTP face, and the step
// components wrapping the assembly steps attach.go and serve.go implement.
// A step component's callback binds the assembled values its body reads and
// then runs that body, so a step has one implementation.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	speedapp "github.com/vislake/speed/go/app"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// hostFace is the application component's product: the composed HTTP handler
// and the listener's drain state. Its reads and lifecycle methods are what a
// host-side serve loop drives; the fields stay unexported because only this
// component's callbacks write them.
type hostFace struct {
	// handler is the composed face: the protected-face composition
	// (composeFace), wrapped in the shared SPA file server when this boot
	// serves a frontend directory.
	handler http.Handler
	// server is the http.Server Start builds and serves. It carries the
	// observability middleware as its handler, the serve timeouts and the
	// request base context.
	server *http.Server
	// listener is the bound listener Start serves on; an error Serve
	// reports other than the shutdown's own ErrServerClosed is logged at
	// Error level.
	listener net.Listener
	// addr is the listen address: the resolved port before Start, the
	// listener's own address (the real port a :0 bind picked) after it.
	addr string
	// baseCtx is the context every served request inherits. It is
	// deliberately the host's own assembly context, never the
	// signal-derived context a serve loop waits on, so a shutdown signal
	// never cancels in-flight requests ahead of the drain (net/http's
	// Server.BaseContext contract).
	baseCtx context.Context
	// drained closes when the asynchronous drain Stop began has finished;
	// drainErr carries its result. Both are written before the close, so a
	// reader that sees the closed channel sees the error.
	drained  chan struct{}
	drainErr error

	stopOnce sync.Once
}

// Handler returns the composed handler. It is nil until the component's Init
// has run.
func (f *hostFace) Handler() http.Handler { return f.handler }

// start builds the http.Server over the composed handler and serves it on
// the face's listen address, in a goroutine: the observability middleware is
// applied here, at serve time, exactly where the engine's serve loop applies
// it (the outermost layer a served request reaches), the server's request
// base context is the face's own, and Start records the listener's real
// address so a caller can reach a port the operating system picked.
func (f *hostFace) start(ctx context.Context) error {
	server := &http.Server{
		Addr:              f.addr,
		Handler:           obs.Middleware(f.handler),
		ReadHeaderTimeout: speedapp.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return f.baseCtx },
	}
	listener, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("reference-app: serve: listen on %s: %w", f.addr, err)
	}
	f.server = server
	f.listener = listener
	f.addr = listener.Addr().String()
	f.drained = make(chan struct{})
	obs.FromContext(ctx).Info("server listening", "addr", f.addr)
	go func() {
		if err := server.Serve(f.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			obs.FromContext(ctx).Error("the server stopped serving", "error", err)
		}
	}()
	return nil
}

// stop is the non-blocking half of shutdown: it detaches a goroutine that
// stops the listener from accepting and waits out the in-flight requests,
// bounded by the shutdown timeout, and returns immediately. A face that
// never started is a no-op, so Stop is safe before Start.
func (f *hostFace) stop(ctx context.Context) {
	if f.server == nil || f.drained == nil {
		return
	}
	f.stopOnce.Do(func() {
		go func() {
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), speedapp.ShutdownTimeout)
			defer cancel()
			f.drainErr = f.server.Shutdown(drainCtx)
			close(f.drained)
		}()
	})
}

// close is the blocking half: it reports the drain stop began, waiting it
// out bounded by the shutdown timeout on top of the caller's context. A
// face whose drain has not finished -- Close before Stop, or a drain still
// in flight -- stops accepting and waits out the in-flight requests here,
// synchronously and bounded the same way; a face that never started
// releases nothing.
func (f *hostFace) close(ctx context.Context) error {
	if f.server == nil {
		return nil
	}
	if f.drained != nil {
		select {
		case <-f.drained:
			if f.drainErr != nil {
				return fmt.Errorf("reference-app: shut the HTTP server down: %w", f.drainErr)
			}
			return nil
		default:
		}
	}

	drainCtx, cancel := context.WithTimeout(ctx, speedapp.ShutdownTimeout)
	defer cancel()
	if err := f.server.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("reference-app: shut the HTTP server down: %w", err)
	}
	return nil
}

// hostStep is a step component's product: New must return one non-nil value
// and a step owns no resource of its own -- what it works on is the
// serverBuild and the view it derives from the registry.
type hostStep struct{}

// newHostStep produces the marker a step component's New returns.
func newHostStep(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
	return &hostStep{}, nil
}

// hostFaceOf returns the application component's own product from the
// instance a callback was handed. The registry passes back the very value
// New returned, so a mismatch means the descriptor and its callbacks
// disagree about the product -- a wiring error reported by name rather than
// a panic inside a lifecycle callback.
func hostFaceOf(instance any) (*hostFace, error) {
	face, ok := instance.(*hostFace)
	if !ok {
		return nil, fmt.Errorf("reference-app: the application component was handed a %T, want its own *hostFace product", instance)
	}
	return face, nil
}

// appComponent returns the host's application component: the one component
// that owns the HTTP face and the listener.
//
// Its product is the *hostFace the pre-serve step requires, which turns
// "the face exists" into plan order. Init composes the face -- the phase
// that must compose it: composing installs the two event subscriptions (the
// demo notification glue and the smilesim terminal signal), no subscription
// may land after a Start, and the seat gate admits those writes only while
// Init runs. Start only listens; Stop begins the non-blocking drain and
// Close waits it out.
//
// baseCtx is the context every served request inherits: the host's own
// assembly context, never a signal-derived one, so a shutdown signal never
// cancels in-flight requests ahead of the drain.
//
// live tells the component whether this assembly owns the listener: Start
// binds and serves it only then. A drive that hands the composed handler to
// its caller (BuildServer, whose handler an httptest.Server or the caller's
// own server fronts) composes the very same face with no listener of its
// own, which is the difference between the two drives and nothing else.
func appComponent(b *serverBuild, baseCtx context.Context, live bool) pkgcore.Component {
	return pkgcore.Component{
		Name:     "reference-app.app",
		Provides: []any{(*hostFace)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &hostFace{baseCtx: baseCtx}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			view, err := viewFromComponents(reg)
			if err != nil {
				return err
			}
			if err := b.bindRegistry(reg); err != nil {
				return err
			}
			if err := b.bindRuntimeServices(reg); err != nil {
				return err
			}
			return b.composeHostFace(view, face)
		},
		Start: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			if !live {
				return nil
			}
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			return face.start(ctx)
		},
		Stop: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			face.stop(ctx)
			return nil
		},
		Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			return face.close(ctx)
		},
	}
}

// composeHostFace composes the face the application component serves, in the
// one order the pieces require: a mux carrying the platform liveness routes,
// the mounted-route seed for the observability middleware's route-label
// budget (healthz and metrics, which no module registers, plus every route
// the view's registry mounted -- the last write before that middleware is
// constructed, which Start does), then the protected face composeFace
// builds over it, then the shared SPA file server when this boot serves a
// frontend directory.
func (b *serverBuild) composeHostFace(view assemblyView, face *hostFace) error {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	obs.RegisterMountedRoutes(append([]pkgcore.MountedRoute{
		{Path: obs.HealthzPath},
		{Path: obs.MetricsPath},
	}, view.routes.MountedRoutes()...))

	handler, err := b.composeFace(view, mux)
	if err != nil {
		return err
	}
	if b.cfg.WebDistDir != "" {
		handler = spa.New(b.cfg.WebDistDir, handler, hostSPAOptions()...)
	}
	face.handler = handler
	face.addr = ":" + b.cfg.Port
	return nil
}

// stepInit returns the Init callback the host's step components share: bind
// the assembled values the step bodies read, derive the assembly view from
// the registry the callback receives, and run the step's body over it. One
// adapter serves every step, so no two steps can drift into different view
// wiring.
func (b *serverBuild) stepInit(run func(ctx context.Context, view assemblyView) error) func(context.Context, *pkgcore.ComponentRegistry, any) error {
	return func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
		if err := b.bindRegistry(reg); err != nil {
			return err
		}
		if err := b.bindRuntimeServices(reg); err != nil {
			return err
		}
		view, err := viewFromComponents(reg)
		if err != nil {
			return err
		}
		return run(ctx, view)
	}
}

// stepInitWithFace returns the pre-serve step's Init callback: the same
// adapter, plus the composed face the step's own requirement has ordered
// the application component to provide.
func (b *serverBuild) stepInitWithFace(run func(ctx context.Context, view assemblyView, face *hostFace) error) func(context.Context, *pkgcore.ComponentRegistry, any) error {
	return func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
		face, err := pkgcore.Get[*hostFace](reg)
		if err != nil {
			return fmt.Errorf("reference-app: read the composed HTTP face: %w", err)
		}
		view, err := viewFromComponents(reg)
		if err != nil {
			return err
		}
		return run(ctx, view, face)
	}
}

// postBootstrapComponent returns the post-bootstrap step as a component: its
// Init binds the assembled values the step reads and runs the
// runPostBootstrap body, which publishes the two runtime services the later
// steps bind (bindRuntimeServices).
func (b *serverBuild) postBootstrapComponent() pkgcore.Component {
	return pkgcore.Component{
		Name: "reference-app.post_bootstrap",
		New:  newHostStep,
		Init: func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			if err := b.bindRegistry(reg); err != nil {
				return err
			}
			view, err := viewFromComponents(reg)
			if err != nil {
				return err
			}
			// The merged catalog is published into the by-type context
			// here: rendering a message (org's invitation mail, a
			// notification template, a verification code) reads it through
			// the registry, and this step runs before anything a request --
			// or the demo seeds below -- can render.
			reg.Put(view.catalog)
			return b.runPostBootstrap(ctx, reg, view)
		},
	}
}

// postAttachComponent returns the post-attach step as a component: its Init
// binds the assembled values the step reads and runs the runPostAttach body.
func (b *serverBuild) postAttachComponent() pkgcore.Component {
	return pkgcore.Component{
		Name: "reference-app.post_attach",
		New:  newHostStep,
		Init: b.stepInit(b.runPostAttach),
	}
}

// preServeComponent returns the pre-serve step as a component: its Init
// runs the runPreServe body over the composed face. The (*hostFace) requirement is what places it after the
// application component in the plan, so the demo seeds it registers run
// through the composed handler and its subscription lands after them.
func (b *serverBuild) preServeComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:     "reference-app.pre_serve",
		Requires: []pkgcore.Requirement{{Token: (*hostFace)(nil)}},
		New:      newHostStep,
		Init: b.stepInitWithFace(func(ctx context.Context, view assemblyView, face *hostFace) error {
			return b.runPreServe(ctx, view, face.Handler())
		}),
	}
}

// hostComponents returns the host's own components in the registration order
// the assembly plans by: the override and provider components first (their
// seams are what the modules' descriptors read while constructing), then the
// post-bootstrap step (it runs after every module's Init turn has published
// what it reads), the post-attach step, the application component, the
// pre-serve step -- ordered after the application component by its
// (*hostFace) requirement, not by registration alone -- and the worker last.
// Independent components take their plan order from the registration order
// (the assembly's own tie rule), so this order is the plan order; the module
// components' own descriptors are the composition's business, not this
// list's.
//
// baseCtx is the context the application component's served requests inherit
// (see appComponent's own doc comment), and live tells it whether this
// assembly owns the listener.
func (b *serverBuild) hostComponents(ctx context.Context, reg *pkgcore.ComponentRegistry, live bool) ([]pkgcore.Component, error) {
	wiring, err := b.hostWiringComponents(reg)
	if err != nil {
		return nil, err
	}
	return append(wiring, b.hostStepComponents(ctx, live)...), nil
}

// hostStepComponents returns the host's assembly-step components alone, in
// the registration order the assembly plans by (see hostComponents for the
// ordering rationale).
func (b *serverBuild) hostStepComponents(baseCtx context.Context, live bool) []pkgcore.Component {
	components := []pkgcore.Component{
		b.postBootstrapComponent(),
		b.postAttachComponent(),
		appComponent(b, baseCtx, live),
		b.preServeComponent(),
		b.workerComponent(),
	}
	// The steps hold no state a second replica would silently split: what
	// they publish during Init is the assembly's own context, and what they
	// start runs per replica over the shared database and queue, so a
	// distributed composition may select them (host_wiring.go's
	// replicaSafeModule has the full argument).
	for i := range components {
		components[i] = replicaSafeModule(components[i])
	}
	return components
}
