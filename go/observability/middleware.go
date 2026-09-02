package observability

import (
	"context"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/vislake/speed/go/pkgcore"
)

// instrumentationName identifies this package's own tracer and meter to
// their providers, following OpenTelemetry's convention of naming an
// instrumentation scope after the importable package path that produces
// it.
const instrumentationName = "github.com/vislake/speed/go/observability"

// operationName is the "operation" otelhttp.NewHandler names its span
// after by default. WithSpanNameFormatter below overrides the actual span
// name with something more specific (method + path), so this value never
// reaches an exporter; it exists only because otelhttp.NewHandler requires
// one.
const operationName = "http.server.request"

// Metric instrument names and units. Names follow OpenTelemetry's
// dotted-namespace convention (the Prometheus exporter translates dots to
// underscores and appends the unit and, for counters, "_total"), matching
// the names otelhttp's own built-in HTTP metrics use so a Grafana
// dashboard built against either recognizes them.
const (
	requestCountName    = "http.server.request.count"
	requestDurationName = "http.server.request.duration"
	requestDurationUnit = "s"
)

// The attribute keys Middleware attaches to both the span and the metrics
// below. These three are the ONLY labels the metrics ever carry -- see the
// package doc comment's "tenant_id is not a metric label" section and
// middleware_test.go's cardinality tests. They deliberately reuse
// OpenTelemetry semantic-convention names (http.request.method,
// http.route, http.response.status_code) rather than inventing
// speed-specific ones, so a generic OTel-aware dashboard recognizes them
// unmodified.
const (
	httpRouteKey      = attribute.Key("http.route")
	httpMethodKey     = attribute.Key("http.request.method")
	httpStatusCodeKey = attribute.Key("http.response.status_code")
)

// MaxRouteLabelValues bounds how many distinct http.route metric label
// values a single Middleware instance will ever emit before it starts
// collapsing new ones into RouteLabelOverflowValue -- see Middleware's own
// "Route label caveat" doc comment for the live, unauthenticated exploit
// this defends against, and middleware_test.go's
// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded for the
// negative-control proof. Exported so that proof (and any consumer
// auditing this behavior) checks the actual enforced value rather than a
// hardcoded guess that could silently drift out of sync with it.
//
// 256 is comfortably above the number of distinct literal routes any real
// module in this repository registers today (a handful per module; see
// mountModuleRoutes in examples/reference-app/cmd/server/server.go), even
// summed across all 20 planned modules, while still bounding worst-case
// series growth to a small, fixed number instead of the unbounded growth
// an attacker-supplied path previously produced. If a legitimate route
// count ever approaches this, that is a signal to build the route-capture
// mechanism the "Route label caveat" section below already anticipates,
// not to raise this constant.
const MaxRouteLabelValues = 256

// RouteLabelOverflowValue replaces the http.route metric label once
// MaxRouteLabelValues distinct values have already been recorded. It
// cannot collide with a real route: every route registered anywhere in
// this repository is an absolute path starting with "/" (see
// pkgcore.MountedRoute's own doc comment), and this value deliberately is
// not one.
const RouteLabelOverflowValue = "{overflow}"

