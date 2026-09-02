# observability

The foundational layer only: dual-profile OTel initialization, a
context-aware structured logger, and generic HTTP instrumentation. See
`docs/internal/09-observability.md` for the full design intent — including
the per-domain "must-instrument metrics" table (queue depth, metering
outbox lag, notification delivery rate, payment callback success, ...),
which belongs to the modules that own those domains (`jobs`, `metering`,
`notification`, `billing-gateway`, `ai-gateway`) once they exist. None of
them exist yet (root `CLAUDE.md`'s M0 status), so this module does not
speculatively build instrumentation for them.

| Concern | Where |
|---|---|
| `Init` (dual-profile TracerProvider/MeterProvider wiring, global install, shutdown) + `MetricsHandler` (the demo profile's `/metrics` handler) | `init.go` |
| `FromContext` / `WithLogger` (the context-aware `*slog.Logger`) | `logger.go` |
| `Middleware` (per-request span + request-count/duration metrics) + `AnnotateTenant` | `middleware.go` |

## Public API

### `init.go`

| Signature | Purpose |
|---|---|
| `func Init(ctx context.Context, profile pkgcore.Profile, opts ...Option) (func(context.Context) error, error)` | Wires a `TracerProvider` and `MeterProvider` for `profile` and installs them as OTel's **global** providers (`otel.SetTracerProvider` / `otel.SetMeterProvider`), so `Middleware`, `FromContext`'s span lookup, and any future module's own `otel.Tracer`/`otel.Meter` calls all reach them with no provider threaded through every call site. Returns a shutdown function the caller must invoke during graceful process shutdown |
| `type Config struct { ServiceName, OTLPEndpoint string; OTLPInsecure bool }` + `Option` / `WithServiceName` / `WithOTLPEndpoint` / `WithOTLPInsecure` | `Init`'s tunables. `OTLPEndpoint` is deliberately **not** read from the environment by this package — see `Config.OTLPEndpoint`'s own doc comment for why (the seam a real host wires up, either directly or once `pkgcore/config`'s bootstrap loader is wired in) |
| `var ErrMissingOTLPEndpoint` | Returned by `Init` when `profile` is `ProfileProduction` and no endpoint was supplied |
| `func MetricsHandler() http.Handler` | The `/metrics` handler the most recent `Init` call wired up: a real Prometheus scrape endpoint for `ProfileDemo`, a 404 explaining metrics are pushed via OTLP for `ProfileProduction` (or before `Init` has run at all) |

**`ProfileDemo` needs zero external dependencies.** Traces go to stdout (`stdouttrace`, synchronous — no batching delay). Metrics go to stdout too (`stdoutmetric`, periodic, for a developer tailing the process) **and** to an in-process Prometheus registry exposed via `MetricsHandler` — a fresh `prometheus.NewRegistry()` per `Init` call, never `prometheus.DefaultRegisterer`, so a second `Init` call in the same process (every test in this package that exercises `ProfileDemo` does exactly this) never panics with "duplicate metrics collector registration".

**`ProfileProduction` pushes both signals over OTLP/gRPC** (`otlptracegrpc` batched, `otlpmetricgrpc` periodic) to `Config.OTLPEndpoint`.

### `logger.go`

| Signature | Purpose |
|---|---|
| `const TraceIDKey, SpanIDKey, TenantIDKey = "trace_id", "span_id", "tenant_id"` | The shared, `snake_case` field/attribute keys, per root `CLAUDE.md`'s logging rule |
| `func WithLogger(ctx context.Context, logger *slog.Logger) context.Context` | Attaches a hand-built logger to `ctx` for `FromContext` to find. The **one** legitimate call site is process startup, before any request/trace context exists — see its own doc comment |
| `func FromContext(ctx context.Context) *slog.Logger` | The **only** sanctioned way to get a logger inside request- or job-scoped code (root `CLAUDE.md`'s "logger from context, not a fresh one" rule). Falls back to `slog.Default()` when no `WithLogger` call attached one. Attaches `trace_id`+`span_id` when `ctx` carries an active OTel span, and `tenant_id` when `ctx` carries one (`pkgcore.TenantFromContext`) — each independently optional; a missing one is simply omitted, never an error or a placeholder |

### `middleware.go`

| Signature | Purpose |
|---|---|
| `func Middleware(next http.Handler) http.Handler` | Wraps `next` to start a request span (via `otelhttp`) and record `http.server.request.count` / `http.server.request.duration`, labeled **only** by method, route and status code. The metrics' route label is capped at `MaxRouteLabelValues` distinct values and `MaxRouteLabelLength` bytes each (see the caveat below); the span's own route attribute always carries the exact, uncapped `URL.Path` |
| `func AnnotateTenant(ctx context.Context)` | Records the tenant carried by `ctx`, if any, as a `tenant_id` attribute on `ctx`'s active span. No-op with no tenant or no span |
| `const MaxRouteLabelValues int` / `const RouteLabelOverflowValue string` | The route-label cardinality bound and its overflow sentinel (`"{overflow}"`) — exported so `middleware_test.go`'s negative control, and any consumer auditing this behavior, checks the actual enforced values rather than a hardcoded guess |
| `const MaxRouteLabelLength int` | Bounds, in bytes, the length of any single `http.route` value before `routeLabelLimiter` uses it as a map key or hands it back as the metric label value — an orthogonal bound to `MaxRouteLabelValues` (which caps distinct-value COUNT, not size): closes a narrower, higher-cost resource-exhaustion vector where a bounded number of very long attacker-controlled paths could still bloat the limiter's map and the exported Prometheus series. See `middleware_test.go`'s `TestMiddleware_LongRoutePaths_LabelLengthIsBounded` for the negative control |

**Hard rule, enforced by construction, not just documented: `tenant_id` never becomes a metric label.** `Middleware`'s metric-recording code path never reads `pkgcore.TenantFromContext` at all. See `middleware_test.go`'s `TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries` for the negative control: two tenants' requests collapse into one series, and no gathered metric family ever carries a `tenant_id` label. Tenant correlation instead goes to span attributes (`AnnotateTenant`) and structured log fields (`FromContext`), both of which Tempo/Loki tolerate at high cardinality — `docs/internal/09-observability.md`.

**`Middleware` is meant to be mounted OUTSIDE `tenancy.Middleware`**, per `docs/internal/01-architecture.md`'s fixed chain order (`recover -> request-id/log-context -> observability -> tenancy.Middleware -> ...`, "not to be casually adjusted"). `tenancy.Middleware`'s own doc comment says nothing about tracing middleware specifically, so that fixed order is the tie-breaker — see `examples/reference-app/cmd/server/main.go`'s `run` for the actual wiring and the same reasoning repeated at the call site. The cost of that position: a tenant is not yet known at `Middleware`'s own layer (Go's `http.Handler` chain propagates request state strictly downward — `tenancy.Middleware`'s `(*http.Request).WithContext` fork is invisible back up the chain), which is exactly why `AnnotateTenant` exists as a separate function business handlers (or, once they exist, `authn.Middleware` / `rbac.RequirePermission`) call once a tenant is actually resolved — a trace `Span`, unlike a plain context value, is a shared mutable object that survives that fork.

**Route-label caveat.** The route label is derived from `URL.Path`, not the lower-cardinality `(*http.Request).Pattern` `net/http.ServeMux` (Go 1.22+) populates on a match: `Pattern` suffers the identical fork-visibility problem `AnnotateTenant`'s doc comment describes for the tenant. This is not only a future-parameterized-route risk: it is exploitable *today*, by any unauthenticated caller, via a plain mux 404 or `tenancy.Middleware`'s own pre-mux 403 (an unrecognized `Host`) — neither requires a valid route or credential, and both still reach `Middleware` with the caller's raw path untouched. `Middleware` therefore runs the metrics' route label (only the metrics' — the span attribute stays exact) through a small in-package limiter that tracks up to `MaxRouteLabelValues` distinct values and collapses anything past that into the fixed `RouteLabelOverflowValue`, so an attacker can never grow this metric's series count past `MaxRouteLabelValues`+1 no matter how many distinct paths they send. See `middleware_test.go`'s `TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded` for the negative control. Capping distinct-value COUNT alone does not cap value SIZE: `examples/reference-app`'s `http.Server` sets no `MaxHeaderBytes`, so it inherits `net/http.DefaultMaxHeaderBytes` (1 MiB) as the effective ceiling on a single request's URL length, meaning an attacker could still send up to `MaxRouteLabelValues` distinct, near-1-MiB paths and make both `routeLabelLimiter`'s internal map and the exported Prometheus series themselves retain attacker-controlled data on that order. `Middleware` therefore also truncates every value (`truncateRouteLabel`, cutting on a UTF-8 rune boundary) to at most `MaxRouteLabelLength` bytes before it can become a map key or a label value — an orthogonal bound to the count cap: truncation can only collapse distinct long values together, never increase how many distinct values `routeLabelLimiter` tracks. See `TestMiddleware_LongRoutePaths_LabelLengthIsBounded` for that negative control. This bound is a circuit breaker, not a precision fix: a future parameterized route (e.g. `/api/v1/billing/subscriptions/{id}`) mounted downstream of `tenancy.Middleware` would still burn through the cap one ID at a time before falling back to the overflow bucket, losing per-route granularity rather than collapsing cleanly to the route template. Fixing that precisely still needs a real route-capture mechanism (mirroring `AnnotateTenant`, for the matched pattern) that this foundational round does not build. Flag this explicitly before adding a parameterized route anywhere downstream of `Middleware`.

## Testing

Full runnable versions of `Init`, `FromContext`/`WithLogger` and `Middleware`/`AnnotateTenant` live in `example_test.go` (package `observability_test`, matching `pkgcore`'s and `dbkit`'s own `example_test.go` convention), each with an `// Output:` comment asserted against the real printed output — compiled **and executed** by CI, not just described in prose. `ExampleMiddleware` in particular is a second, independent negative-control proof (alongside `middleware_test.go`'s own) that two tenants calling the same route collapse into one metric series with no `tenant_id` label.

`init_test.go`'s `TestInit_Production_ExportsRealSpansAndMetricsOverOTLP` substitutes for a testcontainers-based OTel Collector integration test: no dedicated testcontainers-go module exists for the Collector, so it instead stands up a real gRPC server implementing the actual generated OTLP collector service interfaces (`go.opentelemetry.io/proto/otlp`) in-process, and proves `Init(..., ProfileProduction, ...)` genuinely serializes and transmits a real span and a real metric to it — no Docker required, so it stays in the regular unit-test set.
