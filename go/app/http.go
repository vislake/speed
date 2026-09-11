package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// HTTPSpec declares the host's half of the HTTP face and the address Run
// listens on. Everything in it is host policy; the order the pieces compose
// in is the engine's (see buildHandler).
type HTTPSpec struct {
	// Addr is the listen address ("host:port" or ":port") Run binds. Run
	// refuses an empty address rather than binding a random port.
	Addr string

	// Middleware is the host's own middleware chain, outermost first: the
	// composed chain wraps the mux carrying the liveness routes, every
	// module route and ExtraRoutes, and the first entry listed here is the
	// one a request reaches first. A chain that needs the mux itself (the
	// platform middleware chain is built over the protected handler, not
	// over a bare next-handler) states that with a closure: the entry
	// receives the already-composed handler and returns the chain built
	// around it.
	Middleware []func(http.Handler) http.Handler

	// ExtraRoutes are the host's hand-written routes, mounted beside the
	// module routes exactly as a module's own routes are (pkgcore.MountRoutes
	// semantics: the handler serves Path and everything below it). A route
	// conflicting with one already mounted panics at assembly time, the
	// loudest report a wiring error can get.
	ExtraRoutes []pkgcore.MountedRoute

	// SPA, when non-nil, serves a built single-page application at the
	// outermost layer of the host's own composition: requests the frontend
	// answers never reach the middleware chain or the mux, while everything
	// the frontend does not answer falls through unchanged (pkgcore/spa's
	// interception rules). It wraps outside Middleware, inside the
	// observability middleware every request is counted at.
	SPA *SPASpec
}

// SPASpec declares a single-page-application directory to serve: the
// directory holding the built frontend, and the options go/pkgcore/spa is
// built with.
type SPASpec struct {
	// Dir is the directory holding the built frontend's index.html and
	// assets.
	Dir string

	// Options customise the spa.Handler. The host declares the composed
	// server's own surface through them (spa.WithServerPrefix("/api"),
	// spa.WithServerPath(obs.HealthzPath) and the rest of the set): the
	// frontend answers every GET/HEAD path a declared option does not claim,
	// so a route left undeclared is answered from disk instead of reaching
	// the handler the engine mounted for it.
	Options []spa.Option
}

// WithHTTP declares the HTTP face the engine composes in stage 7 and the
// address Run serves it on.
func WithHTTP(spec HTTPSpec) Option {
	return func(c *engineConfig) {
		s := spec
		c.httpSpec = &s
	}
}

// buildHandler is stage 7: the fixed HTTP assembly order.
//
// A mux carries the platform liveness routes and every route the registry's
// modules mounted; the host's ExtraRoutes are mounted beside them; the
// registered routes are seeded into the observability middleware's
// route-label budget BEFORE that middleware is built (the seed is a snapshot
// consumed at construction, and without it a burst of garbage paths could
// exhaust the budget before a real route is ever requested); the host's
// middleware chain wraps outside-in; the SPA wraps outside that when one is
// configured; and obs.Middleware wraps the whole composition -- its position
// costs nothing here and counts every request, including the ones an inner
// layer rejects.
//
// The composed http.Server is created here, not yet listening: Run serves it,
// and Close drains it.
func (a *Application) buildHandler(cfg *engineConfig) error {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	pkgcore.MountRoutes(mux, a.registry.Routes.Routes()...)
	RegisterMountedRoutes(a.registry)

	var spec HTTPSpec
	if cfg.httpSpec != nil {
		spec = *cfg.httpSpec
	}
	pkgcore.MountRoutes(mux, spec.ExtraRoutes...)

	var handler http.Handler = mux
	for i := len(spec.Middleware) - 1; i >= 0; i-- {
		if spec.Middleware[i] == nil {
			continue
		}
		handler = spec.Middleware[i](handler)
	}
	if spec.SPA != nil && spec.SPA.Dir != "" {
		handler = spa.New(spec.SPA.Dir, handler, spec.SPA.Options...)
	}
	a.handler = obs.Middleware(handler)

	a.server = &http.Server{
		Addr:              spec.Addr,
		Handler:           a.handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return a.baseCtx },
	}
	return nil
}

// serve runs the composed server until ctx is done or the listener fails,
// then drains through the ordered shutdown. It is the tail of Run, which
// refuses an empty address before anything is assembled: the caller passes
// the signal-derived context, and the server's request base context stays the
// uncancelled one Run received, so a shutdown signal never cancels in-flight
// requests ahead of the drain.
func (a *Application) serve(ctx context.Context) error {
	srv := a.server

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
			// The listener never came up: drain what this process built and
			// report the listener's own failure.
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