// MaxRouteLabelLength bounds, in bytes, the length of a single http.route
// value routeLabelLimiter will ever use as its map key or hand back as the
// metric attribute value -- an orthogonal bound to MaxRouteLabelValues,
// which caps how many distinct values exist, not how large any one of them
// is. See routeLabelLimiter.label's own doc comment for exactly where this
// is applied, and TestMiddleware_LongRoutePaths_LabelLengthIsBounded for
// the negative-control proof.
//
// Capping distinct-value COUNT alone still leaves a narrower, higher-cost
// resource-exhaustion vector open: examples/reference-app's http.Server
// (cmd/server/main.go) sets no MaxHeaderBytes, so it inherits
// net/http.DefaultMaxHeaderBytes (1 MiB) as the effective ceiling on how
// long a single request's URL path -- and therefore the raw value
// routeLabelLimiter.label receives -- can be. Without this bound, an
// attacker sending MaxRouteLabelValues requests, each with a distinct,
// near-1-MiB path, could make routeLabelLimiter's "seen" map retain up to
// roughly MaxRouteLabelValues x 1 MiB of attacker-controlled string data
// for the life of the process, and could make the exported Prometheus
// series themselves carry labels that large -- its own scrape-payload
// concern independent of the map.
//
// 512 is comfortably above the length of every distinct literal route any
// real module in this repository registers today: "/api/v1/notes" (the
// notes module's apiPath) is 14 bytes, and "/healthz" / "/metrics" (see
// mountModuleRoutes and its callers in
// examples/reference-app/cmd/server/server.go) are 8 bytes each. It also
// comfortably outsizes a plausible near-future route this repository does
// not mount yet but its planned modules (root CLAUDE.md's module
// dependency graph) could -- for example a four-level nested resource path
// with a 36-byte UUID at every level plus literal segment names, which
// totals well under 250 bytes -- while still keeping routeLabelLimiter's
// worst-case memory footprint a small, fixed number
// (MaxRouteLabelValues*MaxRouteLabelLength = 128 KiB) instead of the
// up-to-~256-MiB worst case an unbounded value length previously allowed.
// If a legitimate route's raw request path ever approaches this, that is a
// signal to revisit this constant deliberately, not evidence it was set
// too low by accident.
const MaxRouteLabelLength = 512

