package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	obs "github.com/vislake/speed/go/observability"
)

// main is deliberately thin process-lifecycle glue (signal handling,
// http.Server start/stop) with no independently testable pure logic of its
// own -- the testable seam is buildServer (server.go), which
// server_test.go covers directly, and this file's own end-to-end behavior
// is additionally proven by literally running it and curling it (see this
// example's README.md). It has no main_test.go for that reason, matching
// ordinary Go practice of not unit-testing os.Exit/signal-handling glue.
func main() {
	// A JSON *slog.Logger, attached to a base context via obs.WithLogger
	// before anything else is wired: this is the one legitimate place a
	// logger gets constructed by hand (see WithLogger's own doc comment in
	// go/observability/logger.go) -- process startup, before any request
	// or trace context exists for obs.FromContext to derive one from.
	// Every log call below and throughout run and server.go goes through
	// obs.FromContext(ctx) rather than touching this logger (or
	// slog.Default()) directly, so it, and every trace_id/tenant_id
	// FromContext adds automatically once a request is in flight, all
	// land in the same structured JSON stream.
	//
	// This is deliberately a context with no cancellation of its own --
	// see run's own doc comment on baseCtx for why it must stay separate
	// from the signal-derived context used to trigger shutdown.
	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(baseCtx); err != nil {
		obs.FromContext(baseCtx).Error("reference-app server exited with error", "error", err)
		os.Exit(1)
	}
}

// run takes baseCtx -- carrying the logger main attached via obs.WithLogger
// -- rather than building one internally, so that srv.BaseContext below can
// hand it, uncancelled, to every incoming request: BaseContext is
// deliberately NOT the signal-derived ctx immediately below, because if it
// were, every in-flight request's own context would already be Done() the
// instant a shutdown signal arrived, which would race handler code that
// checks ctx.Err() against the graceful drain srv.Shutdown is supposed to
// perform. ctx, by contrast, is exactly the context that SHOULD observe
// that cancellation: it exists to drive this function's own
// shutdown-orchestration select below, not to flow into request handling.
func run(baseCtx context.Context) error {
	ctx, stop := signal.NotifyContext(baseCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := configFromEnv()
	if err != nil {
		return fmt.Errorf("reference-app: load configuration: %w", err)
	}

	// buildServer runs, and can reject an unsupported deployment mode,
	// before obs.Init: buildServer's own error ("deployment mode %q is
	// not wired in this example yet") is the specific diagnostic for this
	// example's actual limitation (root CLAUDE.md's M0 status -- only
	// the standalone deployment mode has business wiring at all), the
	// same limitation obs.Init itself used to double-check. It no longer
	// can: Init takes no deployment mode and therefore refuses none, so
	// this ordering is the only place a misconfigured
	// SPEED_DEPLOYMENT_MODE surfaces, and its message is the accurate
	// one. Since nothing starts listening until after both calls below
	// succeed, deferring obs.Init to second costs nothing.
	handler, cleanup, err := buildServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			obs.FromContext(ctx).Error("reference-app: cleanup failed", "error", cleanupErr)
		}
	}()

	// obs.Init's shutdown must run during graceful shutdown so buffered
	// spans and metrics are flushed rather than dropped when the process
	// exits -- the same reason srv.Shutdown below is given a bounded
	// context instead of just letting the process die.
	obsShutdown, err := obs.Init(ctx, obs.WithServiceName("reference-app"))
	if err != nil {
		return fmt.Errorf("reference-app: init observability: %w", err)
	}
	defer func() {
		if shutdownErr := obsShutdown(context.Background()); shutdownErr != nil {
			obs.FromContext(ctx).Error("reference-app: observability shutdown failed", "error", shutdownErr)
		}
	}()

	// obs.Middleware wraps OUTSIDE buildServer's own authn+tenancy
	// middleware wiring.
	//
	// docs/internal/01-architecture.md's originally documented chain order
	// is recover -> request-id/log-context -> observability ->
	// tenancy.Middleware -> authn.Middleware -> rbac.RequirePermission ->
	// handler. buildServer (server.go) deliberately runs authn.Middleware
	// BEFORE tenancy.Middleware instead -- see its own doc comment on the
	// handler chain, go/authn/AGENTS.md's "The middleware chain is authn, then tenancy" section,
	// and docs/internal/01-architecture.md's own implementation-status
	// note for the full reasoning (a tenancy.Resolver cannot carry a
	// verified JWT's claims to anything downstream, so the documented
	// order would force verifying every token twice). obs.Middleware's own
	// position relative to that pair is unaffected: tenancy.Middleware's
	// doc comment (go/tenancy/middleware.go) says nothing about tracing
	// middleware specifically, so it wraps outermost regardless of which
	// of authn/tenancy runs first inside it. See obs.Middleware's own doc
	// comment for why this position is worth its one real cost (a tenant
	// is not yet known this far out -- see obs.AnnotateTenant, called from
	// notes.Handler once tenancy.Middleware has resolved one, for how
	// tenant_id still reaches the span from there): every request gets a
	// span and is counted here, including ones the inner chain goes on to
	// reject with 401/403, which matters for spotting a flood of them.
	instrumented := obs.Middleware(handler)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           instrumented,
		ReadHeaderTimeout: readHeaderTimeout,
		// BaseContext hands baseCtx -- not ctx -- as the ancestor of every
		// incoming request's context, so obs.FromContext(r.Context())
		// inside a handler finds the JSON logger main attached via
		// obs.WithLogger, in addition to the trace_id/tenant_id
		// obs.Middleware and tenancy.Middleware each add per request.
		// Without this, net/http defaults every request's root context to
		// a bare context.Background(), and the logger attachment above
		// would never reach request-handling code at all -- see run's own
		// doc comment for why baseCtx, specifically not the signal-aware
		// ctx, is the one to use here.
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		obs.FromContext(ctx).Info("reference-app server listening", "addr", srv.Addr, "deployment_mode", string(cfg.DeploymentMode))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		obs.FromContext(ctx).Info("reference-app: shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("reference-app: serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("reference-app: graceful shutdown: %w", err)
	}
	obs.FromContext(ctx).Info("reference-app: server stopped cleanly")
	return nil
}
