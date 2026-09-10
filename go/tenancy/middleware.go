package tenancy

import (
	"errors"
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"
)

// ErrTenantUnresolved is the structured error Middleware writes to the
// response when a request's tenant cannot be resolved and the request's
// (method, path) pair is not on the allowlist configured with
// WithAllowlist.
//
// Per the *apperr.Error contract this value is safe to hold as a
// package-level sentinel: WithParam and WithCause always derive a new
// instance rather than modifying this one, so decorating it per request
// (should a caller want to) cannot race.
var ErrTenantUnresolved = apperr.Forbidden("tenancy.tenant_unresolved")

// ErrTenantSuspended is the structured error Middleware writes to the
// response when a WithTenantStatusResolver-wired resolver reports a
// resolved tenant in any state other than TenantStatusActive:
// TenantStatusSuspended and equally any status outside the two-value
// vocabulary -- the empty status, a case variant, a future third state --
// reported with a nil error. Middleware refuses all of them the same way:
// only an active tenant is servable, so an out-of-vocabulary answer can
// never read as "no news is good news". It is never returned, and never
// even checked for, on a host that has not wired one -- see
// WithTenantStatusResolver's own doc comment for the full "off by
// default" contract this depends on.
var ErrTenantSuspended = apperr.Forbidden("tenancy.tenant_suspended")

// ErrTenantStatusUnavailable is the structured error Middleware writes to
// the response when a wired TenantStatusResolver's Status call itself
// fails. Exactly like a Resolver failure, this fails the request closed
// rather than assuming the tenant is active -- an unreachable status
// source must never be treated as "no news is good news".
var ErrTenantStatusUnavailable = apperr.Internal("tenancy.tenant_status_unavailable")

// errEmptyTenantResolved stands in for the error returned by a Resolver
// that reports success (a nil error) with a zero-value TenantID. A correct
// Resolver never does this, but Middleware treats it exactly like a
// resolution failure so a buggy Resolver cannot inject an empty tenant into
// the request context -- downstream code could otherwise mistake that for
// "no tenant filtering needed" rather than "tenant unknown".
var errEmptyTenantResolved = errors.New("tenancy: resolver reported success with an empty tenant")

// MiddlewareOption configures the behavior of Middleware.
type MiddlewareOption func(*middlewareConfig)

// allowlistKey identifies one exempted (method, path) combination. Keying
// on the pair -- rather than on path alone -- is deliberate: it is what
// lets WithAllowlist scope an exemption to a single HTTP method instead of
// silently exempting every method a path happens to serve.
type allowlistKey struct {
	method string
	path   string
}

// middlewareConfig accumulates the options passed to Middleware.
type middlewareConfig struct {
	allowlist map[allowlistKey]struct{}

	// statusResolver is nil unless WithTenantStatusResolver was applied --
	// the status check is skipped entirely when it is nil, which keeps an
	// unwired host on the plain resolution-plus-injection path.
	statusResolver TenantStatusResolver
}

// WithAllowlist exempts the given (method, paths) combinations from the
// refusals Middleware would otherwise answer with -- on a resolution
// failure and on a wired tenant-status gate's refusal of the resolved
// tenant alike -- never from resolution or tenant injection itself: an
// allowlisted request whose tenant resolves, and passes a wired gate, has
// that tenant injected like any other request. When resolver.Resolve
// fails for a request whose (*http.Request).Method exactly equals method
// AND whose (*http.Request).URL.Path exactly matches one of paths,
// Middleware calls the next handler with no tenant in the request
// context, instead of responding with 403 Forbidden.
//
// The exemption is scoped to method as well as path. Allowlisting
// (http.MethodPost, "/api/v1/orgs/invite") does NOT exempt GET, PUT,
// DELETE or any other method on that exact same path -- each needs its own
// WithAllowlist call (or its own entry in the same call) if it is also
// meant to be exempt. This matters because a spec-first REST API routinely
// puts multiple methods, with different trust requirements, on the exact
// same literal path (e.g. a public POST that creates an invite versus a
// GET on that same path that must resolve a tenant to read one back).
// Treating a path as single-method just because only one of its methods
// needs to be public is exactly the mistake this scoping prevents.
//
// Reserve this for routes that must work before a tenant can be known --
// registration, health checks, public configuration. Every (method, path)
// pair not listed here still fails closed on a resolution failure:
// Middleware never substitutes an empty tenant just to let a
// non-allowlisted request through.
//
// The exemption covers the tenant-status gate (WithTenantStatusResolver)
// the same way: when resolution succeeds but the resolved tenant's status
// refuses the request -- suspended, or any status other than
// TenantStatusActive -- or the Status call itself fails, an allowlisted
// request proceeds with no tenant in its context instead of receiving
// ErrTenantSuspended or ErrTenantStatusUnavailable. A route exempted
// because it must work regardless of tenant state stays up when the
// resolved tenant's state or the status source itself would take other
// routes down.
//
// Matching is an exact string comparison against both Method and URL.Path;
// there is no prefix, wildcard, case-folding or trailing-slash
// normalization for either. In particular Middleware does not apply
// net/http's GET-implies-HEAD convenience -- allowlist http.MethodHead
// explicitly if a health check needs it too.
func WithAllowlist(method string, paths ...string) MiddlewareOption {
	return func(c *middlewareConfig) {
		for _, p := range paths {
			c.allowlist[allowlistKey{method: method, path: p}] = struct{}{}
		}
	}
}

