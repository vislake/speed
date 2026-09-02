// Package observability wires the structured logging, tracing, and metrics
// shared across every speed module, per docs/internal/09-observability.md.
//
// # Scope of this package today
//
// This is the foundational layer only: dual-deployment-mode OTel
// initialization (Init), a context-aware structured logger (FromContext /
// WithLogger) with default-on secret-attribute redaction, and generic HTTP
// instrumentation (Middleware). The full
// per-domain metrics catalog docs/internal/09-observability.md's
// must-instrument-metrics table describes (queue depth, metering outbox
// lag, notification delivery rate, payment callback success, ...) belongs
// to the modules that own those domains (jobs, metering, notification,
// billing-gateway, ai-gateway), none of which exist yet (root CLAUDE.md's
// M0 status) -- this package does not speculatively build instrumentation
// for them.
//
// # Deployment modes
//
// Like every other infrastructure dependency in speed, the export target is
// selected once, at startup, by Init, and never branched on again by
// business code (root CLAUDE.md's "do not branch on deployment mode" rule
// applies here exactly as it does to KVStore or EventBus):
//
//   - DeploymentModeStandalone exports traces to stdout (stdouttrace) and
//     metrics both to stdout (stdoutmetric, for a developer tailing the
//     process) and to an in-process Prometheus handler (MetricsHandler) the
//     host mounts at /metrics -- zero external dependencies, matching every
//     other standalone-mode implementation in this repository.
//   - DeploymentModeDistributed exports both signals over OTLP/gRPC to a
//     collector, whose endpoint is supplied by the host via
//     WithOTLPEndpoint (see Init's doc comment for why this package does
//     not read it from the environment itself).
//
// # The three seams
//
//   - Init(ctx, mode, opts...) wires the TracerProvider and MeterProvider
//     for the given deployment mode and installs them as OpenTelemetry's
//     global providers, so that everything below can reach them without a
//     provider threaded through every module's call sites. It returns
//     a shutdown function for graceful process shutdown.
//   - FromContext(ctx) returns the *slog.Logger every module must log
//     through (root CLAUDE.md's "logger from context, not a fresh one"
//     rule): it automatically attaches trace_id and span_id when ctx
//     carries an active span, and tenant_id when ctx carries one. WithLogger
//     is the one sanctioned place to attach a hand-built logger to a
//     context, for code that runs before any request or trace exists.
//   - Middleware(next) wraps an http.Handler to start a request span (via
//     otelhttp) and record request-count and request-duration metrics.
//
// # Redaction on by default
//
// Every log attribute recorded through the FromContext API passes through a
// redaction layer (redact.go) before the record reaches the sink handler:
// attributes whose key -- or any segment of their group-nested key path --
// is secret-shaped have their whole value replaced by RedactedValue, and
// secret-shaped text embedded in otherwise-benign string and error values is
// masked in place. Redaction is on by default and cannot be disabled from
// any call site (docs/internal/09-observability.md), and it holds
// identically for every sink a host plugs in. The correlation fields
// trace_id, span_id, tenant_id, user_id and job_id are never redacted. The
// span attributes this package's instrumentation emits are kept secret-free
// by construction rather than by a second pass: Middleware assembles only
// method/route/status/tenant attributes, and the request's query string --
// where credentials ride -- never becomes a span name or attribute. See
// redact.go's doc comment for the full contract: the key set, the value
// shapes, the deliberate boundaries (log messages are not scanned; API
// responses, audit logs and dbkit encryption are separate mechanisms), and
// the one documented escape (a hand-built *slog.Logger logged through
// directly, outside FromContext).
//
// # The one rule that matters most: tenant_id is not a metric label
//
// docs/internal/09-observability.md is explicit that tenant_id must never
// become a Prometheus label -- a few thousand tenants would multiply every
// HTTP metric series by a few thousand, which is a well-known way to take
// Prometheus down. Middleware's metric attributes are therefore fixed to
// method, route and status code ONLY; the code path that records them never
// reads a tenant out of the request context at all, by construction, not by
// convention. Tenant correlation instead goes where the design doc says it
// belongs -- span attributes and structured log fields, both of which Tempo
// and Loki tolerate at high cardinality -- via AnnotateTenant and
// FromContext respectively. See middleware_test.go's cardinality tests for
// the negative control proving this.
//
// # Middleware position in the fixed chain, and what it costs
//
// docs/internal/01-architecture.md fixes the middleware chain order as
// recover -> request-id/log-context -> observability -> tenancy.Middleware
// -> authn.Middleware -> rbac.RequirePermission -> handler, and says so is
// not to be casually adjusted. Middleware is built to sit at that outer
// position -- see its own doc comment for why that placement is worth its
// one real cost (a tenant is not yet known there) rather than moving inside
// tenancy.Middleware to dodge it.
package observability
