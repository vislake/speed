package observability

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
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
// name with something more specific (method + bounded route value), so this
// value never reaches an exporter; it exists only because otelhttp.NewHandler
// requires one.
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
// middleware_test.go's cardinality tests. Two of the three
// (http.request.method and http.route) are derived from request inputs an
// unauthenticated caller controls verbatim and are each bounded against
// cardinality attack before they reach an instrument -- see Middleware's
// own "Metric label cardinality caveats" section; the third
// (http.response.status_code) is a small bounded integer by construction.
// They deliberately reuse OpenTelemetry semantic-convention names
// (http.request.method, http.route, http.response.status_code) rather
// than inventing speed-specific ones, so a generic OTel-aware dashboard
// recognizes them unmodified.
const (
	httpRouteKey      = attribute.Key("http.route")
	httpMethodKey     = attribute.Key("http.request.method")
	httpStatusCodeKey = attribute.Key("http.response.status_code")
)

// The three keys below are the request-controlled raw strings otelhttp's
// own server-span semconv attaches to the span it starts -- the v1.41
// semantic-convention key names url.path, user_agent.original and
// client.address, each fed by a request input an unauthenticated caller
// controls verbatim (the percent-decoded path, the User-Agent header and
// the first X-Forwarded-For hop, respectively). Middleware overwrites each
// with a bounded copy in its recording defer -- see Middleware's own "Why
// no span surface carries the raw request path" section for why an
// unbounded copy is an availability defect, not just a disclosure one. The
// keys are spelled here as literals rather than imported from
// go.opentelemetry.io/otel/semconv/v1.41.0, keeping semconv out of this
// package's import graph the same way the http.* keys above avoid
// importing it (the module's own key-spelling pattern, see the comment on
// that block). A future otelhttp upgrade that renamed one of these keys
// would silently install a NEW raw attribute alongside the overwritten one
// and fail middleware_test.go's
// TestMiddleware_InvalidUTF8Request_SpanNameAndAttributesCarryNoRawByte
// blanket UTF-8 scan rather than passing unnoticed.
const (
	httpURLPathKey       = attribute.Key("url.path")
	httpUserAgentKey     = attribute.Key("user_agent.original")
	httpClientAddressKey = attribute.Key("client.address")
)

// MaxRouteLabelValues bounds how many distinct http.route metric label
// values a single Middleware instance will ever emit before it starts
// collapsing new ones into RouteLabelOverflowValue -- see Middleware's own
// "Metric label cardinality caveats" doc comment for the live,
// unauthenticated exploit this defends against, and middleware_test.go's
// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded for the
// negative-control proof. Exported so that proof (and any consumer
// auditing this behavior) checks the actual enforced value rather than a
// hardcoded guess that could silently drift out of sync with it.
//
// 256 is comfortably above the number of distinct literal routes any real
// module in this repository registers (a handful per module; see
// mountModuleRoutes in examples/reference-app/internal/app/server.go), even
// summed across all planned modules, while still bounding worst-case
// series growth to a small, fixed number instead of the unbounded growth
// an attacker-supplied path would produce. If a legitimate route count
// ever approaches this, that is a signal to build the route-capture
// mechanism the "Metric label cardinality caveats" section below already
// anticipates, not to raise this constant.
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
// real module in this repository registers: "/api/v1/notes" (the notes
// module's apiPath) is 14 bytes, and "/healthz" / "/metrics" (see
// mountModuleRoutes and its callers in
// examples/reference-app/internal/app/server.go) are 8 bytes each. It also
// comfortably outsizes any four-level nested resource path with a 36-byte
// UUID at every level plus literal segment names, which totals well under
// 250 bytes -- while still keeping routeLabelLimiter's worst-case memory
// footprint a small, fixed number
// (MaxRouteLabelValues*MaxRouteLabelLength = 128 KiB) instead of the
// up-to-~256-MiB worst case an unbounded value length would allow.
// If a legitimate route's raw request path ever approaches this, that is a
// signal to revisit this constant deliberately, not evidence it was set
// too low by accident.
const MaxRouteLabelLength = 512

// MethodLabelOverflowValue replaces the http.request.method METRIC label
// for every method token that is not one of the nine standard HTTP methods
// (net/http's MethodGet through MethodTrace constants, recorded verbatim --
// the only values a generic OTel-aware dashboard's method dimension is built
// to recognize). See methodMetricLabel for where the collapse happens, and
// Middleware's own "Metric label cardinality caveats" doc comment for the
// live, unauthenticated exploit this defends against and middleware_test.go's
// TestMiddleware_UnboundedMethodTokens_CardinalityIsBounded for the
// negative-control proof. Exported so that proof (and any consumer auditing
// this behavior) checks the actual enforced value rather than a hardcoded
// guess that could silently drift out of sync with it.
//
// The value is "_OTHER", the OpenTelemetry semantic-convention reserved
// value for enum-like attributes such as http.request.method: this package
// deliberately reuses OTel semantic-convention names and values so a generic
// dashboard recognizes them unmodified, and _OTHER is the convention's own
// collapse value for a method outside its known enum. It cannot collide with
// a standard-method label: matching is exact and case-sensitive -- HTTP
// method tokens are case-sensitive per RFC 7230, so "get" is not GET and
// folds here exactly like any other non-standard token -- and none of the
// nine known tokens equals "_OTHER". An attacker CAN send the literal token
// "_OTHER"; it simply collapses onto this same fixed value, growing nothing.
//
// Unlike the route dimension, no per-value LENGTH bound (MaxRouteLabelLength
// style) and no distinct-value COUNT cap (MaxRouteLabelValues style) are
// needed here, and one is deliberately not bolted on: routeLabelLimiter
// must retain every seen path as a map key for the process lifetime, which
// is exactly why its two bounds exist; a set-collapse scheme like this one
// never retains the attacker's token at all -- the match against the nine
// known constants either succeeds and yields a fixed known string, or fails
// and yields this other fixed string -- so series growth stays fixed at
// (9 known methods + 1) x (route bound) x status codes per Middleware
// instance and memory stays constant, whatever token an attacker sends.
//
// If a tenth method ever needs recording verbatim (a standards-body
// addition, say), that is a deliberate decision to make in methodMetricLabel
// with this constant's doc comment updated in the same edit -- the known set
// is exactly the nine net/http constants and nothing else, by construction.
const MethodLabelOverflowValue = "_OTHER"

