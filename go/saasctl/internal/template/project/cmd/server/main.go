//go:build ignore

// Package main is the generated project's minimal starter skeleton --
// exactly the kind of "minimal starter skeleton...freely editable by
// consumers" a modular-monolith-as-libraries repository's own README
// describes, not a business module. It never goes through a kernel
// Registry.Register call itself; its whole job is composing one (see
// buildServer in server.go) and running it.
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
	"time"

	obs "github.com/vislake/speed/go/observability"
)

const (
	// shutdownTimeout bounds how long graceful shutdown waits for in-flight
	// requests to finish before giving up.
	shutdownTimeout = 10 * time.Second

	// readHeaderTimeout bounds how long the server waits to receive a
	// request's headers before aborting the connection -- protects against
	// slow-header (Slowloris-style) connections that trickle bytes to hold a
	// socket open indefinitely.
	readHeaderTimeout = 5 * time.Second
)

// main is deliberately thin process-lifecycle glue (signal handling,
// http.Server start and stop) with no independently testable pure logic of
// its own -- the testable seam is buildServer (server.go), which a project's
// own test suite covers directly. It has no main_test.go for that reason,
// matching the ordinary Go practice of not unit-testing os.Exit and
// signal-handling glue.
func main() {
	// A JSON *slog.Logger, attached to a base context via obs.WithLogger
	// before anything else is wired: process startup is the one legitimate
	// place a logger gets constructed by hand (see WithLogger's own doc
	// comment in go/observability/logger.go) -- before any request or trace
	// context exists for obs.FromContext to derive one from. Every log call
	// below goes through obs.FromContext(ctx) rather than touching this
	// logger (or slog.Default()) directly, so it, and every
	// trace_id/tenant_id FromContext adds automatically once a request is in
	// flight, all land in the same structured JSON stream.
	//
	// This is deliberately a context with no cancellation of its own -- see
	// run's own doc comment on baseCtx for why it must stay separate from
	// the signal-derived context that triggers shutdown.
	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(baseCtx); err != nil {
		obs.FromContext(baseCtx).Error("__APP_NAME__ server exited with error", "error", err)
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
		return fmt.Errorf("__APP_NAME__: load configuration: %w", err)
	}

	// buildServer performs the whole composition -- the Kernel's deployment
	// mode, every seam from the Preset -- and Bootstrap's capability
	// validation of that composition runs inside it, so buildServer is the
	// one place a misconfigured one surfaces: its ErrCapabilityUnsatisfied
	// error (for example "distributed" with seams that cannot satisfy the
	// mode's RequiredCapabilities) names the seam, the implementation and
	// the shortfall. Network-class faults are the one thing assembly cannot
	// catch -- the injected Redis bus, when a project wires one, starts no
	// goroutine and touches no network until the first Subscribe -- so an
	// unreachable address passes Bootstrap and fails loudly at first use
	// instead. buildServer must run before obs.Init because Init takes no
	// deployment mode and therefore refuses none: this ordering is the only
	// place a bad composition fails before telemetry starts, and its error
	// is the accurate one. Since nothing starts listening until after both
	// calls below succeed, deferring obs.Init to second costs nothing.
	handler, cleanup, err := buildServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			obs.FromContext(ctx).Error("__APP_NAME__: cleanup failed", "error", cleanupErr)
		}
	}()

	// obs.Init's shutdown must run during graceful shutdown so buffered
	// spans and metrics are flushed rather than dropped when the process
	// exits -- the same reason srv.Shutdown below is given a bounded context
	// instead of just letting the process die.
	obsShutdown, err := obs.Init(ctx, obs.WithServiceName("__APP_NAME__"))
	if err != nil {
		return fmt.Errorf("__APP_NAME__: init observability: %w", err)
	}
	defer func() {
		if shutdownErr := obsShutdown(context.Background()); shutdownErr != nil {
			obs.FromContext(ctx).Error("__APP_NAME__: observability shutdown failed", "error", shutdownErr)
		}
	}()

	// obs.Middleware wraps OUTSIDE the middleware wiring buildServer
	// assembled (in authn-wiring compositions that inner chain is authn,
	// then tenancy -- for the full chain-order reasoning see buildServer's
	// own doc comment in server.go). Its position costs one real thing (a
	// tenant is not yet known this far out) and buys the useful one: every
	// request gets a span and is counted here, including ones the inner
	// chain goes on to reject with 401/403, which matters for spotting a
	// flood of them.
	instrumented := obs.Middleware(handler)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           instrumented,
		ReadHeaderTimeout: readHeaderTimeout,
		// BaseContext hands baseCtx -- not ctx -- as the ancestor of every
		// incoming request's context, so obs.FromContext(r.Context())
		// inside a handler finds the JSON logger main attached via
		// obs.WithLogger, in addition to the trace_id/tenant_id the
		// middleware layers add per request. Without this, net/http
		// defaults every request's root context to a bare
		// context.Background(), and the logger attachment above would never
		// reach request-handling code at all -- see run's own doc comment
		// for why baseCtx, specifically not the signal-aware ctx, is the
		// one to use here.
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		obs.FromContext(ctx).Info("__APP_NAME__ server listening", "addr", srv.Addr, "deployment_mode", string(cfg.DeploymentMode))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		obs.FromContext(ctx).Info("__APP_NAME__: shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("__APP_NAME__: serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("__APP_NAME__: graceful shutdown: %w", err)
	}
	obs.FromContext(ctx).Info("__APP_NAME__: server stopped cleanly")
	return nil
}
