package integration

import (
	"context"
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
)

// HeaderAPIKey is the header this module's Middleware reads a bearer API key
// from: "X-API-Key: <raw key>".
//
// # Why not "Authorization: Bearer <raw key>"
//
// That is the more common bearer-credential convention, and it is what this
// codebase's own go/authn.Middleware already reads -- and go/authn.
// Middleware is not merely A precedent to be consistent with, it is a hard
// constraint: it is mounted as the OUTERMOST layer of every route this
// reference app serves (examples/reference-app/cmd/server/server.go's own
// "handler := authn.Middleware(...)(topMux)"), and its own doc comment
// states its contract for a present-but-unverifiable bearer credential
// plainly: "A token that does not verify: 401 immediately... letting it
// fall through to \"anonymous\" would silently downgrade a tampered or
// expired credential into a public request." An API key's "sk_..." value is
// not a JWT go/authn's Verifier could ever accept, so reusing Authorization
// for it would make EVERY API-key-authenticated request 401 at go/authn.
// Middleware, before this module's own Middleware -- below it in the
// chain -- ever ran at all. This is a real, verified-in-tree conflict, not a
// hypothetical one: no composition of this reference app's existing
// middleware chain lets an API key share the Authorization header with
// go/authn's session bearer tokens.
//
// A distinct header sidesteps the conflict entirely: go/authn.Middleware
// only inspects Authorization, so a request carrying X-API-Key and no
// Authorization header proceeds through it with no Principal (its own
// documented "no Authorization header" case), then reaches this module's own
// Middleware exactly like a genuinely public, tenant-less request needs to
// (mirroring go/sharing.PathAccess's own allowlisted, Principal-less
// treatment in that same chain). A host mounting this module's own
// Authenticate-gated routes must therefore allowlist them in
// tenancy.Middleware (tenancy.WithAllowlist) the identical way
// sharing.PathAccess already is -- see examples/reference-app/cmd/server/
// server.go's own wiring for the real instance of this.
const HeaderAPIKey = "X-API-Key" //nolint:gosec // a header name, not a credential.

// authenticatedAPIKeyContextKey is the unexported context key Middleware
// stores an *AuthenticatedAPIKey under.
type authenticatedAPIKeyContextKey struct{}

// AuthenticatedAPIKeyFromContext returns the *AuthenticatedAPIKey Middleware
// attached to ctx after a successful Authenticate call, and whether one was
// present at all -- false for any request Middleware did not gate (or
// gated and refused, in which case next is never called and this question
// never arises for that request).
func AuthenticatedAPIKeyFromContext(ctx context.Context) (*AuthenticatedAPIKey, bool) {
	key, ok := ctx.Value(authenticatedAPIKeyContextKey{}).(*AuthenticatedAPIKey)
	return key, ok
}

// AuthMiddleware is the HTTP-transport translation of Service.Authenticate:
// a directly usable net/http middleware that extracts a bearer API key from
// HeaderAPIKey, authenticates it, and on success attaches both the resolved
// tenant (pkgcore.WithTenant) and the full *AuthenticatedAPIKey (retrievable
// via AuthenticatedAPIKeyFromContext) to the request context before calling
// next -- exactly the shape docs/internal/07-platform-services.md's design
// implies for "authenticating an inbound request with an API key" (the gap
// this round closes; see AGENTS.md's "In scope round 6" section).
//
// It holds a *Module, not a *Service, for the identical reason Handler does
// (handler.go's own "Round-1-only, and built differently" doc comment):
// go/integration's own Service is built in Module.Attach, strictly after
// Bootstrap's Register phase -- too late for a middleware a host may want to
// construct and wire during Register, alongside every other module's own
// route mounting. Reading m.service AT CALL TIME is the fix, the identical
// forwarding-wrapper technique Handler, handleDomainEvent and
// webhookDeliveryHandler already use.
//
// # Deliberately separate from HTTPGuard
//
// AuthMiddleware answers "who is this request, and may it proceed at all";
// HTTPGuard (httpguard.go) answers "has this already-identified caller
// exceeded its quota". They compose in that order -- AuthMiddleware first,
// so HTTPGuard's own Extractor can read the resolved tenant/key out of
// context that AuthMiddleware just attached, never re-deriving them itself
// (HTTPGuard "takes no position on how a host authenticates a request",
// per its own doc comment, and this module now supplies exactly one answer
// to that question without HTTPGuard needing to know it did). See
// examples/reference-app/cmd/server/server.go's wireIntegrationAuthenticated
// for the real, composed chain: AuthMiddleware -> HTTPGuard.Middleware ->
// the gated handler.
type AuthMiddleware struct {
	module *Module
}

// NewAuthMiddleware returns an AuthMiddleware reading m's Service at call
// time -- see the type's own doc comment for why m, not m.Service()
// directly, is what this constructor takes.
func NewAuthMiddleware(m *Module) *AuthMiddleware {
	return &AuthMiddleware{module: m}
}

// Middleware wraps next: a missing or empty HeaderAPIKey answers
// ErrAuthenticationFailed (401), matching Authenticate's own empty-string
// refusal so a request presenting NO credential is refused identically to
// one presenting a wrong one -- no observable "did you even try" signal.
// A Middleware whose Module has not yet been Attach'd (m.service == nil, the
// same race Handler's own doc comment describes) answers a coded internal
// error rather than panicking on a nil Service; see middleware_test.go's
// TestAuthMiddleware_ServedBeforeAttach_WritesInternalError.
func (a *AuthMiddleware) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.module.service == nil {
			writeAppError(w, ErrInternal)
			return
		}

		rawKey := r.Header.Get(HeaderAPIKey)
		authenticated, err := a.module.service.Authenticate(r.Context(), rawKey)
		if err != nil {
			writeAppError(w, err)
			return
		}

		ctx := pkgcore.WithTenant(r.Context(), pkgcore.TenantID(authenticated.TenantID))
		ctx = context.WithValue(ctx, authenticatedAPIKeyContextKey{}, authenticated)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