// Middleware wraps next to start a trace span per request (via otelhttp,
// which also extracts/injects W3C trace-context propagation headers) and
// record two metrics -- request count and request duration -- labeled by
// HTTP method, route and status code ONLY.
//
// # Where this sits in the chain, and why
//
// docs/internal/01-architecture.md fixes the middleware chain order as
// recover -> request-id/log-context -> observability -> tenancy.Middleware
// -> authn.Middleware -> rbac.RequirePermission -> handler, and says so is
// not to be casually adjusted. tenancy.Middleware's own doc comment
// (go/tenancy/middleware.go) says nothing about tracing middleware
// specifically, so that fixed chain is the tie-breaker this package
// follows: Middleware is meant to wrap OUTSIDE tenancy.Middleware, not
// inside it. See examples/reference-app/cmd/server/server.go's buildServer
// for where this is actually wired, with the same reasoning repeated at
// the call site.
//
// That position is deliberate, not incidental: a request tenancy.Middleware
// (or, once they exist, authn.Middleware / rbac.RequirePermission) REJECTS
// never reaches a handler at all, so if this middleware ran further in, a
// flood of 403s or 401s -- exactly the signal an operator most needs during
// an attack or a misconfigured client -- would be invisible to both the
// span and the metrics. Every request gets counted here, including ones
// that never reach a tenant-scoped handler.
//
// # The one cost of that position: tenant_id is not reliably available here
//
// Go's http.Handler chain propagates request state strictly downward:
// tenancy.Middleware injects the tenant by calling
// (*http.Request).WithContext, which allocates a new *http.Request, so a
// value it adds is visible to everything it calls next but never bubbles
// back up to a middleware wrapping it from outside. Concretely: by the
// time this middleware's own request-handling code runs
// pkgcore.TenantFromContext against the context it was actually given, in
// the position this package is documented to be mounted at, there usually
// is no tenant yet -- tenancy.Middleware resolves one two layers further
// in. This middleware still checks defensively (see the tenant handling
// below), which costs nothing and covers a caller that mounts it
// differently, but the honest expectation for the documented position is
// that this check is a no-op in production traffic.
//
// This is why AnnotateTenant exists as a separate, exported function
// (see its own doc comment): a trace Span, unlike a plain context value,
// is a shared mutable object that survives every context fork downstream
// of where it was created, so code running AFTER tenancy.Middleware --
// today, examples/reference-app's notes.Handler; once they exist,
// authn.Middleware or rbac.RequirePermission -- can still enrich the exact
// span this middleware started, using the tenant it has by then resolved.
//
// # Route label caveat
//
// The route label is derived from (*http.Request).URL.Path, not the
// lower-cardinality (*http.Request).Pattern net/http.ServeMux populates
// once Go 1.22+ matches a request: Pattern suffers the exact same
// fork-visibility problem as the tenant above (tenancy.Middleware's fork
// sits between this middleware and any mux), so it is not reliably set on
// the request this middleware observes either.
//
// Using the raw path is not merely a "future parameterized route" risk: it
// is exploitable today, by any unauthenticated caller, against this
// repository's current, unmodified route set. Neither a mux 404 (no
// registered pattern matches -- including a request under a registered
// subtree such as examples/reference-app's "/api/v1/notes/", which the
// outer mux dispatches as a match even though the notes Handler's own
// inner mux then 404s an unrecognized sub-path) nor tenancy.Middleware's
// own pre-mux 403 (an unrecognized Host, rejected before the mux ever
// runs) requires a valid tenant, a valid route, or any credential -- and
// both still reach this middleware's metric-recording code with
// (*http.Request).URL.Path exactly as the caller sent it. Left unbounded,
// an attacker can grow this metric's series count without limit simply by
// requesting distinct nonexistent URLs, no code change or new route
// required anywhere in the app -- the same cardinality-explosion failure
// mode CLAUDE.md's tenant_id rule targets, just via a different
// attacker-controlled input.
//
// The metric attrs below are therefore built from routeLabels.label(...),
// not the raw path directly: routeLabelLimiter (see its own doc comment)
// caps the number of distinct http.route METRIC label values this
// Middleware instance will ever emit at MaxRouteLabelValues, collapsing
// every value beyond that into the fixed RouteLabelOverflowValue, AND caps
// the length of any individual value at MaxRouteLabelLength bytes before
// it can become either routeLabelLimiter's map key or the label value
// itself (see MaxRouteLabelLength's own doc comment for why value SIZE,
// not just value COUNT, is its own exploitable dimension here). This does
// not require knowing an application's real route set in advance -- it
// just stops minting new distinct values, and bloating any single one of
// them, once comfortably past what any real, bounded route table could
// produce. The SPAN attributes are deliberately NOT run through this
// limiter: a trace is not a Prometheus series, so it does not share the
// metric instruments' cardinality (or per-value size) problem, matching
// how AnnotateTenant treats tenant_id (span attribute, never a metric
// label) for exactly the same reason.
//
// This bound is a circuit breaker, not a precision fix: a legitimate,
// low-cardinality parameterized route (for example
// "/api/v1/billing/subscriptions/{id}") mounted downstream of
// tenancy.Middleware would still record one distinct value per ID up to
// the cap, silently losing per-route granularity once past it, rather
// than collapsing cleanly to the route template the way a real
// route-capture mechanism (mirroring AnnotateTenant, but for the matched
// pattern) would. Building that mechanism remains future work this
// foundational round does not do; this limiter exists so the interim
// state is "bounded but occasionally imprecise" instead of "unbounded and
// exploitable today." Flag this explicitly before adding a parameterized
// route anywhere downstream of this middleware.
func Middleware(next http.Handler) http.Handler {
	meter := otel.Meter(instrumentationName)
	requestCount, _ := meter.Int64Counter(
		requestCountName,
		metric.WithDescription("Number of HTTP requests handled, labeled by method, route and status code."),
		metric.WithUnit("{request}"),
	)
	requestDuration, _ := meter.Float64Histogram(
		requestDurationName,
		metric.WithDescription("Duration of HTTP requests handled, labeled by method, route and status code."),
		metric.WithUnit(requestDurationUnit),
	)
	routeLabels := newRouteLabelLimiter(MaxRouteLabelValues)

	instrumented := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Defensive only: see the "one cost of that position" section
		// above for why this is expected to be a no-op at this
		// middleware's documented mounting point, and AnnotateTenant's
		// own doc comment for the mechanism that actually attaches
		// tenant_id to this span from further down the chain.
		AnnotateTenant(r.Context())

		next.ServeHTTP(rec, r)

		duration := time.Since(start).Seconds()
		ctx := r.Context()

		// Metric attrs use routeLabels.label(...), NOT the raw path
		// directly -- see the "Route label caveat" section above for the
		// live, unauthenticated exploit this closes. tenant_id is, and
		// must remain, absent from this slice entirely: see
		// middleware_test.go's TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries
		// for the negative control.
		metricAttrs := []attribute.KeyValue{
			httpMethodKey.String(r.Method),
			httpRouteKey.String(routeLabels.label(r.URL.Path)),
			httpStatusCodeKey.Int(rec.status),
		}
		requestCount.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
		requestDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))

		// The span, unlike the metrics above, always carries the exact
		// raw path, never routeLabels' bounded one: a trace is not a
		// Prometheus series, so it does not share the metric instruments'
		// cardinality problem -- see the "Route label caveat" section
		// above, and docs/internal/09-observability.md for why Tempo
		// tolerates high-cardinality dimensions that Prometheus cannot.
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(
			httpMethodKey.String(r.Method),
			httpRouteKey.String(r.URL.Path),
			httpStatusCodeKey.Int(rec.status),
		)
		if rec.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		}
	})

	// otelhttp.WithMeterProvider is pinned to a no-op provider so that
	// otelhttp's own built-in HTTP metrics never run alongside the
	// explicit ones recorded above: this middleware is the single source
	// of truth for which attributes an HTTP metric carries, which is the
	// property middleware_test.go's cardinality tests depend on. otelhttp
	// is still used for what it is genuinely needed for here -- starting
	// a well-formed SERVER span and handling W3C trace-context
	// propagation -- via otel.GetTracerProvider() (the default, since no
	// WithTracerProvider option is passed), which Init installs.
	return otelhttp.NewHandler(instrumented, operationName,
		otelhttp.WithMeterProvider(noop.NewMeterProvider()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// AnnotateTenant records the tenant carried by ctx, if any, as a
// TenantIDKey attribute on ctx's active span. It is a no-op when ctx
// carries no tenant (pkgcore.TenantFromContext's ok result is false -- for
// example a request on tenancy.Middleware's allowlist, or a call made
// before tenancy.Middleware has run) and effectively a no-op when ctx
// carries no active span (trace.SpanFromContext then returns a span that
// silently drops every attribute it is given).
//
// # Why this exists as its own function
//
// Middleware, per its own doc comment, is mounted OUTSIDE
// tenancy.Middleware in the fixed chain (docs/internal/01-architecture.md)
// and therefore does not reliably see a tenant on the request context it
// is handed. A trace Span, unlike a plain context value, is a shared
// mutable object reachable from every context that descends from the one
// it was placed on -- including ones produced by an intervening
// (*http.Request).WithContext fork, which is exactly how tenancy.Middleware
// injects the tenant (see its own doc comment in go/tenancy/middleware.go).
// So code running AFTER tenancy.Middleware has resolved a tenant can still
// reach back and enrich the SAME span Middleware started, by calling this
// function with a context that by then carries both the span and the
// tenant.
//
// Call it once tenant resolution has actually run: from a business
// handler that already reads the tenant for its own purposes (see
// examples/reference-app/internal/notes/handler.go's create method for a
// live example), or from downstream middleware such as a future
// authn.Middleware / rbac.RequirePermission. Unlike a Prometheus metric
// label, a span attribute is exactly where tenant_id belongs per
// docs/internal/09-observability.md: Tempo, unlike Prometheus, tolerates
// high-cardinality dimensions.
func AnnotateTenant(ctx context.Context) {
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(TenantIDKey, string(tenant)))
}

// routeLabelLimiter bounds the number of distinct http.route metric label
// values a single Middleware instance will ever emit to MaxRouteLabelValues,
// collapsing every value seen after that into RouteLabelOverflowValue, AND
// bounds the length of any individual value to MaxRouteLabelLength bytes
// (see label's own doc comment for exactly where that second, orthogonal
// bound is applied). See Middleware's own "Route label caveat" doc comment
// for the live, unauthenticated exploit the count bound exists to close:
// without it, an attacker can create one new, permanent Prometheus/OTel
// metric series per distinct URL path they send, whether or not it
// matches a real route. MaxRouteLabelLength's own doc comment covers the
// narrower, value-SIZE variant of that same exploit the count bound alone
// does not close.
//
// It is created once per Middleware call and shared, via the closure
// Middleware returns, across every concurrent request that handler serves
// -- the same lifetime and sharing pattern requestCount and
// requestDuration (the metric instruments themselves) already have, so
// its internal mutex sees exactly the concurrency a live server's request
// goroutines already produce. middleware_test.go's
// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded drives it
// concurrently, under -race, for exactly this reason.
type routeLabelLimiter struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	limit int
}