// methodMetricLabel returns the value Middleware records for the
// http.request.method METRIC label given the request line's method token:
// the token itself when it is one of the nine standard HTTP methods (the
// net/http MethodGet through MethodTrace constants), MethodLabelOverflowValue
// for everything else. Matching is exact and case-sensitive -- HTTP method
// tokens are case-sensitive per RFC 7230, so "get" folds to
// MethodLabelOverflowValue exactly like an attacker-chosen garbage token
// does. This is a pure, allocation-free, state-free function: nothing about
// the token is retained, which is why (unlike the route dimension, whose
// limiter keeps a "seen" map) the method dimension needs no value-length or
// distinct-value-count bound -- see MethodLabelOverflowValue's own doc
// comment for that reasoning, and Middleware's "Metric label cardinality
// caveats" section for the exploit this closes. The span's METHOD
// attribute is deliberately NOT run through this function (see the same
// section): the span keeps the exact raw token. The span's ROUTE
// attribute, by contrast, now shares the metric side's bounded route
// value (see that section's paragraph on the route dimension).
func methodMetricLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return MethodLabelOverflowValue
	}
}

// Middleware wraps next to start a trace span per request (via otelhttp,
// which also extracts/injects W3C trace-context propagation headers) and
// record two metrics -- request count and request duration -- labeled by
// HTTP method, route and status code ONLY.
//
// # Where this layer sits, and why
//
// Middleware is the layer this module's component declares on the
// registry's Middleware seat (see component.go's initObservability), and
// go/app/chain.Standard applies the seat around the finished chain it
// builds: the assembled handler is this layer outside the whole chain --
// outside authn.Middleware, outside the AdminRoutes and AuthnRoutes
// branches, outside tenancy.Middleware and outside the protected routes'
// own gates. The chain's internal order belongs to go/app/chain; the seat
// can only wrap that chain's output, never enter it (see
// pkgcore.MiddlewareRegistrar for the seat's contract).
//
// That position is deliberate, not incidental: a request authn.Middleware,
// tenancy.Middleware or a route's permission gate REJECTS never reaches a
// handler at all, so if this middleware ran further in, a flood of 403s or
// 401s -- exactly the signal an operator most needs during an attack or a
// misconfigured client -- would be invisible to both the span and the
// metrics. Every request the chain handles gets counted here, including
// ones that never reach a tenant-scoped handler. A host that serves
// requests outside the chain -- the reference app's SPA file server,
// wrapped around its composed face when it serves a frontend directory --
// keeps those requests outside this layer too.
//
// # The one cost of that position: tenant_id is not reliably available here
//
// Go's http.Handler chain propagates request state strictly downward:
// tenancy.Middleware injects the tenant by calling
// (*http.Request).WithContext, which allocates a new *http.Request, so a
// value it adds is visible to everything it calls next but never bubbles
// back up to a middleware wrapping it from outside. Concretely: by the
// time this middleware's own request-handling code runs
// pkgcore.TenantFromContext against the context it was actually given --
// at the seat layer, outside the chain -- there usually is no tenant yet:
// tenancy.Middleware resolves one further down the chain. This middleware
// still checks defensively (see the tenant handling below), which costs
// nothing and covers a caller that mounts it differently, but the honest
// expectation for this layer's position is that the check is a no-op in
// production traffic.
//
// This is why AnnotateTenant exists as a separate, exported function
// (see its own doc comment): a trace Span, unlike a plain context value,
// is a shared mutable object that survives every context fork downstream
// of where it was created, so code running AFTER tenancy.Middleware --
// today, examples/reference-app's notes.Handler; once they exist,
// authn.Middleware or rbac.RequirePermission -- can still enrich the exact
// span this middleware started, using the tenant it has by then resolved.
//
// # Metric label cardinality caveats
//
// Two of the three metric labels -- http.route and http.request.method --
// are derived from request inputs an unauthenticated caller controls
// verbatim, and each is bounded against the cardinality-explosion failure
// mode CLAUDE.md's tenant_id rule targets before it can feed an
// instrument. The third, http.response.status_code, is a small bounded
// integer by construction and needs no bound.
//
// # Route dimension
//
// The route label is derived from (*http.Request).URL.Path, not the
// lower-cardinality (*http.Request).Pattern net/http.ServeMux populates
// once Go 1.22+ matches a request: Pattern suffers the exact same
// fork-visibility problem as the tenant above (tenancy.Middleware's fork
// sits between this middleware and any mux), so it is not reliably set on
// the request this middleware observes either.
//
// Using the raw path is not merely a parameterized-route risk: it is
// exploitable by any unauthenticated caller, against this repository's
// current, unmodified route set. Neither a mux 404 (no
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
// required anywhere in the app.
//
// # Method dimension
//
// The http.request.method label has the identical hole, one dimension
// over: it is fed by (*http.Request).Method -- the request line's raw
// method token, which net/http accepts from any caller with no set
// constraint, no normalization and no truncation (RFC 7230 token
// characters only, but that still leaves the token space effectively
// unbounded). The same pre-auth 404s and 403s the route exploit above
// uses reach this middleware's metric-recording code with r.Method
// exactly as the caller sent it, so without a bound an attacker sending
// one distinct method token per request creates one new, permanent
// series per token, multiplied against the (bounded) route and status
// dimensions. Bounding the route dimension while leaving this sibling
// raw would only be a half-fix; both request-controlled dimensions are
// bounded, and middleware_test.go carries a negative control for each.
//
// The metric attrs below are therefore bounded on BOTH request-controlled
// dimensions before they reach the instruments:
//
//   - http.route comes from routeLabels.label(...), never the raw path
//     directly: routeLabelLimiter (see its own doc comment) caps the
//     number of distinct http.route METRIC label values this Middleware
//     instance will ever emit at MaxRouteLabelValues, collapsing every
//     value beyond that into the fixed RouteLabelOverflowValue, AND caps
//     the length of any individual value at MaxRouteLabelLength bytes
//     before it can become either routeLabelLimiter's map key or the
//     label value itself (see MaxRouteLabelLength's own doc comment for
//     why value SIZE, not just value COUNT, is its own exploitable
//     dimension here), AND replaces any byte sequence that is not valid
//     UTF-8 before the value can become a map key or a label -- a path
//     whose percent-decoded bytes are invalid UTF-8 (net/http
//     percent-decodes byte-wise, so %FF in a request target reaches this
//     middleware as a raw 0xFF byte with no parse error anywhere) would
//     otherwise hand the exporter a label value it refuses to encode,
//     voiding the entire /metrics scrape for the life of the process (see
//     sanitizeRouteLabel's own doc comment for that third, orthogonal
//     dimension of the same unauthenticated-DoS class the count and
//     length caps close). When the host registers the application's real
//     route table (RegisterMountedRoutes), every entry -- a
//     pkgcore.MountedRoute.Path, the PREFIX the module's handler was
//     mounted at -- is seeded into the limiter at construction as the
//     label of its whole mount subtree, so the distinct-value budget
//     cannot be exhausted by attacker garbage before a real route is ever
//     requested and no request at or below a real mount can ever overflow
//     (see routeLabelLimiter.seed). None of this requires knowing an
//     application's real route set in advance -- the bounds just stop
//     minting new distinct values, and bloating any single one of them,
//     once comfortably past what any real, bounded route table could
//     produce.
//   - http.request.method comes from methodMetricLabel(r.Method), never
//     the raw token directly: the nine standard methods (net/http's
//     MethodGet through MethodTrace constants) are recorded verbatim, so
//     existing dashboards keep working unchanged, and every other token
//     -- attacker-chosen or merely exotic -- collapses to the single
//     fixed MethodLabelOverflowValue. A set-collapse scheme retains
//     nothing, so unlike the route limiter it needs neither a
//     distinct-value count cap nor a per-value length bound (see
//     MethodLabelOverflowValue's own doc comment for that reasoning).
//
// The span does not share the metric instruments' cardinality (or
// per-value size) problem -- a trace is not a Prometheus series -- but a
// trace IS an exit of the data-protection rule ("never enter logs, traces
// or API responses"), and the raw path is where request paths carry tenant
// and
// resource ids. The span's route attribute therefore shares the route
// dimension's bounded value with the metric side: routeLabels.label(...)
// is computed once per request and feeds both the metric label and the
// span attribute, so the metric side's three bounds are disclosure bounds
// on the span too -- a request below a seeded mount is labeled with the
// mount prefix whatever id-bearing segments its raw path carries, an
// unmatched path stays exact only while the distinct-value budget allows
// and then collapses to RouteLabelOverflowValue, and every value is
// length-bounded and UTF-8-valid. Pinned by middleware_test.go's
// TestMiddleware_SpanRouteAttribute_MatchesTheMetricRouteLabel. The
// span's METHOD attribute is the deliberate exception: it keeps the
// exact raw request-line token (the method token is a bounded enum by
// protocol -- net/http accepts only RFC 7230 token characters -- never a
// disclosure or validity surface, and verbatim methods keep existing
// trace dashboards working), matching how AnnotateTenant treats
// tenant_id -- span attribute, never a metric label -- for the same
// Tempo-tolerates-high-cardinality reason.
//
// # Why no span surface carries the raw request path
//
// The span NAME is under the same bound, not a raw-path exception to it:
// the otelhttp formatter at the bottom of Middleware returns method + the
// same routeLabels.label(...) result. A raw method + raw path name would
// carry the raw byte an invalid-UTF-8 request target supplies -- a
// trace-side correlation string in the tolerated-cardinality class of
// tenant_id, but an availability defect once the raw byte is invalid:
// net/http percent-decodes a request target byte-wise, so a request whose
// target contains %FF (or any other invalid UTF-8 encoding) reaches this
// middleware with the raw invalid byte in the path and no parse error
// anywhere. proto3 string fields must be valid UTF-8, and the Go protobuf
// encoder fails the WHOLE ExportTraceServiceRequest on one invalid string,
// so a single such request made otlptracegrpc drop the entire export batch
// (codes.Internal sits outside its retry whitelist): sustained 100% trace
// loss -- and traces are exactly the system an operator uses to
// investigate the request that caused it. The route value's own bounds are
// what keep the name encodable (sanitizeRouteLabel replaces the invalid
// bytes, truncateRouteLabel caps the value, and the distinct-value bound
// caps how many distinct names exist at all): the metric label, the span's
// http.route attribute and the span name share one boundary, the route
// value, and every request-controlled string that reaches the span runs
// through it or through its length and UTF-8 bounds (see
// boundedRequestString below).
//
// The one span surface that keeps the ACTUAL path is url.path -- the
// attribute otelhttp's own server-span semconv attaches at span creation
// (its internal semconv package, key "url.path", fed by the raw
// percent-decoded (*http.Request).URL.Path; the dependency installs it
// unconditionally, so no formatter-only fix can reach it). otelhttp's
// semconv attaches two more request-controlled raw strings the same way --
// user_agent.original (the User-Agent header) and client.address (the
// first X-Forwarded-For hop, else the peer address) -- and net/http
// applies no byte validation to header values, so all three can carry an
// invalid byte exactly as the path can. The recording defer therefore
// OVERWRITES all three with exporter-safe copies: url.path and
// user_agent.original keep their real text under the route label's length
// and UTF-8 bounds, so an operator can still find the exact resource a
// slow trace was for, and client.address is re-derived from the same
// X-Forwarded-For / peer inputs otelhttp's semconv uses (spanClientAddress)
// and run through the same bounds. Pinned at span formation by
// TestMiddleware_InvalidUTF8Request_SpanNameAndAttributesCarryNoRawByte
// and, end to end through a real OTLP/gRPC export, by
// exporter/otlp's TestMiddleware_InvalidUTF8Request_ExportBatchStillArrives.
//
// The route bound is a circuit breaker, not a precision fix: once
// requests pass, the closest thing this middleware has to a route
// template is the mount-prefix label a seeded subtree folds onto (see
// RegisterMountedRoutes), so every distinct template mounted under one
// prefix -- say "/api/v1/billing/subscriptions/{id}" and
// "/api/v1/billing/subscriptions/{id}/comments" under the seeded
// "/api/v1/billing" -- shares that one prefix as its label, and a route
// the host mounted but did not register (added after
// RegisterMountedRoutes, for example) still records one distinct value
// per distinct request path up to the cap before collapsing to the
// overflow bucket. That is a deliberate, bounded imprecision -- the
// alternative to an unbounded label space. True per-template folding
// (mirroring AnnotateTenant, but for the matched pattern) is not built:
// the route limiter exists so the state is "bounded but occasionally
// imprecise" instead of "unbounded and exploitable." Flag this
// explicitly before adding a parameterized route anywhere downstream of
// this middleware. The method bound has no
// equivalent imprecision: its known set is complete by construction, so
// the method dimension is never approximate.
func Middleware(next http.Handler) http.Handler {
	meter := otel.Meter(instrumentationName)
	requestCount, err := meter.Int64Counter(
		requestCountName,
		metric.WithDescription("Number of HTTP requests handled, labeled by method, route and status code."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		// A construction error means the meter could not register the
		// instrument (an invalid name, or a conflicting registration of
		// the same name) and handed back a no-op in its place -- requests
		// would then go uncounted with no startup signal at all, the same
		// silent-failure mode initLocalExporters' missing-reader warning
		// exists to prevent. The error goes to OTel's global error handler
		// (stderr by default, capturable via otel.SetErrorHandler) rather
		// than being dropped; recording into the returned instrument stays
		// safe either way, since a no-op instrument accepts every record.
		otel.Handle(fmt.Errorf("observability: create %q counter instrument: %w", requestCountName, err))
	}
	requestDuration, err := meter.Float64Histogram(
		requestDurationName,
		metric.WithDescription("Duration of HTTP requests handled, labeled by method, route and status code."),
		metric.WithUnit(requestDurationUnit),
	)
	if err != nil {
		otel.Handle(fmt.Errorf("observability: create %q histogram instrument: %w", requestDurationName, err))
	}
	routeLabels := newRouteLabelLimiter(MaxRouteLabelValues)
	// Seed the limiter with the application's real route table so the
	// distinct-value budget cannot be exhausted by request-time garbage
	// before a real route is ever requested -- see RegisterMountedRoutes'
	// own doc comment.
	for _, path := range registeredRoutePaths() {
		routeLabels.seed(path)
	}

	instrumented := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		// Defensive only: see the "one cost of that position" section
		// above for why this is expected to be a no-op at this
		// middleware's documented mounting point, and AnnotateTenant's
		// own doc comment for the mechanism that actually attaches
		// tenant_id to this span from further down the chain.
		AnnotateTenant(r.Context())

		// The recording block below runs in a defer, not after
		// next.ServeHTTP returns, so a handler that panics is still
		// counted: nothing above this layer recovers a panic (net/http's
		// per-connection recovery is the first thing a panic reaches), so
		// without the defer a panicking handler would unwind straight past
		// this middleware and vanish from both the metrics and the span
		// status -- contradicting this middleware's own "Every request the
		// chain handles gets counted here" contract in its doc comment's
		// "Where this layer sits" section.
		panicked := true
		defer func() {
			// A handler that panicked before writing any response produced
			// no status for the client at all (net/http aborts the
			// connection). The 500 recorded here is this middleware's
			// stand-in for "the request failed server-side": the
			// statusRecorder's implicit-200 default would mislabel a
			// request that never completed.
			if panicked && !rec.wroteHeader {
				rec.status = http.StatusInternalServerError
			}

			duration := time.Since(start).Seconds()
			ctx := r.Context()

			// Metric attrs bound BOTH request-controlled label dimensions
			// before recording: http.request.method through
			// methodMetricLabel(...) and http.route through
			// routeLabels.label(...), never the raw token or raw path
			// directly -- see the "Metric label cardinality caveats"
			// section above for the live, unauthenticated exploit each
			// bound closes. tenant_id is, and must remain, absent from
			// this slice entirely: see middleware_test.go's
			// TestMiddleware_MetricsExcludeTenant_NoPerTenantSeries for
			// the negative control.
			routeLabel := routeLabels.label(r.URL.Path)
			metricAttrs := []attribute.KeyValue{
				httpMethodKey.String(methodMetricLabel(r.Method)),
				httpRouteKey.String(routeLabel),
				httpStatusCodeKey.Int(rec.status),
			}
			requestCount.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
			requestDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))

			// The span shares the metric side's route value: its http.route
			// attribute is the same routeLabels.label(...) result computed
			// above, never the raw path -- a trace is not a Prometheus
			// series, but it IS an exit of the data-protection rule, and
			// the bounded route value is the disclosure bound on the span
			// (see the "Metric label cardinality caveats" section above
			// and middleware_test.go's
			// TestMiddleware_SpanRouteAttribute_MatchesTheMetricRouteLabel).
			// The method attribute is the deliberate exception, keeping the
			// exact raw token (see methodMetricLabel's own doc comment),
			// and the span name set by the otelhttp formatter below shares
			// the same bounded route value (see that section's "Why no
			// span surface carries the raw request path" paragraph).
			//
			// The overwrites below cover the request-controlled raw strings
			// otelhttp's own semconv attached at span creation -- url.path,
			// user_agent.original and client.address -- each replaced with
			// its boundedRequestString form, so no raw byte a caller's
			// path or headers carried can reach the exported span: an
			// invalid-UTF-8 string fails the proto3 marshal of the whole
			// export batch (see the same paragraph). The conditions mirror
			// otelhttp's own attach conditions (a non-empty User-Agent, a
			// resolvable client address), so the overwrite never invents an
			// attribute where the dependency attached none.
			span := trace.SpanFromContext(ctx)
			span.SetAttributes(
				httpMethodKey.String(r.Method),
				httpRouteKey.String(routeLabel),
				httpStatusCodeKey.Int(rec.status),
				httpURLPathKey.String(boundedRequestString(r.URL.Path)),
			)
			if ua := r.UserAgent(); ua != "" {
				span.SetAttributes(httpUserAgentKey.String(boundedRequestString(ua)))
			}
			if addr := spanClientAddress(r); addr != "" {
				span.SetAttributes(httpClientAddressKey.String(boundedRequestString(addr)))
			}
			// A panicking handler is an error even when it had already
			// written a status below 500 before the panic: the request did
			// not complete, and the span must say so.
			if panicked || rec.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
		}()

		next.ServeHTTP(rec, r)
		panicked = false
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
	// The span-name formatter returns method + the bounded route value,
	// never method + the raw path: the raw path can carry a byte that
	// would fail the proto3 marshal of the whole export batch (see
	// Middleware's own "Why no span surface carries the raw request path"
	// section). routeLabels.label(...) is the same bounded value the metric
	// side and the span's http.route attribute use, so all three share one
	// boundary; the call is idempotent, so the recording defer's later call
	// for the same path returns the identical value (otelhttp may also
	// invoke the formatter a second time once a mux match populates
	// r.Pattern, with the same result).
	return otelhttp.NewHandler(instrumented, operationName,
		otelhttp.WithMeterProvider(noop.NewMeterProvider()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + routeLabels.label(r.URL.Path)
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
// Middleware, per its own doc comment, stands OUTSIDE the whole chain
// (the Middleware seat's layer) and therefore does not reliably see a
// tenant on the request context it
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
// examples/reference-app/internal/notes/handler.go's NotesCreateNote
// method for a live example), or from downstream middleware such as
// authn.Middleware / rbac.RequirePermission. Unlike a Prometheus metric
// label, a span attribute is exactly where tenant_id belongs: Tempo,
// unlike Prometheus, tolerates high-cardinality dimensions.
func AnnotateTenant(ctx context.Context) {
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(TenantIDKey, string(tenant)))
}

// mountedRoutesMu guards mountedRoutePaths. Registration happens once at
// host assembly time in a real binary, but this package's own tests call
// RegisterMountedRoutes around repeated Middleware constructions (exactly
// the repeated-call pattern init.go's metricsHandlerMu exists for), and
// nothing forbids a host from registering at any time, so the snapshot is
// mutex-guarded rather than a bare package variable.
var (
	mountedRoutesMu   sync.Mutex
	mountedRoutePaths []string
)

// RegisterMountedRoutes hands Middleware the application's real route
// table -- the pkgcore.MountedRoute values the host's modules registered
// on the host's pkgcore.ComponentRegistry (the registry's MountedRoutes
// reading of the Routes seat), plus any host-level routes the host
// mounts itself -- so the route label limiter every Middleware instance
// creates can reserve a place for each real route BEFORE any request
// traffic arrives. Without that reservation, the limiter's
// distinct-value budget (MaxRouteLabelValues) is first-come-first-served:
// an attacker sending enough distinct garbage paths right after startup
// fills the budget and collapses every genuine route to
// RouteLabelOverflowValue for the life of the process -- per-route metrics
// gone even though no bound was violated.
//
// A registered entry is a mount PREFIX, not the full path of any one
// operation: pkgcore.MountedRoute.Path is "the prefix the handler was
// mounted at" (pkgcore/registry.go), and every operation of the mounted
// handler lies at or below it. Each entry is therefore seeded as the
// label of its whole mount subtree -- a request whose path is the
// entry's own path or lies below it (net/http ServeMux subtree
// semantics; see routeLabelLimiter.seed and belowSeededMount) is labeled
// with the entry's path itself, whatever garbage arrives later. The
// label's granularity ceiling is thus the mount: every template under
// one prefix shares the prefix as its label. This is the closest the
// limiter can get to route-template folding without a real route-capture
// mechanism; a route the host did not register here (added after this
// call, say) is not covered and remains subject to the ordinary bounded
// behavior -- exact recording while the budget allows, then the overflow
// bucket.
//
// Call it once, at assembly time, before constructing the Middleware that
// serves the traffic: Middleware snapshots the registered paths at
// construction, so a later registration does not reach an already-built
// handler. Last registration wins, and an empty (or nil) route list
// clears any earlier registration -- the reset form this package's own
// tests use between cases. The paths are truncated and UTF-8-sanitized
// exactly like request-time labels when they are seeded, so the same
// bounds apply to them (see routeLabelLimiter.seed).
//
// examples/reference-app is the mandatory first consumer: its BuildServer
// calls this with the module table reg.Routes.Routes() holds plus the two
// host-level routes (/healthz and /metrics) it mounts on the mux
// directly, and its flowtests/obs_route_seed_test.go drives the real composed
// stack through the garbage-flood scenario. The mechanism's behavioral
// proof inside this module is middleware_test.go's
// TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded, and the
// mount-subtree semantics -- a deep operation under a seeded prefix
// keeping its own series after the budget is exhausted -- are pinned by
// TestMiddleware_RealRoutesBelowSeededPrefixes_SurviveStartupGarbage and
// TestMiddleware_SeededMountPrefix_FoldsRequestsAtOrBelowIt.
func RegisterMountedRoutes(routes []pkgcore.MountedRoute) {
	mountedRoutesMu.Lock()
	defer mountedRoutesMu.Unlock()
	mountedRoutePaths = make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			mountedRoutePaths = append(mountedRoutePaths, route.Path)
		}
	}
}

