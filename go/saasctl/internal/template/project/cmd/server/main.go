//go:build ignore

// Package main is the generated project's minimal starter skeleton --
// exactly the kind of "minimal starter skeleton...freely editable by
// consumers" a modular-monolith-as-libraries repository's own README
// describes, not a business module. It never registers itself as a
// component; its whole job is composing the host's component set (see the
// selection's server.go) and running it.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	obs "github.com/vislake/speed/go/observability"
	// Blank-imported for its init side effect: registers the OTLP exporter
	// factory the engine's observability init composes when
	// APP_OTLP_ENDPOINT is set. Without this import, setting the endpoint
	// fails that init with an error naming the missing import; a deployment
	// that removes this import must also stop setting APP_OTLP_ENDPOINT
	// (and may drop the endpoint from the observability component's
	// configuration in server.go):
	// the two are one wiring, split only because go/observability keeps its
	// OTLP dependencies -- gRPC and protobuf -- out of the package a
	// consumer imports for the exporters it does want.
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
	// Blank-imported for its init side effect: registers the local metrics
	// reader this project's /metrics route serves through
	// obs.MetricsHandler. Without this import /metrics answers 404, which
	// is go/observability's documented default for a host that never opted
	// into the Prometheus exporter; with it the route serves this
	// process's real scrape output while no OTLP endpoint is set. (Once
	// APP_OTLP_ENDPOINT selects the OTLP exporters, /metrics answers 404
	// again -- by design, since no local registry is being kept to
	// scrape; the OTLP collector is where metrics go then.)
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"
)

// main is deliberately thin process-lifecycle glue: it attaches the
// process's logger and hands the loaded bootstrap configuration to the
// selection's own assembly (cmd/server/server.go), whose runServer hands
// the host's components and its serve step to the engine's RunAssembly:
// the whole set assembles, the application component serves until the
// signal, and the engine then shuts down in two phases
// (github.com/vislake/speed/go/app). It has no main_test.go for that
// reason: the testable seam is the composition in server.go, which the
// repository's own scaffold integration test boots end to end, and the
// process-lifecycle glue left here is the ordinary "do not unit-test
// os.Exit" shape.
func main() {
	// A JSON *slog.Logger, attached to a base context via obs.WithLogger
	// before anything else is wired: process startup is the one legitimate
	// place a logger gets constructed by hand (see WithLogger's own doc
	// comment in go/observability/logger.go) -- before any request or trace
	// context exists for obs.FromContext to derive one from. Every log call
	// below, and every one the engine's own assembly and drain emit, goes
	// through obs.FromContext(ctx) rather than touching this logger (or
	// slog.Default()) directly, so it, and every trace_id/tenant_id
	// FromContext adds automatically once a request is in flight, all land
	// in the same structured JSON stream.
	//
	// This is deliberately a context with no cancellation of its own --
	// the engine's RunAssembly overlays the shutdown signals on it, and the
	// http component's Serve stage hands its listener a request base
	// context decoupled from that cancellation (context.WithoutCancel), so
	// a shutdown signal never cancels in-flight requests ahead of the
	// graceful drain.
	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(baseCtx); err != nil {
		obs.FromContext(baseCtx).Error("__APP_NAME__ server exited with error", "error", err)
		os.Exit(1)
	}
}

// run loads this deployment's bootstrap configuration -- the loader target
// the assembly hands to the engine as its configuration targets, and the
// resolved host-config transform on top of it -- and runs the composed
// server until the process is signalled.
func run(baseCtx context.Context) error {
	cfg, hc, err := configFromEnv()
	if err != nil {
		return fmt.Errorf("__APP_NAME__: load configuration: %w", err)
	}
	return runServer(baseCtx, cfg, hc)
}