// newRouteLabelLimiter returns a routeLabelLimiter that lets up to limit
// distinct values through unchanged before it starts returning
// RouteLabelOverflowValue for anything new.
func newRouteLabelLimiter(limit int) *routeLabelLimiter {
	return &routeLabelLimiter{seen: make(map[string]struct{}, limit), limit: limit}
}

// label returns path unchanged if it has already been recorded, or if
// fewer than limit distinct values have been recorded so far (in which
// case path itself is now recorded); otherwise it returns
// RouteLabelOverflowValue without recording path, so the number of
// distinct values label can ever return stays fixed at limit+1 for the
// lifetime of l.
//
// path is truncated to at most MaxRouteLabelLength bytes via
// truncateRouteLabel BEFORE any of the above happens -- before it is
// compared against or inserted into l.seen, and before it is returned --
// so both l.seen's memory footprint and the value the caller goes on to
// use as the actual metric attribute (see Middleware) are bounded by
// MaxRouteLabelLength regardless of how long the caller's raw path is.
// This is an orthogonal bound to the limit/RouteLabelOverflowValue
// machinery above: truncation can make two distinct long paths collapse
// into the same tracked value (both being effectively attacker garbage,
// this is an acceptable, even desirable, side effect), but it never
// changes how many distinct values l.seen can hold, and never by itself
// produces RouteLabelOverflowValue.
func (l *routeLabelLimiter) label(path string) string {
	path = truncateRouteLabel(path)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[path]; ok {
		return path
	}
	if len(l.seen) >= l.limit {
		return RouteLabelOverflowValue
	}
	l.seen[path] = struct{}{}
	return path
}