// registeredRoutePaths returns a copy of the registered route paths, for
// Middleware to snapshot into the limiter it builds.
func registeredRoutePaths() []string {
	mountedRoutesMu.Lock()
	defer mountedRoutesMu.Unlock()
	return append([]string(nil), mountedRoutePaths...)
}

// routeLabelLimiter bounds the number of distinct http.route metric label
// values a single Middleware instance will ever emit to MaxRouteLabelValues,
// collapsing every value seen after that into RouteLabelOverflowValue, AND
// bounds the length of any individual value to MaxRouteLabelLength bytes
// (see label's own doc comment for exactly where that second, orthogonal
// bound is applied). See Middleware's own "Metric label cardinality
// caveats" doc comment for the live, unauthenticated exploit the count
// bound exists to close:
// without it, an attacker can create one new, permanent Prometheus/OTel
// metric series per distinct URL path they send, whether or not it
// matches a real route. MaxRouteLabelLength's own doc comment covers the
// narrower, value-SIZE variant of that same exploit the count bound alone
// does not close, and sanitizeRouteLabel's own doc comment the value
// VALIDITY variant neither bound checks. The sibling http.request.method
// dimension is bounded by its own mechanism (methodMetricLabel) -- a fixed
// set-collapse that needs no per-instance state and therefore no count or
// length bound of its own -- so this limiter is the route half of a
// two-dimension story, never the whole of it.
//
// It is created once per Middleware call and shared, via the closure
// Middleware returns, across every concurrent request that handler serves
// -- the same lifetime and sharing pattern requestCount and
// requestDuration (the metric instruments themselves) already have, so
// its internal mutex sees exactly the concurrency a live server's request
// goroutines already produce. middleware_test.go's
// TestMiddleware_UnboundedRoutePaths_CardinalityIsBounded drives it
// concurrently, under -race, for exactly this reason. At construction the
// host's registered route table is seeded into it (see
// RegisterMountedRoutes and seed), so real routes claim their budget
// slots -- and, as mount-subtree labels, every request at or below them
// -- before any request traffic can.
type routeLabelLimiter struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	limit int
	// seeded is every seed entry that successfully reserved a budget slot
	// (see seed), kept most-specific-first so label()'s first subtree
	// match is the deepest mount covering the request.
	seeded []seededMount
}

