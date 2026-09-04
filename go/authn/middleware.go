package authn

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"

	obs "github.com/vislake/speed/go/observability"
)

// authorizationHeader and bearerPrefix are the credential's transport. The
// scheme comparison is case-insensitive because RFC 7235 says the scheme
// token is.
const (
	authorizationHeader = "Authorization"
	bearerPrefix        = "bearer "

	// errorContentType is what the middleware writes its structured error
	// bodies as.
	errorContentType = "application/json; charset=utf-8"
)

// RevocationChecker reports whether a session has been signed out ahead of
// its access token's natural expiry. *SessionManager implements it; the
// interface exists so Middleware depends on the question rather than on the
// whole session machinery.
type RevocationChecker interface {
	// IsRevoked reports whether sessionID is revoked. An error means the
	// question could not be answered, which Middleware treats as a
	// refusal, never as a "no".
	IsRevoked(ctx context.Context, sessionID string) (bool, error)
}

// principalCtxKey addresses the Principal carried by a request context. A
// private key type is what keeps this value from colliding with anything
// another package puts in the same context.
type principalCtxKey struct{}

// WithPrincipal returns a copy of ctx carrying p. Handlers and tests use it;
// Middleware is the only thing in a running server that should.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the authenticated Principal carried by ctx.
// The second result is false for an unauthenticated request.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	if !ok || p.UserID == "" {
		return Principal{}, false
	}
	return p, true
}

// middlewareConfig accumulates the options passed to Middleware.
type middlewareConfig struct {
	revocation RevocationChecker
}

// MiddlewareOption configures Middleware.
type MiddlewareOption func(*middlewareConfig)

// WithRevocationChecker makes Middleware consult checker for every verified
// token, which is what turns RevocationModeImmediate from a stored list into
// an enforced one. Without it, revocation takes effect on the refresh path
// only and outstanding access tokens live out their natural lifetime -- which
// is exactly RevocationModeNatural, and is the right default.
func WithRevocationChecker(checker RevocationChecker) MiddlewareOption {
	return func(c *middlewareConfig) {
		if checker != nil {
			c.revocation = checker
		}
	}
}

// Middleware verifies the Authorization bearer token, if there is one, and
// puts the resulting Principal in the request context.
//
// It is OPTIONAL authentication, and the three cases are deliberately not the
// same:
//
//   - No Authorization header: the request proceeds with no Principal. Public
//     routes -- the login form's own configuration, registration, the token
//     endpoints, social callbacks -- have to work before anyone is
//     authenticated, and a global 401 would make them unreachable. Whether a
//     particular route tolerates that is the route's decision, enforced with
//     RequireAuthenticated.
//   - A token that does not verify: 401 immediately. This is an assertion of
//     identity that FAILED, which is not the same as an absence of one, and
//     letting it fall through to "anonymous" would silently downgrade a
//     tampered or expired credential into a public request.
//   - A token that verifies: the Principal goes in the context.
//
// Middleware NEVER calls pkgcore.WithTenant. Injecting the tenant is
// tenancy.Middleware's single job, and it takes it from this Principal via
// NewPrincipalResolver -- see that function for why the chain runs in this
// order.
func Middleware(verifier *Verifier, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	cfg := &middlewareConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, present := bearerToken(r)
			if !present {
				next.ServeHTTP(w, r)
				return
			}

			principal, err := verifier.Verify(r.Context(), raw)
			if err != nil {
				writeAppError(w, err)
				return
			}

			if cfg.revocation != nil {
				revoked, err := cfg.revocation.IsRevoked(r.Context(), principal.SessionID)
				if err != nil {
					// Fail closed. The alternative -- treat an
					// unreachable revocation list as "not
					// revoked" -- means a store outage
					// silently re-enables every session an
					// operator just signed out, which is the
					// one moment the guarantee matters.
					obs.FromContext(r.Context()).Error("session revocation check failed",
						"session_id", principal.SessionID, "error", err)
					writeAppError(w, ErrRevocationCheckFailed)
					return
				}
				if revoked {
					writeAppError(w, ErrSessionRevoked)
					return
				}
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// RequireAuthenticated refuses a request that carries no Principal.
//
// It is per-route rather than global on purpose: a module's public and
// private endpoints share one mounted handler, and the split between them is
// a property of the route, not of the module.
func RequireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := PrincipalFromContext(r.Context()); !ok {
			writeAppError(w, ErrAuthenticationRequired)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the credential from the Authorization header. The
// second result is false when there is no bearer credential to extract, which
// is not an error.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get(authorizationHeader)
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(bearerPrefix):])
	return token, token != ""
}

