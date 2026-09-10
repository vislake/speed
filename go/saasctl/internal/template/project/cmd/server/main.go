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
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	obs "github.com/vislake/speed/go/observability"

	"__APP_NAME__/internal/hostcore"
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

	// The serve-and-drain lifecycle -- the obs.Middleware wrap, the
	// server's ReadHeaderTimeout/ShutdownTimeout values, the BaseContext
	// that hands baseCtx (never the signal-derived ctx) to every request,
	// and the graceful drain -- is the shared host kernel's
	// (internal/hostcore), byte-identical to the reference app's copy:
	// ServeUntilShutdown's own doc comment carries the full reasoning for
	// each of those choices. What stays this process's own: the logger
	// baseCtx carries, the signal context ctx, and BuildServer's
	// composition and cleanup.
	return hostcore.ServeUntilShutdown(ctx, baseCtx, handler, ":"+cfg.Port, "__APP_NAME__", string(cfg.DeploymentMode))
}