// seededMount is one seed entry remembered for request-to-subtree
// matching: label is the value label() returns (the registered path,
// truncated and UTF-8-sanitized exactly like any other value), and match
// is label with any trailing "/" removed -- the root against which a
// request's path is tested for "at or below" (see belowSeededMount), so
// a registration of "/api/v1/notes" and one of "/api/v1/notes/" cover
// the same subtree while emitting the entry's own spelling verbatim.
type seededMount struct {
	label string
	match string
}

// newRouteLabelLimiter returns a routeLabelLimiter that lets up to limit
// distinct values through unchanged before it starts returning
// RouteLabelOverflowValue for anything new.
func newRouteLabelLimiter(limit int) *routeLabelLimiter {
	return &routeLabelLimiter{seen: make(map[string]struct{}, limit), limit: limit}
}

// label returns path unchanged if it has already been recorded, or if
// path lies at or below a seeded mount (see seed), in which case the
// seed's own value is returned -- that value's budget slot was reserved
// at construction, so nothing new is recorded and the overflow machinery
// is never consulted; or if fewer than limit distinct values have been
// recorded so far (in which case path itself is now recorded); otherwise
// it returns RouteLabelOverflowValue without recording path, so the
// number of distinct values label can ever return stays fixed at limit+1
// for the lifetime of l.
//
// path is passed through truncateRouteLabel (capped at MaxRouteLabelLength
// bytes) and sanitizeRouteLabel (invalid UTF-8 replaced) BEFORE any of the
// above happens -- before it is compared against or inserted into l.seen,
// and before it is returned -- so both l.seen's memory footprint and the
// value the caller goes on to use as the actual metric attribute (see
// Middleware) are bounded by MaxRouteLabelLength and always valid UTF-8,
// regardless of what the caller's raw path contains. The length bound is
// orthogonal to the limit/RouteLabelOverflowValue machinery above:
// truncation can make two distinct long paths collapse into the same
// tracked value (both being effectively attacker garbage, this is an
// acceptable, even desirable, side effect), but it never changes how many
// distinct values l.seen can hold, and never by itself produces
// RouteLabelOverflowValue. The UTF-8 sanitization likewise never produces
// RouteLabelOverflowValue: an invalid path is still counted, under a
// sanitized label, because "this request happened" remains true whatever
// bytes its path carried -- see sanitizeRouteLabel for why validity (not
// just count and length) is a bound this package must enforce at all.
func (l *routeLabelLimiter) label(path string) string {
	path = truncateRouteLabel(path)
	path = sanitizeRouteLabel(path)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.seen[path]; ok {
		return path
	}
	// A request at or below a seeded mount is labeled with the seed's own
	// value -- see seed for why every seed is a mount PREFIX whose whole
	// subtree this closes. The scan is over the seeded mounts only, each
	// of which already occupies a budget slot, so this branch neither
	// records nor consults the overflow machinery; most-specific-first
	// ordering (seed sorts on every insertion) makes the first match the
	// deepest mount covering the request.
	for _, m := range l.seeded {
		if belowSeededMount(m.match, path) {
			return m.label
		}
	}
	if len(l.seen) >= l.limit {
		return RouteLabelOverflowValue
	}
	l.seen[path] = struct{}{}
	return path
}

