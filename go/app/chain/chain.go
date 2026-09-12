// Package chain assembles the platform's fixed HTTP middleware chain --
// the order every speed host's request path follows -- around a host's own
// protected handler and route branches. It is the composition the app
// module's charter documents: no policy of its own beyond the platform
// order, no state, no infrastructure construction. docs/internal/
// 01-architecture.md's fixed-middleware-order paragraph is the order's
// design authority; this package is its one implementation.
//
// Two entry points compose it. Chain takes the host's pieces directly: the
// verifier, the protected handler and the two structurally exempt route
// branches, which the host has partitioned and mounted itself -- the path
// for a host with a custom route layout. Standard derives the same
// composition from the assembly's accumulated declarations (the http
// component's product, go/app/httpserve): it admits every mounted route
// through the host's route-authorization table, partitions the authn
// subtree and the admin subtree out of it, mounts the rest on the host's
// protected mux, and delegates to Chain; it then wraps the declared
// platform middleware around the finished chain -- the one layer that
// stands OUTSIDE the fixed order, never inside it (Standard's own doc
// comment states that boundary and the contract a middleware on that layer
// holds to).
package chain

import (
	"fmt"
	"net/http"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// Config parameterizes Chain: the host's own pieces of the fixed
// middleware chain, everything the platform cannot know on its own.
//
// Verifier, Protected and AuthnRoutes are required; the rest are optional
// parameters of the composition and are left unset by a host whose module
// set does not include the module they come from (a host with no admin
// module wires neither Impersonation nor AdminRoutes nor
// TenantStatusResolver).
type Config struct {
	// Verifier is the token verifier Chain wraps the whole composition
	// in: authn.Middleware(Verifier), the outermost layer. Required --
	// it is what turns a bearer token into the verified
	// authn.Principal every layer below reads.
	Verifier *authn.Verifier

	// Protected is the handler every request that is not exempt by
	// structure (AuthnRoutes, AdminRoutes) reaches through the tenancy
	// chain: the host's own route mux, with obs.MountLiveness and every
	// guarded module route already mounted on it. Required.
	Protected http.Handler

	// AuthnRoutes is authn's own HTTP subtree -- typically what
	// authn.ExemptSubtree splits out of the guarded module route set --
	// dispatched from authn.Middleware's output AHEAD of the tenancy
	// chain, ungated. Required, and must be non-empty: a composition that
	// calls Chain has an authn module (Verifier has to come from one),
	// and a mounted authn surface that silently fell through to the
	// tenancy chain would refuse every pre-auth authn operation with
	// tenancy.tenant_unresolved.
	AuthnRoutes []pkgcore.MountedRoute

	// AdminRoutes is admin's own mounted route (its operator console),
	// dispatched from authn.Middleware's output ahead of both the
	// impersonation decorator and the tenancy chain. Empty for a host
	// with no admin module.
	//
	// The dedicated branch is load-bearing, not cosmetic: admin's
	// permissions are evaluated in rbac.SystemDomain against the
	// CALLER'S OWN real, unsubstituted Principal, so admin's surface must
	// sit behind neither tenancy.Middleware's tenant resolution nor
	// Impersonation's identity substitution (go/admin/AGENTS.md states
	// both). Letting either run first would let an ordinary tenant's own
	// Owner role, or an impersonated identity, reach admin's console.
	AdminRoutes []pkgcore.MountedRoute

	// Impersonation, when non-nil, is applied between authn.Middleware and
	// the tenancy chain -- its one correct position. It is the host's own
	// decorator, built from whichever module provides identity
	// substitution: a host with admin wired passes
	// admin.ImpersonationMiddleware(adminModule.Impersonation()), a host
	// with no such module leaves this nil. Deliberately a plain
	// middleware value rather than a module-typed parameter, so this
	// package's dependency closure stays bounded by the chain itself (a
	// host that never wires impersonation pays for no impersonation
	// module).
	//
	// The expected decorator never reorders the chain: it reads the real,
	// already-verified authn.Principal authn.Middleware just installed,
	// and -- only when the request carries a valid impersonation grant id
	// -- substitutes a Principal naming the impersonation target for
	// everything downstream, including tenancy.Middleware's own tenant
	// resolution. A request with no such header, or an invalid one, is
	// unaffected: the decorator is a no-op for every route unless an
	// operator has actually started an impersonation session.
	Impersonation func(http.Handler) http.Handler

	// TenantStatusResolver, when non-nil, is wired into the tenancy chain
	// as tenancy.WithTenantStatusResolver: it turns "an operator marked a
	// tenant suspended" into every route actually refusing that tenant's
	// requests on the very next one, rather than a ledger fact nothing
	// downstream consults. admin's own tenant ledger structurally
	// satisfies it, but the interface does not know admin exists -- the
	// identical no-import-in-either-direction shape org.FeatureGate uses.
	TenantStatusResolver tenancy.TenantStatusResolver

	// ExtraAllowlist is the host's own additions to the pre-auth
	// allowlist chain appends to app.PreAuthAllowlist(): a genuinely public
	// route the host mounts (a token-addressed share link, an
	// API-key-authenticated whoami endpoint, a POST-only invitation
	// acceptance) that resolves its own tenant server-side once the
	// allowlist lets the request reach it at all. Scope each entry to the
	// exact method the route serves -- tenancy.WithAllowlist matches
	// (method, path) exactly.
	ExtraAllowlist []tenancy.MiddlewareOption
}

// Chain assembles the fixed middleware chain around cfg.Protected and
// returns the composed handler. The order it encodes is
// docs/internal/01-architecture.md's fixed order -- authn, then the
// optional impersonation decorator, then tenancy with the pre-auth
// allowlist -- with the two structurally exempt branches dispatched ahead
// of the tenancy chain:
//
//	authn.Middleware(verifier)                      outermost
//	  ├── AdminRoutes        (dispatched from its output; no tenancy, no impersonation)
//	  ├── AuthnRoutes        (dispatched from its output; no tenancy)
//	  └── "/" -> Impersonation (optional) -> tenancy.Middleware(+allowlist) -> Protected
//
// authn runs FIRST because it is the only order that verifies a token
// exactly once: a tenancy.Resolver's signature cannot hand a verified
// JWT's claims to anything downstream, so running tenancy first would
// force verifying every token twice over two code paths free to drift --
// and the path that ends up deciding would be the one that authenticated
// nothing. Verified once, tenancy.Middleware(authn.NewPrincipalResolver())
// reads the already-verified Principal out of the request context and
// remains the only place pkgcore.WithTenant is called. authn.Middleware
// is OPTIONAL auth (a missing token proceeds with no Principal; an
// invalid one 401s immediately), so tenancy.Middleware's fail-closed
// default -- refuse any request whose (method, path) is not allowlisted
// AND whose resolver failed -- is what makes every route mounted on
// Protected require a valid Principal with no per-route wrapping; the
// allowlisted paths (PreAuthAllowlist plus cfg.ExtraAllowlist) are the
// only ones that work with no Principal at all.
//
// The gate every route mounted on Protected carries above the tenancy
// chain -- rbac's route-authorization table (rbac.GuardRoutes, applied by
// the host before mounting) and, inside the modules that gate on plan
// entitlements, their own checks riding the ai-gateway seams -- is the
// next step of the documented order; it lives at the route level, not in
// this chain, because each host's decision table is host-specific.
//
// The chain returns an error, never a partially composed handler, when
// cfg is missing a required piece or AuthnRoutes is empty.
func Chain(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Verifier == nil:
		return nil, fmt.Errorf("chain: Verifier is required (authn.Middleware wraps the whole composition in it)")
	case cfg.Protected == nil:
		return nil, fmt.Errorf("chain: Protected is required (the host's own route mux)")
	case len(cfg.AuthnRoutes) == 0:
		return nil, fmt.Errorf("chain: AuthnRoutes is empty; the authn subtree must be mounted ahead of the tenancy chain, not left to fall through it")
	}

	allowlist := append(app.PreAuthAllowlist(), cfg.ExtraAllowlist...)
	if cfg.TenantStatusResolver != nil {
		allowlist = append(allowlist, tenancy.WithTenantStatusResolver(cfg.TenantStatusResolver))
	}

	rest := tenancy.Middleware(authn.NewPrincipalResolver(), allowlist...)(cfg.Protected)
	if cfg.Impersonation != nil {
		rest = cfg.Impersonation(rest)
	}

	topMux := http.NewServeMux()
	branches := make([]pkgcore.MountedRoute, 0, len(cfg.AdminRoutes)+len(cfg.AuthnRoutes))
	branches = append(branches, cfg.AdminRoutes...)
	branches = append(branches, cfg.AuthnRoutes...)
	// pkgcore.MountRoutes registers both branches at their exact paths and
	// below them; see its own doc comment for why the dual registration is
	// the only correct shape.
	pkgcore.MountRoutes(topMux, branches...)
	topMux.Handle("/", rest)

	return authn.Middleware(cfg.Verifier)(topMux), nil
}
