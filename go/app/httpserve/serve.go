package httpserve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"

	"github.com/vislake/speed/go/app/chain"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// serve.go carries the Serve-stage assembly and the two-beat shutdown: the
// one place the accumulated declarations become a serving process.

// serveFace is the component's Serve callback -- the entry-point round that
// runs after every component's Start callback. It reads the accumulated
// routes and middleware, assembles the handler per the host's link policy
// (fixed chain or chainless, platform middleware outermost of the chain,
// the host's outer wrapper outside everything), and opens the listener
// unless the configuration declares compose-only.
func serveFace(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
	face, ok := instance.(*Face)
	if !ok {
		return fmt.Errorf("httpserve: the component holds an instance of type %T", instance)
	}
	return face.serve(ctx)
}

// serve assembles the face and, unless compose-only, binds the listener.
//
// The assembly order is the one the pieces require, and it is unchanged from
// the composition hosts used to hand-write:
//
//  1. a mux carrying the platform liveness routes (observability's
//     MountLiveness; healthz and metrics answer without a tenant),
//  2. the mounted-route seed for the observability middleware's route-label
//     budget (the liveness paths, which no module registers, plus every
//     route the assembly mounted) -- written before the middleware below is
//     constructed,
//  3. the host's own routes (policy.MountHostRoutes) on the same mux,
//  4. the fixed chain (chain.Standard, which admits the mounted routes
//     through the authorization table, splits the authn and admin subtrees
//     out and wraps tenancy around the mux), or -- for a chainless policy --
//     the mux itself,
//  5. the platform middleware declared through the middleware face,
//     outermost of the chain (applied by Standard for the guarded form,
//     here for the chainless one),
//  6. the host's outer wrapper (policy.OuterWrapper) outside everything.
//
// The listener's request base context is deliberately detached from the
// serve context's cancellation (context.WithoutCancel): a shutdown signal
// must never cancel in-flight requests ahead of the drain net/http's
// Shutdown performs -- it may keep the context's values.
func (f *Face) serve(ctx context.Context) error {
	if err := f.policy.validate(); err != nil {
		return err
	}

	routes := f.Routes()
	middlewares := f.Middlewares()

	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	obs.RegisterMountedRoutes(append([]pkgcore.MountedRoute{
		{Path: obs.HealthzPath},
		{Path: obs.MetricsPath},
	}, routes...))
	if f.policy.MountHostRoutes != nil {
		f.policy.MountHostRoutes(mux)
	}

	var handler http.Handler
	if f.policy.Chainless {
		// No fixed chain: the accumulated routes are mounted on the
		// protected mux directly (the guarded branch hands them to
		// Standard's derivation instead), and the platform middleware wraps
		// the mux itself -- the same layer position Standard gives it.
		pkgcore.MountRoutes(mux, routes...)
		handler = mux
		for _, mw := range slices.Backward(middlewares) {
			handler = mw(handler)
		}
	} else {
		composed, err := chain.Standard(f, f.policy.Verifier, mux, f.policy.chainOptions()...)
		if err != nil {
			return fmt.Errorf("httpserve: compose the middleware chain: %w", err)
		}
		handler = composed
	}
	if f.policy.OuterWrapper != nil {
		handler = f.policy.OuterWrapper(handler)
	}

	f.mu.Lock()
	f.handler = handler
	f.addr = f.cfg.Addr
	f.mu.Unlock()

	if !f.cfg.Listen {
		return nil
	}

	server := &http.Server{
		Addr:              f.cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: f.cfg.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	listener, err := net.Listen("tcp", f.cfg.Addr)
	if err != nil {
		return fmt.Errorf("httpserve: listen on %s: %w", f.cfg.Addr, err)
	}

	f.mu.Lock()
	f.server = server
	f.listener = listener
	f.addr = listener.Addr().String()
	f.listening = true
	f.drained = make(chan struct{})
	f.mu.Unlock()

	obs.FromContext(ctx).Info("server listening", "addr", listener.Addr().String())
	go func() {
		// net.ErrClosed is the expected shape of every shutdown: the two
		// beats close the listener themselves (stopAccepting), so Serve
		// returns that error rather than http.ErrServerClosed.
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			obs.FromContext(ctx).Error("the server stopped serving", "error", err)
		}
	}()
	return nil
}