// truncateRouteLabel caps path at MaxRouteLabelLength bytes, cutting on a
// UTF-8 rune boundary rather than an arbitrary byte offset: net/http
// already percent-decodes (*http.Request).URL.Path before this package
// ever sees it, so a path segment can legally contain multi-byte UTF-8,
// and slicing mid-rune would hand both l.seen and the exported Prometheus
// label an invalid, truncated encoding instead of a shorter-but-valid
// string. path shorter than or equal to the limit is returned unchanged,
// with no allocation.
func truncateRouteLabel(path string) string {
	if len(path) <= MaxRouteLabelLength {
		return path
	}
	cut := MaxRouteLabelLength
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return path[:cut]
}

// statusRecorder wraps http.ResponseWriter to capture the status code
// written, so Middleware can label its metrics and span with it. Go's
// http.ResponseWriter has no getter for the status once written.
//
// Unlike the route label (see Middleware's own doc comment for why that
// one is best-effort), the status code recorded here is always accurate
// regardless of where in the chain Middleware is mounted: an
// http.ResponseWriter is never re-derived or copied by any middleware the
// way an *http.Request is -- every layer, tenancy.Middleware included,
// writes through the exact same ResponseWriter all the way down to the
// handler that actually calls WriteHeader or Write.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader implements http.ResponseWriter, recording status the first
// time it is called -- a later call (which net/http itself would log as a
// superfluous WriteHeader call) does not overwrite the recorded value,
// matching what actually reaches the client.
func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write implements http.ResponseWriter. A handler that calls Write without
// ever calling WriteHeader implicitly sends 200, exactly like the
// underlying http.ResponseWriter would, so that implicit status is
// recorded here too.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying http.ResponseWriter to
// http.NewResponseController, so that a handler downstream of Middleware
// can still reach optional interfaces such as http.Flusher or
// http.Hijacker (needed by, for example, the SSE-based in-app-notification
// endpoints docs/internal/09-observability.md's metrics table anticipates)
// straight through this wrapper, per the standard library's own Go 1.20+
// ResponseWriter-wrapping convention.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
