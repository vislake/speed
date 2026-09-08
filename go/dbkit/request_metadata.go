package dbkit

import "context"

// RequestMetadata carries the three request-context values an audit_events
// row can record about the request an audited action ran in: IP, UserAgent
// and TraceID (go/dbkit/audit/model.go's AuditEvent fields of the same
// names). The type exists so HTTP layers can stamp request context onto a
// context.Context the way pkgcore's WithActor/ActorFromContext pair stamps
// identity -- the shape this carrier deliberately mirrors -- and the audit
// write paths (dbkit's own write-capture plugin and go/dbkit/audit's Emit)
// can read it at capture time.
//
// An empty field means "not known for this action", never "known to be
// absent": a background job with no HTTP request behind it simply carries
// no RequestMetadata at all, and the audit rows its actions produce store
// the empty string for exactly the columns the schema declares NOT NULL
// with an empty-string default. Which values a host puts here (the client
// address of an HTTP request, the User-Agent header, the current trace id)
// is the populating layer's decision; this package only carries them.
type RequestMetadata struct {
	// IP is the client address of the request the audited action ran in.
	IP string
	// UserAgent is the request's User-Agent header, verbatim.
	UserAgent string
	// TraceID is the distributed-trace id the request's work ran under.
	TraceID string
}

// requestMetadataCtxKey is the unexported context key WithRequestMetadata
// installs RequestMetadata under. A dedicated unexported type, rather than
// a bare string, keeps this key from colliding with a key set by another
// package -- mirroring go/pkgcore/tenant.go's own ctxKey pattern.
type requestMetadataCtxKey struct{}

// WithRequestMetadata returns a copy of ctx carrying md as the request
// metadata of the action the context describes. Layered independently of
// every other carrier: setting it never clears or overwrites the actor,
// the tenant or anything else a context already carries.
func WithRequestMetadata(ctx context.Context, md RequestMetadata) context.Context {
	return context.WithValue(ctx, requestMetadataCtxKey{}, md)
}

// RequestMetadataFromContext returns the RequestMetadata carried by ctx.
// The second result is false when ctx carries none, in which case the
// first result is the zero RequestMetadata.
//
// Mirroring pkgcore.ActorFromContext's identical convention, a present
// RequestMetadata whose fields are all empty is still reported present as
// long as WithRequestMetadata was called: this function answers "was
// request metadata set on this context", not "does it look genuinely
// populated" -- the latter judgment, if a caller needs it, belongs to the
// caller.
func RequestMetadataFromContext(ctx context.Context) (RequestMetadata, bool) {
	md, ok := ctx.Value(requestMetadataCtxKey{}).(RequestMetadata)
	return md, ok
}
