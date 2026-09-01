package tenancy

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tenantErrorContentType is the Content-Type Middleware sets when it writes
// ErrTenantUnresolved as a response body.
const tenantErrorContentType = "application/json; charset=utf-8"

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
}

// WithAllowlist exempts the given (method, paths) combinations from tenant
// resolution. When resolver.Resolve fails for a request whose
// (*http.Request).Method exactly equals method AND whose
// (*http.Request).URL.Path exactly matches one of paths, Middleware calls
// the next handler with no tenant in the request context, instead of
// responding with 403 Forbidden.
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
// registration, health checks, public configuration -- per
// docs/internal/04-data-and-tenancy.md. Every (method, path) pair not
// listed here still fails closed on a resolution failure: Middleware never
// substitutes an empty tenant just to let a non-allowlisted request
// through.
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
// systems suffer a horizontal-privilege-escalation breach; see
// docs/internal/04-data-and-tenancy.md's section on the tenant context's
// trust boundary.
//
// When resolver.Resolve fails -- or reports success with an empty
// TenantID, which Middleware treats the same way -- the request is
// rejected with ErrTenantUnresolved as a 403 Forbidden response, unless its
// (method, path) pair was registered with WithAllowlist, in which case the
// request proceeds with no tenant in its context. Middleware never
// proceeds with a zero-value tenant on a non-allowlisted (method, path)
// pair: that is the forgotten-tenant-filter bug class that turns into a
// cross-tenant data leak, not a style choice.
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
				writeTenantUnresolved(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(pkgcore.WithTenant(r.Context(), tenantID)))
		})
	}
}

// tenantErrorBody is the JSON shape written for ErrTenantUnresolved,
// matching the {code, params} structured-error convention documented in
// docs/internal/11-cross-cutting.md.
type tenantErrorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeTenantUnresolved writes ErrTenantUnresolved to w as a JSON body.
// The Resolver's own error is deliberately never included in the body: it
// may carry internal detail from whatever produced it -- once authn
// supplies the authenticated-request Resolver, that could be
// token-validation internals -- and internal detail must never reach an
// API response.
func writeTenantUnresolved(w http.ResponseWriter) {
	w.Header().Set("Content-Type", tenantErrorContentType)
	w.WriteHeader(ErrTenantUnresolved.Status)
	_ = json.NewEncoder(w).Encode(tenantErrorBody{
		Code:   ErrTenantUnresolved.Code,
		Params: ErrTenantUnresolved.Params,
	})
}