// Middleware returns net/http middleware that resolves the current
// request's tenant with resolver and, on success, injects it into the
// request context with pkgcore.WithTenant, so downstream handlers read it
// back with pkgcore.TenantFromContext (or fail closed with
// pkgcore.MustTenantFromContext).
//
// The resolved tenant is the ONLY tenant source downstream code may trust.
// Middleware never reads a tenant from a request header, query parameter or
// body -- under any option, in any configuration. Accepting a
// client-supplied tenant_id is the single most common way multi-tenant
// systems suffer a horizontal-privilege-escalation breach; a custom
// Resolver must uphold the same rule and derive the tenant from a source
// the server itself controls (see Resolver's doc comment).
//
// When resolver.Resolve fails -- or reports success with an empty
// TenantID, which Middleware treats the same way -- the request is
// rejected with ErrTenantUnresolved as a 403 Forbidden response, unless its
// (method, path) pair was registered with WithAllowlist, in which case the
// request proceeds with no tenant in its context. Middleware never
// proceeds with a zero-value tenant on a non-allowlisted (method, path)
// pair: that is the forgotten-tenant-filter bug class that turns into a
// cross-tenant data leak, not a style choice.
//
// When WithTenantStatusResolver was given, a successfully resolved tenant
// is additionally checked against it, and TenantStatusActive is the only
// status that lets the request through: TenantStatusSuspended rejects it
// with ErrTenantSuspended, and so does any other status reported with a
// nil error -- the empty status, a case variant or a future state this
// version does not define are refused, never read as "assume active", the
// same default-refuse discipline errEmptyTenantResolved applies to a
// Resolver's out-of-contract success. A Status call that itself errors
// rejects with ErrTenantStatusUnavailable. Both refusal classes are
// fail-closed, matching Resolver's own discipline, and both consult the
// allowlist the way a resolution failure does: an allowlisted request
// whose resolved tenant is refused proceeds with no tenant in its
// context, never with the tenant the status gate just refused. With no
// TenantStatusResolver wired (the default), this check never runs at
// all, and Middleware is the plain resolution-plus-injection described
// above.
func Middleware(resolver Resolver, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	cfg := &middlewareConfig{allowlist: make(map[allowlistKey]struct{})}
	for _, opt := range opts {
		opt(cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, err := resolver.Resolve(r)
			if err == nil && tenantID == "" {
				err = errEmptyTenantResolved
			}
			if err != nil {
				if _, allowed := cfg.allowlist[allowlistKey{method: r.Method, path: r.URL.Path}]; allowed {
					next.ServeHTTP(w, r)
					return
				}
				writeError(w, ErrTenantUnresolved)
				return
			}

			ctx := pkgcore.WithTenant(r.Context(), tenantID)
			if cfg.statusResolver != nil {
				status, statusErr := cfg.statusResolver.Status(ctx, tenantID)
				if statusErr != nil || status != TenantStatusActive {
					if _, allowed := cfg.allowlist[allowlistKey{method: r.Method, path: r.URL.Path}]; allowed {
						// The same escape the resolution-failure branch
						// gives an allowlisted request: proceed with no
						// tenant in the context -- never the tenant the
						// status gate just refused. A route exempted
						// because it must work regardless of tenant state
						// stays up whatever this gate would otherwise do.
						next.ServeHTTP(w, r)
						return
					}
					if statusErr != nil {
						writeError(w, ErrTenantStatusUnavailable)
					} else {
						// TenantStatusActive is the only status that lets a
						// request through: TenantStatusSuspended and
						// anything else reported with a nil error -- the
						// empty status, a case variant, a future state this
						// version does not define -- refuse the same way, an
						// out-of-vocabulary answer never reading as "assume
						// active".
						writeError(w, ErrTenantSuspended)
					}
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// writeError writes appErr to w as the coded error envelope (see
// pkgcore/httpapi) -- ErrTenantUnresolved, ErrTenantSuspended or
// ErrTenantStatusUnavailable, Middleware's three possible refusals. The
// underlying Resolver/TenantStatusResolver error, if any, is deliberately
// never included in the body: it may carry internal detail from whatever
// produced it -- an authenticated-request Resolver's token-validation
// internals, for one -- and internal detail must never reach an API
// response. A typed *apperr.Error is always coded, so the fallback (used
// only for a non-*apperr.Error) is the value itself.
func writeError(w http.ResponseWriter, appErr *apperr.Error) {
	httpapi.WriteError(w, appErr, appErr)
}
