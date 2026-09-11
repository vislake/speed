---
title: observability
weight: 4
description: "OpenTelemetry wiring chosen by options, not deployment mode; the context-aware structured logger whose redaction layer is on by default; and generic HTTP instrumentation with cardinality-bounded labels."
---

# observability

The observability foundation of a speed-based service: OpenTelemetry
initialization whose exporter wiring your options choose (no
deployment mode is ever consulted), a context-aware structured logger
whose PII/secret redaction is on by default with no per-call opt-out,
and HTTP middleware recording one request span plus
request-count/duration metrics behind cardinality-bounded labels.
The per-domain "must-instrument metrics" — queue depth, delivery
rates, payment outcomes — belong to the modules that own those
domains; this package supplies the wiring they all record through.

## When to choose it

Every binary uses it: `obs.Init` runs once at process startup, every
log line in request- or job-scoped code goes through
`obs.FromContext(ctx)` (never a hand-built logger), and the HTTP
`Middleware` wraps your top-level mux. Choosing the exporters is the
one real decision. No `WithOTLPEndpoint` (the default) wires the local
exporters — traces and metrics to stdout, which doubles as the
development default; a host that wants a Prometheus scrape endpoint
blank-imports `go/observability/exporter/prometheus`, which registers
the local pull-based reader `MetricsHandler` serves. Supplying
`WithOTLPEndpoint` pushes both signals over OTLP/gRPC instead — and
requires blank-importing `go/observability/exporter/otlp`, or `Init`
fails with an error naming the import. Both exporter families live in
subpackages so a logger-only consumer never inherits the gRPC or
Prometheus dependency trees; depguard keeps the root package clean of
both SDKs.

```go
shutdown, err := obs.Init(ctx, obs.WithServiceName("my-service"))
if err != nil {
    return err
}
defer shutdown(ctx) // graceful process shutdown

obs.RegisterMountedRoutes(reg.Routes.Routes()) // seed the route limiter before traffic

mux := http.NewServeMux()
mux.Handle("/", obs.Middleware(appHandler)) // outside tenancy.Middleware, per the fixed chain
```

`Init` may run more than once — each successful call tears down the
previous provider pair — and an explicitly empty service name is
refused before any provider is built.

## Core concepts and API essentials

- **`FromContext` / `WithLogger`** — the logger comes from the
  context; `FromContext` attaches `trace_id`/`span_id` from an active
  span and `tenant_id` from the tenant context, each independently
  optional, falling back to `slog.Default()`. The attribute keys are
  the shared `snake_case` constants (`TraceIDKey`, `TenantIDKey`, ...)
  every layer of the platform logs with.
- **Redaction, on by default** — every logger `FromContext` returns is
  wrapped; there is no per-call way to disable it. Two rules at the
  attribute level: a key containing a sensitive stem (`token`,
  `secret`, `password`, `authorization`, `credential`, `key` — with a
  word-boundary rule for `token` so `prompt_tokens` survives) gets its
  whole value replaced by `[REDACTED]`; and secret-shaped values under
  benign keys (Bearer/Basic credentials, JWTs, provider-prefixed keys,
  secret URL query parameters) are masked in place. Correlation fields
  (`trace_id`, `user_id`, `job_id`, ...) are never redacted. The
  deliberate boundary: PII- and prompt-shaped content under benign
  keys is the logging call site's responsibility — the layer is the
  backstop for the credential classes, never the main line for free
  text.
- **`Middleware`** — starts a span via otelhttp and records
  `http.server.request.count`/`duration` labeled by method, route and
  status only. Both attacker-controlled dimensions are bounded before
  they reach an instrument: the method label collapses to the nine
  standard methods plus `_OTHER`, and the route label is capped at
  `MaxRouteLabelValues` distinct values of `MaxRouteLabelLength` bytes,
  valid UTF-8 only, overflowing into `{overflow}` — so garbage paths
  can never mint series. **`tenant_id` never becomes a metric label**;
  tenant correlation goes to span attributes (`AnnotateTenant(ctx)`,
  called once a tenant is resolved) and log fields instead.
  `RegisterMountedRoutes` seeds the limiter with the host's real mount
  prefixes so a post-startup garbage flood cannot collapse genuine
  routes into the overflow label.
- **Span surfaces** — the span name and route attribute carry the
  bounded route value, never the raw path (whose segments carry
  tenant and resource ids into the tracing backend); the query string
  never becomes a span attribute. `url.path` is the one surface
  keeping the actual path, bounded and UTF-8-sanitized so one invalid
  byte cannot void an entire OTLP export batch.

## Boundaries and pitfalls

- Mount the middleware **outside** the whole fixed chain — it is the
  outermost layer a served request reaches, wrapping the composed
  handler above `authn.Middleware` — and call `AnnotateTenant` from
  business code once the tenant is known — the span survives the
  context fork; a plain context value does not.
- `MetricsHandler` answers 404 until `exporter/prometheus` is
  blank-imported — that is the opt-in contract, and generated
  projects inherit it.
- The logger a hand-built `WithLogger` attaches must be plain
  `slog.New(handler)` with no `With`-attached attributes — they would
  bypass the redaction layer; static attributes belong on the
  `FromContext` result.
- Log messages are constant strings; everything variable goes into
  attributes (`duration_ms`, `job_id`, ...), per the platform logging
  rules.
- No structured logging through `fmt.Println`/`log.Printf`, in any
  module — `FromContext` is the channel.

## Source

- [observability AGENTS.md](https://github.com/vislake/speed/blob/main/go/observability/AGENTS.md)