// belowSeededMount reports whether request path is the seeded mount's own
// path or lies below it, following the subtree semantics of the
// net/http.ServeMux mount the seed represents: a handler mounted at P
// serves P and everything under P/, never a path that merely shares P as
// a string prefix (a request "/api/v1/notesXYZ" is not below a mount at
// "/api/v1/notes"). match is the seed's path with any trailing "/"
// removed, so trailing-slash and bare spellings of one mount cover the
// same subtree; a match of "" -- a seed of "/", a mount at the root --
// is below every request, since every request path starts with "/".
func belowSeededMount(match, path string) bool {
	return path == match || strings.HasPrefix(path, match+"/")
}

// seed reserves a place for path in l's seen set and registers path as a
// mount-subtree label, without ever consulting the limit-to-overflow
// machinery: used at construction time to pre-insert the application's
// real route table (see RegisterMountedRoutes) so that request-time
// garbage paths cannot exhaust the distinct-value budget and collapse a
// real route to RouteLabelOverflowValue before the route is ever
// requested. Every seed is a pkgcore.MountedRoute.Path -- the PREFIX a
// module's handler was mounted at (pkgcore/registry.go), not the full
// path of any one operation -- so an exact-string reservation alone
// would protect only the request whose whole path IS that prefix, and
// every genuine operation path deeper than its mount would still compete
// with garbage for the remaining budget. seed therefore also remembers
// path as the label of its whole subtree: label() returns the seed's own
// value for every request at or below it (see belowSeededMount), which
// is the closest this limiter can get to route-template folding without
// a real route-capture mechanism, and which makes garbage sent below a
// real mount unable to mint a label or consume a budget slot at all.
//
// path goes through the same truncation and UTF-8 sanitization as label's
// inputs, so a seed can never occupy more than MaxRouteLabelLength bytes
// or introduce an invalid-UTF-8 value into l.seen (a real route is a
// valid string by construction, but the discipline is uniform). Once the
// set is full -- only possible when a host registered more routes than
// MaxRouteLabelValues, which is its own beyond-any-planned-route-count
// signal -- further seeds are dropped, and a dropped seed is not
// registered as a mount-subtree label either: every entry in l.seeded
// has its label present in l.seen, which is what keeps the
// distinct-values bound at limit+1 no matter how many requests its
// subtree attracts.
func (l *routeLabelLimiter) seed(path string) {
	path = truncateRouteLabel(path)
	path = sanitizeRouteLabel(path)

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) >= l.limit {
		return
	}
	if _, ok := l.seen[path]; ok {
		return
	}
	l.seen[path] = struct{}{}
	l.seeded = append(l.seeded, seededMount{label: path, match: strings.TrimRight(path, "/")})
	// Most-specific mount first, so label()'s linear scan can stop at the
	// first match: among overlapping seeds only the deepest can contain
	// the request's path, and seeds whose subtrees do not overlap can
	// never both match one path.
	sort.SliceStable(l.seeded, func(i, j int) bool {
		return len(l.seeded[i].match) > len(l.seeded[j].match)
	})
}

