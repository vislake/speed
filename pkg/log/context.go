package log

import (
	"context"
	"log/slog"
)

// contextKey is this package's key in a context. It is an unexported type, so
// nothing outside can collide with it or read the logger back out by
// constructing the key.
type contextKey struct{}

// FromContext returns the logger carried by ctx, or the process default logger
// when it carries none.
//
// The fallback neither panics nor returns a logger that drops records: a log
// call has to hold on any context, and the record still passes the redaction
// layer. What it does not do is reach the configured destinations — the
// default logger writes to standard output, and is filtered by the bootstrap
// level rather than by the level this run was configured with.
//
// A missing injection therefore shows up as the whole path's logs appearing on
// standard output instead of in the configured file. That is deliberate: it is
// visible without producing a failure signal, and it is fixed by injecting a
// logger where the context is created, not by widening the fallback.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return Default()
}

// WithLogger returns a context carrying l. ctx is unchanged.
//
// The logger to inject is the one taken from this module's product through
// Named, not Default(): injecting the default logger would take the whole path
// off the configured destinations. Injection belongs to whoever creates the
// context — the HTTP entry point, the job runner — not to a middleware, which
// would leave several parties each believing they are the fallback.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// WithAttrs binds attributes to the logger in ctx and returns a context
// carrying the result: the take, bind and put back that an injection point
// does, written once.
//
// The attributes are bound rather than passed with each record, so they are
// evaluated and judged for redaction once and reused by every record of that
// request. On a context with no logger the attributes accumulate on the
// default logger, so nothing is lost, but the path stays on the bootstrap
// chain.
func WithAttrs(ctx context.Context, args ...any) context.Context {
	return WithLogger(ctx, FromContext(ctx).With(args...))
}
