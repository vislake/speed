package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// legacy_http.go carries the transition HTTP face: the component that
// composes the host's handler from the declaration seats during Start and
// owns the listener's non-blocking stop and bounded drain in Stop and Close.
// The engine's core (the driver and the loader) contains no HTTP assembly
// and no listening; this file is the temporary adapter that keeps the old
// option surface serving until the hosts compose their own application
// component.
//
// The pieces that are not transition-specific stay reusable helpers beside
// it -- chain.Chain, chain.Standard, PreAuthAllowlist, RegisterMountedRoutes
// and the serve timeouts (kernel.go) -- so a host's own application component
// composes the same face from the same pieces.

// legacyHTTPState carries what the transition HTTP component composes and
// drains: the handler and the server the Start callback builds, the
// application they belong to, and the drain the Stop callback begins.
type legacyHTTPState struct {
	app *Application

	handler http.Handler
	server  *http.Server

	// drained closes when the asynchronous drain Stop began has finished;
	// drainErr carries its result. Both are written before the close, so a
	// reader that sees the closed channel sees the error.
	drained  chan struct{}
	drainErr error

	stopOnce sync.Once
}

// legacyHTTPComponent returns the transition HTTP component. The face is
// composed in the Init stage -- the plan places the component after the
// post-attach step and before the pre-serve step, which is the old order,
// and a host's face composition that installs an event subscription (the
// reference app's demo glue does) declares while the seats are still open.
// Stop begins the non-blocking drain, Close waits it out bounded by the
// shutdown timeout, and the listener itself is started by Run's serve step.
func legacyHTTPComponent(cfg *engineConfig, registry *pkgcore.Registry, app *Application) (pkgcore.Component, *legacyHTTPState) {
	state := &legacyHTTPState{app: app}
	return pkgcore.Component{
		Name: legacyComponentPrefix + "http",
		New:  newTransitionMarker,
		Init: func(context.Context, *pkgcore.ComponentRegistry, any) error {
			return composeLegacyFace(cfg, registry, state)
		},
		Stop: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			state.beginDrain(ctx)
			return nil
		},
		Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
			return state.waitDrain(ctx)
		},
	}, state
}

// composeLegacyFace is the fixed HTTP assembly order the old HTTP stage
// encoded: a mux carrying the platform liveness routes, the host's
// ExtraRoutes, and -- unless the host composes its own protected face
// through Compose -- every route the module registry mounted; the registered
// routes seeded into the observability middleware's route-label budget
// BEFORE the middleware is constructed (the seed is a snapshot consumed at
// construction); the host's middleware entries wrapping outside-in; and the
// SPA wrapping outside that. The composed http.Server is created here, not
// yet listening: Run serves it and the component's Close drains it.
func composeLegacyFace(cfg *engineConfig, registry *pkgcore.Registry, state *legacyHTTPState) error {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	if registry != nil {
		RegisterMountedRoutes(registry)
	}

	var spec HTTPSpec
	if cfg.httpSpec != nil {
		spec = *cfg.httpSpec
	}
	pkgcore.MountRoutes(mux, spec.ExtraRoutes...)

	var handler http.Handler = mux
	if spec.Compose != nil {
		composed, err := spec.Compose(mux)
		if err != nil {
			return fmt.Errorf("app: compose the protected face: %w", err)
		}
		if composed == nil {
			return errors.New("app: Compose returned a nil handler")
		}
		handler = composed
	} else if registry != nil {
		pkgcore.MountRoutes(mux, registry.MountedRoutes()...)
	}
	for i := len(spec.Middleware) - 1; i >= 0; i-- {
		if spec.Middleware[i] == nil {
			continue
		}
		handler = spec.Middleware[i](handler)
	}
	if spec.SPA != nil && spec.SPA.Dir != "" {
		handler = spa.New(spec.SPA.Dir, handler, spec.SPA.Options...)
	}

	state.handler = handler
	state.drained = make(chan struct{})
	state.server = &http.Server{
		Addr:              spec.Addr,
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return state.app.baseCtx },
	}
	state.app.handler = handler
	state.app.server = state.server
	return nil
}

// beginDrain is the Stop half: the non-blocking stop notification. It
// detaches a goroutine that stops the listener from accepting and waits out
// the in-flight requests, bounded by the shutdown timeout; Stop itself
// returns immediately, and Close waits for the goroutine's result.
func (s *legacyHTTPState) beginDrain(ctx context.Context) {
	if s.server == nil || s.drained == nil {
		return
	}
	s.stopOnce.Do(func() {
		go func() {
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownTimeout)
			defer cancel()
			s.drainErr = s.server.Shutdown(drainCtx)
			close(s.drained)
		}()
	})
}

// waitDrain is the Close half: it waits for the drain, bounded by the
// shutdown timeout on top of the caller's context. A server whose drain
// never began (Stop never ran -- an assembly that failed before Start, or a
// caller that closes a New-assembled application directly) is drained here
// synchronously; a server that never listened shuts down immediately.
func (s *legacyHTTPState) waitDrain(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	if s.drained == nil {
		drainCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("app: shut the HTTP server down: %w", err)
		}
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
	defer cancel()
	select {
	case <-s.drained:
		if s.drainErr != nil {
			return fmt.Errorf("app: shut the HTTP server down: %w", s.drainErr)
		}
		return nil
	case <-waitCtx.Done():
		_ = s.server.Close()
		return fmt.Errorf("app: shut the HTTP server down: %w", waitCtx.Err())
	}
}

// serve runs the composed server until ctx is done or the listener fails,
// then shuts the assembly down through the component lifecycle. It is the
// tail of Run, which refuses an empty address before anything is assembled:
// the caller passes the signal-derived context, and the server's request
// base context stays the uncancelled one Run received, so a shutdown signal
// never cancels in-flight requests ahead of the drain.
//
// obs.Middleware is applied here, at serve time: it is the outermost layer a
// served request reaches (counting every request, including the ones an
// inner layer rejects), and applying it here is what keeps
// Application.Handler the host's own composition -- a host that serves a
// New-composed handler itself decides its own instrumentation instead of
// paying for a second layer.
func (a *Application) serve(ctx context.Context) error {
	srv := a.server
	srv.Handler = obs.Middleware(srv.Handler)

	serveErr := make(chan error, 1)
	go func() {
		obs.FromContext(ctx).Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		obs.FromContext(ctx).Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			// The listener never came up: tear the assembly down and report
			// the listener's own failure.
			if closeErr := a.Close(context.Background()); closeErr != nil {
				obs.FromContext(ctx).Error("shutdown after a serve failure failed", "error", closeErr)
			}
			return fmt.Errorf("app: serve: %w", err)
		}
	}

	if err := a.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	obs.FromContext(ctx).Info("server stopped cleanly")
	return nil
}
