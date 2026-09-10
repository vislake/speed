package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	speedapp "github.com/vislake/speed/go/app"
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

// observabilityOptions assembles the options run passes to obs.Init from
// the bootstrap configuration ConfigFromEnv already resolved: the service
// name always, plus obs.WithOTLPEndpoint exactly when cfg.OTLPEndpoint is
// non-empty (see the OTLPEndpoint bootstrap field's own doc comment in
// internal/app/bootstrap.go for what
// the variable changes). The endpoint option is conditional rather than
// unconditional because WithOTLPEndpoint("") and its absence are
// deliberately different in meaning: supplying an explicitly empty option
// is indistinguishable from an unset one, but the branch also keeps the
// no-endpoint default free of an option that has nothing to say -- the
// same shape cfg.RedisAddr's conditional composition follows. It is a
// separate, package-level function rather than inline in run so
// main_test.go can pin the conditional without invoking Init (Init with a
// non-empty endpoint builds real OTLP exporters -- the exporter/otlp
// blank import in internal/app/server.go registers the factory -- which is behaviour
// for a real boot, not a unit test).
func observabilityOptions(cfg app.ServerConfig) []obs.Option {
	opts := []obs.Option{obs.WithServiceName("reference-app")}
	if cfg.OTLPEndpoint != "" {
		opts = append(opts, obs.WithOTLPEndpoint(cfg.OTLPEndpoint))
	}
	return opts
}

// main is deliberately thin process-lifecycle glue (signal handling,
// http.Server start/stop) with two independently testable seams beyond
// BuildServer (internal/app/server.go, covered by flowtests/server_test.go): observabilityOptions
// above and runHealthcheck below, both covered directly by main_test.go.
// This file's own end-to-end behavior is additionally proven by literally
// running it and curling it (see this example's README.md), which is why
// the ordinary boot path (run) still has no test of its own, matching
// ordinary Go practice of not unit-testing os.Exit/signal-handling glue.
func main() {
	if len(os.Args) > 1 && os.Args[1] == healthcheckArg {
		// The probe resolves its port through the same loader-driven
		// bootstrap the server itself boots from -- ConfigFromEnv, the call
		// run makes below -- so the two can never disagree about which port
		// this deployment listens on. The probe therefore also fails on a
		// bootstrap configuration the server itself would refuse, which is
		// the honest answer: a container whose configuration cannot load is
		// not healthy. The one fact this probe requires is cfg.Port;
		// runHealthcheck's own doc comment covers the rest.
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
	// package's assembly (internal/app/server.go) goes through
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
		// CodeQL's go/clear-text-logging alert on this line: reviewed and
		// confirmed a false positive. The traced flow is
		// authn/module.go's socialCredentialItems() -> a local anonymous
		// struct field named "secretKey" (already //nolint:gosec'd at its
		// declaration) holding a CONFIG-ITEM KEY NAME constant like
		// "authn.social.google.client_secret", not a credential value ->
		// pkgcore.ConfigItem{Key: ...} -> pkgcore.validateConfigItem, whose
		// own doc comment guarantees it names only the Key, never a
		// Sensitive item's value, in any error it returns -> up through
		// Module.Register/Kernel.Bootstrap/BuildServer/run to here. CodeQL's
		// heuristic matched the identifier "secretKey" as if it held a
		// secret; it holds a schema key name. Separately, obs.FromContext's
		// logger passes every attribute through go/observability's
		// redaction layer (redact.go), which does real value-content
		// scanning (bearer tokens, JWTs, URL userinfo/DSN passwords) and is
		// pinned by TestRedact_ErrorValues and
		// TestRedact_SecretShapesInValues -- a second, independent guard
		// even if an err ever did carry such a value. Do not "fix" this
		// by renaming secretKey or by suppressing this specific alert
		// without re-tracing the flow if module.go's struct shape changes.
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

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("reference-app: load configuration: %w", err)
	}

	// BuildServer performs the whole composition -- the Kernel's
	// deployment mode, the optional Redis-backed EventBus APP_REDIS_ADDR
	// requests, every other seam from the Preset -- and Bootstrap's
	// capability validation of that composition runs inside it, so it is
	// the one place a misconfigured one surfaces: its ErrCapabilityUnsatisfied
	// error (e.g. "distributed" with no APP_REDIS_ADDR) names the seam
	// and the shortfall. Network-class faults are the one thing assembly
	// cannot catch -- RedisEventBus starts no goroutine and touches no
	// network until the first Subscribe, so an unreachable APP_REDIS_ADDR
	// passes Bootstrap and fails loudly at first use instead. BuildServer
	// must run before obs.Init because Init takes no deployment mode and
	// therefore refuses none: this ordering is the only place a bad
	// composition fails before telemetry starts, and its error is the
	// accurate one. Since
	// nothing starts listening until after both calls below succeed,
	// deferring obs.Init to second costs nothing.
	// BuildServer's fourth return value, the wired *compliance.Module, is
	// the reach flowtests/compliance_flow_test.go needs into the retention/erasure/
	// export services; the real process has nothing to do with it -- the
	// compliance module's own registered jobs handler drives its sweep on
	// the queue this app starts below.
	handler, cleanup, _, err := app.BuildServer(ctx, cfg)
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
	// context instead of just letting the process die. The option set is
	// observabilityOptions(cfg): an APP_OTLP_ENDPOINT resolved by
	// ConfigFromEnv is handed over through obs.WithOTLPEndpoint, the
	// APP_REDIS_ADDR-shaped wiring that decides between the OTLP exporters
	// (exporter/otlp blank-imported in internal/app/server.go) and the local ones.
	obsShutdown, err := obs.Init(ctx, observabilityOptions(cfg)...)
	if err != nil {
		return fmt.Errorf("reference-app: init observability: %w", err)
	}
	defer func() {
		if shutdownErr := obsShutdown(context.Background()); shutdownErr != nil {
			obs.FromContext(ctx).Error("reference-app: observability shutdown failed", "error", shutdownErr)
		}
	}()

	// obs.Middleware wraps OUTSIDE BuildServer's own authn+tenancy
	// middleware wiring.
	//
	// The chain BuildServer composes deliberately runs authn.Middleware
	// before tenancy.Middleware (see its own doc comment on the handler
	// chain for why: a tenancy.Resolver cannot carry a verified JWT's
	// claims to anything downstream, so the reverse order would force
	// verifying every token twice). obs.Middleware's own
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
	//
	// app.ServeUntilShutdown owns the whole serve-and-drain sequence
	// (the obs.Middleware wrap included, at exactly this position) plus
	// the server's ReadHeaderTimeout/ShutdownTimeout values, so this
	// process shell and a generated project's run share the host-neutral
	// half: see its doc comment for the BaseContext/baseCtx
	// reasoning this call's arguments carry.
	return speedapp.ServeUntilShutdown(ctx, baseCtx, handler, ":"+cfg.Port, "reference-app", string(cfg.DeploymentMode))
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
