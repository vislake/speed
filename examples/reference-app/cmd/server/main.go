package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore/probe"

	"github.com/vislake/speed/examples/reference-app/internal/app"
)

// healthcheckArg is the first os.Args element that diverts main into
// runHealthcheck instead of the ordinary server boot (run, below). It exists
// for exactly one caller: this example's Dockerfile's HEALTHCHECK
// instruction. The distroless/static runtime image that Dockerfile's final
// stage uses ships no shell, no curl and no wget -- the only executable
// inside that container is this same statically-linked binary -- so an
// exec-form HEALTHCHECK re-invokes it with this argument to probe its own
// /healthz over loopback instead. Every other invocation (`go run
// ./cmd/server`, the compiled binary with no arguments, every existing
// test) is completely unaffected, since main only takes this branch when
// os.Args[1] is exactly this string.
const healthcheckArg = "healthcheck"

// healthcheckTimeout bounds this example's own probe -- generous for a
// loopback call, but finite so a wedged server makes Docker's HEALTHCHECK
// report unhealthy rather than hang indefinitely. It is this host's policy
// value, handed to probe.WithTimeout by runHealthcheck below.
const healthcheckTimeout = 3 * time.Second

// main is deliberately thin process-lifecycle glue. The serve-and-drain
// sequence, signal handling and observability init all live in the
// application engine now (internal/app.Run over go/app's Run), so what
// remains here is the logger the process attaches at startup and the two
// seams its own tests cover directly: the healthcheck branch below and
// runHealthcheck. This file's own end-to-end behavior is additionally proven
// by literally running it and curling it (see this example's README.md).
func main() {
	if len(os.Args) > 1 && os.Args[1] == healthcheckArg {
		// The probe resolves its port through the same loader-driven
		// bootstrap the server itself boots from -- ConfigFromEnv, the call
		// run makes below -- so the two can never disagree about which port
		// this deployment listens on. The probe therefore also fails on a
		// bootstrap configuration the server itself would refuse, which is
		// the honest answer: a container whose configuration cannot load is
		// not healthy. The one fact this probe requires is cfg.Port.
		cfg, err := app.ConfigFromEnv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "reference-app: healthcheck:", err)
			os.Exit(1)
		}
		if err := runHealthcheck(context.Background(), cfg.Port); err != nil {
			fmt.Fprintln(os.Stderr, "reference-app: healthcheck:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// A JSON *slog.Logger, attached to a base context via obs.WithLogger
	// before anything else is wired: this is the one legitimate place a
	// logger gets constructed by hand (see WithLogger's own doc comment in
	// go/observability/logger.go) -- process startup, before any request
	// or trace context exists for obs.FromContext to derive one from.
	// Every log call below, throughout run and throughout the app
	// package's assembly, goes through obs.FromContext(ctx) rather than
	// touching this logger (or slog.Default()) directly, so it, and every
	// trace_id/tenant_id FromContext adds automatically once a request is
	// in flight, all land in the same structured JSON stream.
	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(baseCtx); err != nil {
		// CodeQL's go/clear-text-logging alert on this line is a false
		// positive: the traced flow is
		// authn/module.go's socialCredentialItems() -> a local anonymous
		// struct field named "secretKey" (already //nolint:gosec'd at its
		// declaration) holding a CONFIG-ITEM KEY NAME constant like
		// "authn.social.google.client_secret", not a credential value ->
		// pkgcore.ConfigItem{Key: ...} -> pkgcore.validateConfigItem, whose
		// own doc comment guarantees it names only the Key, never a
		// Sensitive item's value, in any error it returns -> up through
		// Module.Register/Kernel.Bootstrap/the engine's assembly to here.
		// CodeQL's heuristic matched the identifier "secretKey" as if it
		// held a secret; it holds a schema key name. Separately,
		// obs.FromContext's logger passes every attribute through
		// go/observability's redaction layer (redact.go), which does real
		// value-content scanning (bearer tokens, JWTs, URL userinfo/DSN
		// passwords) and is pinned by TestRedact_ErrorValues and
		// TestRedact_SecretShapesInValues -- a second, independent guard
		// even if an err ever did carry such a value. Do not "fix" this by
		// renaming secretKey or by suppressing this specific alert without
		// re-tracing the flow if module.go's struct shape changes.
		obs.FromContext(baseCtx).Error("reference-app server exited with error", "error", err)
		os.Exit(1)
	}
}

// run loads this deployment's bootstrap configuration and hands it to the
// app package's Run, which assembles the server, serves it until the
// process is signalled and drains it in the engine's fixed order. Signals,
// observability init and the graceful shutdown are all the engine's now;
// baseCtx -- carrying the logger main attached -- travels as the request
// base context, so an in-flight request's own context observes the signal
// only through the drain the engine performs, never ahead of it.
func run(baseCtx context.Context) error {
	cfg, err := app.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("reference-app: load configuration: %w", err)
	}
	return app.Run(baseCtx, cfg)
}

// runHealthcheck probes this same server's own liveness endpoint over
// loopback and reports whether it answered 200 -- see healthcheckArg's own
// doc comment for why this exists and who calls it (this example's
// Dockerfile's HEALTHCHECK, exec-form, re-invoking this binary with that
// argument rather than shelling out to a probe tool the distroless/static
// runtime image does not have).
// port is the resolved bootstrap Port: main.go's healthcheck branch loads it
// through the very ConfigFromEnv call run boots from, so the probe and the
// server agree on the port by construction. An empty port -- the shape a
// direct caller (or an explicitly emptied PORT variable) can still hand this
// function -- falls back to DefaultPort, exactly as the listener's own
// resolution does. The probe itself is pkgcore/probe's (see its Check: the
// loopback dial is structural there), healthcheckTimeout above is the
// bound this host pins on it, and the path is observability's own
// HealthzPath -- the same constant obs.MountLiveness mounts it at.
func runHealthcheck(ctx context.Context, port string) error {
	if port == "" {
		port = app.DefaultPort
	}
	return probe.Check(ctx, port, obs.HealthzPath, probe.WithTimeout(healthcheckTimeout))
}