// PrincipalResolver adapts the verified Principal in a request context to
// tenancy.Resolver, so tenancy.Middleware injects the tenant the ACCESS TOKEN
// named and never one the client supplied.
//
// # Why the chain is authn.Middleware then tenancy.Middleware
//
// docs/internal/01-architecture.md draws the middleware chain the other way
// round. It cannot work that way, and the reason is in the Resolver signature
// itself: Resolve(r *http.Request) (pkgcore.TenantID, error) returns a tenant
// and nothing else. A resolver that verified the JWT would have no way to
// hand the claims it just validated to anything downstream, so the token
// would have to be verified a second time by authn.Middleware -- two
// verification paths over one credential, free to drift apart, with the
// tenant decided by the one that is NOT the one authorising the request.
//
// Running authn.Middleware first verifies once, and this resolver then reads
// the already-verified result. tenancy.Middleware keeps its single job (it,
// and only it, calls pkgcore.WithTenant) and keeps its fail-closed behaviour
// unchanged: Resolve returns an error whenever no Principal is present, so an
// unauthenticated request gets tenancy's own 403 unless its (method, path)
// pair is on the allowlist -- which is exactly how the pre-auth routes work.
type PrincipalResolver struct{}

// NewPrincipalResolver returns the resolver described above.
func NewPrincipalResolver() *PrincipalResolver { return &PrincipalResolver{} }

// Resolve implements tenancy.Resolver. It returns the tenant from the request
// context's Principal, and an error when there is none -- never a default,
// never an empty tenant reported as success.
func (*PrincipalResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		return "", ErrAuthenticationRequired
	}
	if principal.TenantID == "" {
		return "", ErrTokenInvalid
	}
	return principal.TenantID, nil
}

// errorBody is the {code, params} envelope every structured error is written
// as, matching the shape tenancy.Middleware already writes.
type errorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeAppError writes err as its structured envelope with its suggested HTTP
// status. Only the code and its parameters are written: an error's cause may
// carry token-validation internals or a database message, and internal detail
// must never reach a response body.
//
// A rate-limit or lockout error (ratelimit.go's ErrRateLimited,
// ErrAccountLocked) additionally gets a Retry-After header from its
// "retry_after_seconds" parameter -- the HTTP-specific translation of the
// underlying decision, and therefore this function's job rather than
// ratelimit.go's (docs/internal/11-cross-cutting.md). handler.go's HTTP
// surface shares this one implementation with Middleware and
// RequireAuthenticated above rather than writing its own, so every authn
// endpoint's error body has exactly one shape and exactly one place that
// decides what a Retry-After header is worth.
func writeAppError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal.WithCause(errors.Join(err))
	}
	if seconds, ok := retryAfterSeconds(appErr); ok {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	w.Header().Set("Content-Type", errorContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: appErr.Code, Params: appErr.Params})
}

// retryAfterSeconds extracts the "retry_after_seconds" parameter
// ratelimit.go's ErrRateLimited and ErrAccountLocked carry, reporting false
// when appErr carries none.
func retryAfterSeconds(appErr *apperr.Error) (int, bool) {
	if appErr.Params == nil {
		return 0, false
	}
	seconds, ok := appErr.Params["retry_after_seconds"].(int)
	return seconds, ok
}

// compile-time check that *PrincipalResolver satisfies tenancy.Resolver.
var _ tenancy.Resolver = (*PrincipalResolver)(nil)