// stopFace is the component's Stop callback, which the registry drives in
// the first beat -- the components that declared Serve stop accepting new
// requests before any other component is notified, so the requests in
// flight drain against a still-complete system.
func stopFace(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
	face, ok := instance.(*Face)
	if !ok {
		return fmt.Errorf("httpserve: the component holds an instance of type %T", instance)
	}
	face.stop(ctx)
	return nil
}

// stop is the first beat's half of shutdown, and it keeps that beat's
// promise synchronously: the listener stops accepting before Stop returns
// -- the close happens here, never inside the drain goroutine, whose
// scheduling must not widen the window in which a new connection can still
// enter a system that has begun stopping. The in-flight drain continues in
// the background, bounded by the shutdown timeout, and close waits it out.
// A face that never listened is a no-op, so Stop is safe before Serve.
func (f *Face) stop(ctx context.Context) {
	f.mu.Lock()
	server := f.server
	drained := f.drained
	listener := f.listener
	f.mu.Unlock()
	if server == nil || drained == nil {
		return
	}
	f.stopOnce.Do(func() {
		stopAccepting(listener)
		go func() {
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), f.cfg.ShutdownTimeout)
			defer cancel()
			drainErr := shutdownServer(server, drainCtx)
			f.mu.Lock()
			f.drainErr = drainErr
			f.mu.Unlock()
			close(drained)
		}()
	})
}

// shutdownServer runs the graceful drain and normalizes its one expected
// report: stopAccepting has already closed the listener, so Shutdown's own
// close of it fails with net.ErrClosed -- an error naming this component's
// deliberate close, not a drain failure. Anything else is a real one.
func shutdownServer(server *http.Server, ctx context.Context) error {
	if err := server.Shutdown(ctx); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// stopAccepting closes the listener so the socket refuses new connections.
// Both shutdown beats call it (the first beat's Stop, and Close when no
// drain has run), so it must tolerate a face that never bound a listener
// and a listener already closed -- the drain paths, and a repeated
// Shutdown, may close it more than once.
func stopAccepting(listener net.Listener) {
	if listener == nil {
		return
	}
	_ = listener.Close()
}

// closeFace is the component's Close callback: the blocking half.
func closeFace(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
	face, ok := instance.(*Face)
	if !ok {
		return fmt.Errorf("httpserve: the component holds an instance of type %T", instance)
	}
	return face.close(ctx)
}

// close reports the drain stop began, waiting it out bounded by the
// shutdown timeout on top of the caller's context. A face whose drain has
// not finished -- Close before Stop, or a drain still in flight -- stops
// accepting and waits out the in-flight requests here, synchronously and
// bounded the same way; a face that never listened releases nothing.
func (f *Face) close(ctx context.Context) error {
	f.mu.Lock()
	server := f.server
	drained := f.drained
	listener := f.listener
	f.mu.Unlock()
	if server == nil {
		return nil
	}
	if drained != nil {
		select {
		case <-drained:
			f.mu.Lock()
			drainErr := f.drainErr
			f.mu.Unlock()
			if drainErr != nil {
				return fmt.Errorf("httpserve: shut the HTTP server down: %w", drainErr)
			}
			return nil
		default:
		}
	}

	// No drain has finished (Close before Stop, or a drain still in
	// flight): stop accepting here, then wait the in-flight requests out.
	stopAccepting(listener)
	drainCtx, cancel := context.WithTimeout(ctx, f.cfg.ShutdownTimeout)
	defer cancel()
	if err := shutdownServer(server, drainCtx); err != nil {
		return fmt.Errorf("httpserve: shut the HTTP server down: %w", err)
	}
	return nil
}

// The compile-time assertions pin the product's three consumer contracts:
// the two declaration faces the component provides, and the chain-derived
// reading (chain.RouteSource) the fixed chain's assembly takes.
var (
	_ pkgcore.RouteRegistrar      = (*Face)(nil)
	_ pkgcore.MiddlewareRegistrar = (*Face)(nil)
	_ chain.RouteSource           = (*Face)(nil)
)