// sanitizeRouteLabel replaces every maximal byte sequence in path that is
// not valid UTF-8 with the Unicode replacement rune (U+FFFD), returning
// path unchanged -- with no allocation -- when path is already valid.
//
// This is the route label's third bound, and it exists because neither
// MaxRouteLabelValues nor MaxRouteLabelLength can see the failure it
// closes: net/http percent-decodes (*http.Request).URL.Path byte-wise, so
// a request target containing %FF (or any other invalid UTF-8 encoding)
// reaches Middleware with the raw invalid byte in the path and no parse
// error anywhere -- and, recorded verbatim as the http.route label, that
// byte makes the Prometheus exporter refuse the entire series on every
// Gather (client_golang validates label values: "label value ... is not
// valid UTF-8"). The scrape then answers 500 with zero metrics -- not just
// this middleware's, but every module's -- for the life of the process,
// because the offending data point is cumulative and the limiter has
// cached the raw path in its "seen" set. One unauthenticated request thus
// permanently kills /metrics: the same unauthenticated-DoS class the count
// and length caps exist for, through the dimension neither checks. The
// exporter-side half of the defense (promhttp.ContinueOnError, so a single
// bad series costs only itself) is wired in
// go/observability/exporter/prometheus; this function is the
// label-formation-side half that keeps the class from ever reaching the
// exporter through this middleware's route dimension. The span side of the
// same class -- the raw byte reaching the span name or otelhttp's own
// semconv attributes, where its cost is a dropped OTLP export batch rather
// than a voided scrape -- is bounded by the same function, through
// boundedRequestString (see Middleware's own "Why no span surface carries
// the raw request path" section).
//
// Replacement, rather than rejection or overflow-collapse, is the choice
// because the request still happened and must still be counted: an
// attacker's invalid path is attacker garbage, and counting it under a
// sanitized label keeps the count honest while remaining bounded (the
// sanitized value can never collide with a legitimate route, since a
// legitimate path is valid UTF-8 and passes through unchanged, and it can
// only shrink the distinct-value space by folding distinct invalid paths
// onto identical replacement text). The scan runs only over the
// already-truncated path (label truncates first), so per-request cost is
// bounded by MaxRouteLabelLength bytes even when the raw path is a full
// 1 MiB attacker request line.
func sanitizeRouteLabel(path string) string {
	if utf8.ValidString(path) {
		return path
	}
	s := strings.ToValidUTF8(path, "\uFFFD")
	if len(s) > MaxRouteLabelLength {
		return truncateRouteLabel(s)
	}
	return s
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

// boundedRequestString returns s run through the route label's length and
// UTF-8 bounds -- truncateRouteLabel (capped at MaxRouteLabelLength bytes,
// cutting on a rune boundary) then sanitizeRouteLabel (invalid UTF-8
// replaced with U+FFFD) -- the same two passes routeLabels.label runs on a
// request path before the route value is formed (see label's own doc
// comment for why both run and in that order). It exists for the raw
// request-controlled strings otelhttp's own server-span semconv attaches
// to the span it starts -- url.path, user_agent.original and
// client.address -- which Middleware's recording defer overwrites with
// this function's result (see Middleware's own "Why no span surface
// carries the raw request path" section).
//
// The route bound's third dimension, the distinct-value count cap, is
// deliberately NOT applied here: these are span attributes, not Prometheus
// series, so distinct-value count is not a resource dimension, and the
// actual path must stay findable per request. What must hold for every
// value is exporter safety plus a size ceiling: proto3 strings must be
// valid UTF-8, and the Go protobuf encoder fails the WHOLE OTLP export
// batch on one invalid string -- the same failure class sanitizeRouteLabel's
// own doc comment describes for the Prometheus label side, with the same
// root (net/http percent-decodes a request target byte-wise and applies no
// byte validation to header values, so invalid bytes reach this middleware
// with no parse error anywhere). Valid, short input passes through
// unchanged with no allocation.
func boundedRequestString(s string) string {
	return sanitizeRouteLabel(truncateRouteLabel(s))
}

// spanClientAddress mirrors the client-address selection otelhttp's own
// server-span semconv performs before attaching the client.address
// attribute to the span it starts (otelhttp@v0.69.0
// internal/semconv/server.go: the first X-Forwarded-For hop when the
// header is present, else the host half of RemoteAddr). Middleware needs
// the same value it is about to overwrite, so the overwrite re-derives it
// from the same request inputs rather than importing the dependency's
// unexported helper; the mirror is pinned end to end by exporter/otlp's
// TestMiddleware_InvalidUTF8Request_ExportBatchStillArrives and by
// middleware_test.go's
// TestMiddleware_InvalidUTF8Request_SpanNameAndAttributesCarryNoRawByte,
// so a future otelhttp change to this selection fails a test instead of
// silently overwriting a value the dependency chose differently. Returns
// "" when neither input yields an address, matching otelhttp's own
// attach condition (it attaches client.address only when its derivation
// is non-empty).
func spanClientAddress(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			xff = xff[:idx]
		}
		return xff
	}
	// A real server always supplies RemoteAddr as "IP:port"; an empty
	// result (or one that does not parse) means no peer address to mirror.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
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
//
// CodeQL's go/reflected-xss alert traces a notification verified_contacts
// address through pkgcore.Mail{To: [...]} into consoleMailer.Send's
// m.w.Write(remaining) (pkgcore/mailer_console.go) and merges that with this
// Write:
// reviewed and confirmed a false positive. consoleMailer.w is an arbitrary
// injected io.Writer (os.Stdout in production, never an HTTP response), and
// r.ResponseWriter here is a genuinely unrelated http.ResponseWriter
// instance from a real HTTP handler chain -- the two never share a real
// writer at runtime. CodeQL's interprocedural points-to analysis conflates
// them purely because both satisfy the identical Write([]byte) (int, error)
// signature. Separately: no handler in this codebase sets an HTML
// Content-Type on an HTTP response (grepped for text/html -- the only hits
// are outbound email bodies), and every http.Error call site (the only
// place request-derived text reaches a response body) goes through the
// oapi-codegen-generated boilerplate's call to Go's stdlib http.Error, which
// unconditionally forces Content-Type: text/plain; charset=utf-8 plus
// X-Content-Type-Options: nosniff -- so no reflected-XSS-shaped sink exists
// on this path regardless of the conflation above.
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
// endpoints the must-instrument metrics catalog anticipates)
// straight through this wrapper, per the standard library's own Go 1.20+
// ResponseWriter-wrapping convention.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
