package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"

	"github.com/vislake/speed/go/pkgcore"
)

// TraceIDKey, SpanIDKey and TenantIDKey are the structured log field keys
// FromContext attaches, and the span attribute key AnnotateTenant attaches.
// They are shared, stable, snake_case names per root CLAUDE.md's logging
// rule (field names are snake_case and consistent across the whole stack:
// tenant_id, user_id, job_id, trace_id, ... one place spelling it userId
// and another uid makes logs unqueryable), exported so other modules that
// need to name the same field -- in a log call of their own, or a query
// against Loki -- spell it identically rather than inventing their own
// casing.
const (
	TraceIDKey  = "trace_id"
	SpanIDKey   = "span_id"
	TenantIDKey = "tenant_id"
)

// loggerCtxKey is the unexported context key WithLogger stores a logger
// under. A dedicated unexported type, rather than a bare string, keeps this
// key from colliding with a key set by another package, mirroring
// go/pkgcore/tenant.go's own ctxKey pattern.
type loggerCtxKey struct{}

// WithLogger returns a copy of ctx carrying logger, so that a later
// FromContext call on ctx (or on any context derived from it) returns
// logger -- enriched with whatever trace/tenant fields that later call's
// own context carries, and wrapped in the redaction layer (see redact.go) --
// instead of falling back to slog.Default().
//
// logger is used as a plain sink base: build it as slog.New(handler), with
// no With-attached attributes. Attributes pre-attached to it live in the
// handler's own state and would be rendered by the sink without passing
// through FromContext's redactor; static attributes belong on the logger
// FromContext returns instead (logger.With(...) on that one IS redacted).
//
// This is the principal place a hand-built *slog.Logger gets attached:
// process startup, before any request, job or trace context exists to
// derive one from. The call site this package expects is
// examples/reference-app/cmd/server/main.go's run function, which
// attaches a logger to the background context before anything else is
// wired, so every background-context log line during startup still goes
// through FromContext rather than a raw slog.Default() call sprinkled
// through main.go. Every other caller of FromContext -- inside a
// request, a job, an event handler -- already has a context that carries
// everything FromContext needs automatically; threading a logger through
// WithLogger there would only defeat that and is not what this function
// is for. Any other genuinely context-less special case may attach a
// hand-built logger here too (tests are the everyday one): the shared
// rules -- root CLAUDE.md's take-the-logger-from-the-context rule and
// backend-coding-standards.md §11's never-a-fresh-logger-inside-a-
// request-path rule -- exclude exactly the sites where a context exists,
// and the startup confinement of hand-built loggers is this module's own
// rule (see FromContext's doc comment and this module's AGENTS.md), not
// a prohibition on every non-startup site.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey{}, logger)
}

// baseLogger returns the logger WithLogger attached to ctx, or
// slog.Default() when none was attached. slog.Default() is read fresh on
// every call rather than cached at package-init time, so a caller that
// invokes slog.SetDefault after importing this package -- tests are the
// common case -- is still honored.
func baseLogger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerCtxKey{}).(*slog.Logger); ok && logger != nil {
		return logger
	}
	return slog.Default()
}

// FromContext returns the *slog.Logger every module must log through,
// enriched with whatever of the following ctx carries:
//
//   - trace_id and span_id, when ctx holds an active OTel span (checked via
//     the span's own SpanContext, so this works for a span from ANY
//     TracerProvider -- the local exporters' stdout one, the OTLP
//     exporters' one, or a test's in-memory one -- FromContext itself
//     never touches a provider);
//   - tenant_id, when ctx holds one, via pkgcore.TenantFromContext. Its
//     second, ok result is handled exactly like TenantFromContext's own
//     contract: no tenant is not an error, so the field is simply omitted
//     rather than the call failing.
//
// This is the ONLY sanctioned way to obtain a logger inside request-scoped
// or job-scoped code (root CLAUDE.md's rule that the logger comes from the
// context; backend-coding-standards.md §11's never-a-fresh-logger-inside-a-
// request-path rule): constructing a fresh *slog.Logger deep inside a
// request path loses this correlation, and a later log line can no longer
// be traced back to the request, trace or tenant it belongs to. Both conditions above are independently optional,
// which is what makes this safe to call from a background job with no
// active span, or before tenancy.Middleware has resolved a tenant (an
// allowlisted route, or a request still upstream of that middleware), just
// as it is from deep inside a fully-scoped request: the fields that do not
// apply are simply left off, never replaced with a placeholder or an
// error.
//
// # Redaction
//
// Every logger FromContext returns is wrapped in the redaction layer
// described in redact.go: sensitive attributes are replaced before the
// record reaches the sink handler -- on by default, with no per-call way
// to disable it, and identically for every sink a host plugs in. The key
// set, the value shapes detected, and the deliberate boundaries are all in
// redact.go's doc comment.
//
// Two contract points follow from the wrap:
//
//   - FromContext does not return the exact *slog.Logger WithLogger
//     attached: it returns a logger that shares the attached one's sink
//     handler with the redaction layer in between. When ctx carries no
//     enrichment, the returned logger is still redacted, and two calls
//     return distinct *slog.Logger values that write identically.
//   - The redaction guarantee holds for everything logged through this
//     API. A *slog.Logger a host constructs by hand and logs through
//     directly -- outside FromContext -- does not pass through the
//     redactor; that construction site is the deliberate escape, confined
//     by this module's own rules to the genuinely context-less sites a
//     hand-built logger is legitimate at: process startup above all, plus
//     any other special case that has no context to derive a logger from
//     (see WithLogger's doc comment). Root CLAUDE.md and
//     backend-coding-standards.md are not the source of that confinement;
//     they only require the context logger where a context exists.
func FromContext(ctx context.Context) *slog.Logger {
	logger := baseLogger(ctx)

	var attrs []any
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		attrs = append(attrs, TraceIDKey, sc.TraceID().String(), SpanIDKey, sc.SpanID().String())
	}
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		attrs = append(attrs, TenantIDKey, string(tenant))
	}

	return redactedLogger(logger).With(attrs...)
}

// redactedLogger returns logger with its handler wrapped in the redaction
// layer, or logger itself when its handler is already wrapped. It is the
// single funnel through which the loggers this package hands out become
// redaction-guaranteed: wrapping happens here, once, and every record any
// derived logger emits (its own log calls and anything attached to it via
// With, whose attributes this layer redacts when they are attached) passes
// through the redactor before the sink sees it. See redact.go for the
// full mechanism.
func redactedLogger(logger *slog.Logger) *slog.Logger {
	if _, ok := logger.Handler().(*redactHandler); ok {
		return logger
	}
	return slog.New(&redactHandler{next: logger.Handler()})
}
