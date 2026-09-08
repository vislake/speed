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
// next -- exactly the shape the design implies for "authenticating an
// inbound request with an API key".
//
// It holds a *Module, not a *Service, for the identical reason Handler does
// (handler.go's own doc comment):
// go/integration's own Service is built in Module.Attach, strictly after
// Bootstrap's Register phase -- too late for a middleware a host may want to
// construct and wire during Register, alongside every other module's own
// route mounting. Reading m.service AT CALL TIME is the fix, the identical
// forwarding-wrapper technique Handler, handleDomainEvent and
// webhookDeliveryHandler already use.
//
// # Deliberately separate from HTTPGuard -- and rate-limited BEFORE it
// # authenticates
//
// AuthMiddleware answers "who is this request, and may it proceed at all";
// HTTPGuard (httpguard.go) answers "has this caller exceeded its quota".
// The two compose around authentication, and the guard belongs OUTSIDE it:
// a request that presents a forged or unrecognized X-API-Key header would
// otherwise reach Authenticate's lookups with no bound at all -- it is
// refused by this middleware, but refused AFTER paying for two database
// lookups, and never charged against any rate-limit budget, since a guard
// mounted behind authentication never even sees it. A host wiring a guard
// manually therefore mounts it FIRST (guard.Middleware -> this middleware
// -> the gated handler), and a host that wires WithAuthenticationGuard on
// the Module gets exactly that ordering without composing it: the guard
// evaluates before any Authenticate call and its denial answers 429 with
// the request never reaching authentication. When the guard runs before
// authentication its Extractor cannot read a resolved tenant/key out of
// context (nothing has resolved one yet); HTTPGuard's Extractor contract
// treats empty identifiers as ordinary keys, so an extractor that returns
// them for a not-yet-authenticated request charges the global layer and
// the shared anonymous counters -- see WithAuthenticationGuard's own doc
// comment for the full pre-auth identifier discussion, which follows
// authn's login-limit precedent of limiting attempts before verifying
// credentials. See examples/reference-app/cmd/server/server.go's
// wireIntegrationAuthenticated for the composed chain a host mounts.
type AuthMiddleware struct {
	module *Module
}

// NewAuthMiddleware returns an AuthMiddleware reading m's Service at call
// time -- see the type's own doc comment for why m, not m.Service()
// directly, is what this constructor takes.
func NewAuthMiddleware(m *Module) *AuthMiddleware {
	return &AuthMiddleware{module: m}
}

// Middleware wraps next with the authenticate-or-refuse gate: a missing or
// empty HeaderAPIKey answers ErrAuthenticationFailed (401), matching
// Authenticate's own empty-string refusal so a request presenting NO
// credential is refused identically to one presenting a wrong one -- no
// observable "did you even try" signal. A Middleware whose Module has not
// yet been Attach'd (m.service == nil, the same race Handler's own doc
// comment describes) answers a coded internal error rather than panicking
// on a nil Service; see middleware_test.go's
// TestAuthMiddleware_ServedBeforeAttach_WritesInternalError.
//
// When the Module carries a WithAuthenticationGuard guard, the guard runs
// FIRST, wrapping the authenticate gate itself -- so the request pays the
// rate-limit budget before any Authenticate call, and a denied request
// answers 429 without authentication ever running (see the type's own
// "Deliberately separate from HTTPGuard" doc section).
func (a *AuthMiddleware) Middleware(next http.Handler) http.Handler {
	authenticate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// m.service is genuinely read per request, INSIDE this closure: it
		// does not exist until Attach, which runs after Register -- the
		// moment a host typically builds the chain by calling Middleware --
		// so only a served request can know whether it exists yet. The guard
		// below is the opposite shape.
		if a.module == nil || a.module.service == nil {
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

	// The guard is read ONCE, here, while Middleware is assembling the chain
	// it returns -- never per request. Requests served through that chain
	// are already inside the guard; a request-time re-read could not
	// recompose the chain this wrap just finalized. See the Module's own
	// authGuard field comment for the full contrast with the per-request
	// reads above.
	if a.module != nil && a.module.authGuard != nil {
		return a.module.authGuard.Middleware(authenticate)
	}
	return authenticate
}
