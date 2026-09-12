package chain

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// RouteSource is what Standard reads: the mounted route set it derives the
// chain's route partition from, and the platform middleware the assembly
// declared. The value a boot passes is the http component's product
// (go/app/httpserve: its *Face answers both readings, and the same product
// is what the declaration faces accumulated into); the interface stays
// structural here so this package keeps a bounded dependency closure and a
// test can hand in a fixture.
type RouteSource interface {
	// MountedRoutes returns every route the assembled components mounted,
	// in registration order.
	MountedRoutes() []pkgcore.MountedRoute
	// Middlewares returns every middleware declared through the middleware
	// face, in registration order: Standard applies them as the assembled
	// chain's outermost layer, first registered outermost. See
	// pkgcore.MiddlewareRegistrar for the face's contract and its boundary
	// -- the layer stands outside the fixed chain, and the face offers no
	// way to insert into it.
	Middlewares() []func(http.Handler) http.Handler
}

// Standard derives the fixed middleware chain from the assembly's route
// source (the http component's product, go/app/httpserve) instead of from a
// hand-partitioned route set: it admits every mounted
// route through the host's route-authorization table (rbac.GuardRoutes),
// splits the authn subtree out with authn.ExemptSubtree, splits the admin
// subtree out by the prefix the host declares, mounts everything else on
// protected, and hands the result to Chain -- the order itself stays Chain's
// one implementation.
//
// protected is the host's own protected-face handler (typically the mux the
// assembling component prepared, already carrying the platform liveness
// routes and the host's own routes); Standard mounts the source's non-exempt
// routes onto it, so it must be a *http.ServeMux and must not already carry
// those routes. The partition is validated in full before the first route is
// mounted, and a refused composition mounts nothing: Standard's error return
// leaves protected exactly as it was.
//
// The host-supplied half of the composition travels through options: the
// authorizer and rule table (WithAuthorization -- the business decision of
// which permission each route requires), the admin module's mount prefix
// (WithAdminPrefix, so the admin subtree is split by the module's own
// constant rather than a copy), the impersonation decorator
// (WithImpersonation, built from whichever module provides identity
// substitution), the tenant-status gate (WithTenantStatusResolver) and the
// host's extra pre-auth entries (WithExtraAllowlist).
//
// The rbac authorization domain weaves per route here, at mount time: each
// route the table decides is wrapped in rbac's permission gate before it is
// mounted, so a request meets the gate between the mux dispatch and the
// module handler. The billing quota domain does not weave here -- billing's
// quota mechanism is the entitlements seam checked inside go/ai-gateway
// before a provider is reached, wired at module construction
// (aigateway.WithEntitlements), not a route-level decorator.
//
// The platform middleware the assembly declared rides OUTSIDE the derived
// chain: after Chain produces the fixed composition, Standard wraps the
// middleware face's entries around the finished handler in registration
// order, first registered outermost. The layer is deliberately outside
// everything -- outside authn.Middleware, outside the exempt branches -- so
// a middleware there wraps every request the chain handles, including one
// authn or tenancy refuses before any route is reached. A request the host
// answers outside this handler -- a frontend file server wrapped around it,
// say -- never reaches the layer. At that position the middleware runs on
// the raw, unauthenticated request: no authn.Principal and no tenant
// context exist there, so the work must be stateless bypass work (tracing,
// metrics, panic recovery) -- see pkgcore.MiddlewareRegistrar for the
// face's contract.
//
// The fixed order inside the chain is NOT reachable through the face:
// authn.Middleware stays outermost of the chain, the AdminRoutes and
// AuthnRoutes branches keep their structural exemptions, and
// tenancy.Middleware with its pre-auth allowlist keeps its place -- a
// middleware declared on the face can only ever wrap the chain's finished
// output. The chain's own insertion points stay what they are (the
// impersonation decorator, the tenant-status resolver, the extra allowlist
// entries); none of them is this face.
func Standard(src RouteSource, verifier *authn.Verifier, protected *http.ServeMux, opts ...Option) (http.Handler, error) {
	cfg := &standardConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if src == nil {
		return nil, fmt.Errorf("chain: the route source is required (Standard derives the route partition from the mounted module routes)")
	}
	if protected == nil {
		return nil, fmt.Errorf("chain: protected is required (Standard mounts the non-exempt module routes on it)")
	}

	routes := src.MountedRoutes()
	if (cfg.az == nil) != (len(cfg.rules) == 0) {
		return nil, fmt.Errorf("chain: the route-authorization table and its authorizer are declared together; WithAuthorization takes both or neither")
	}
	if cfg.az != nil {
		guarded, err := rbac.GuardRoutes(cfg.az, routes, cfg.rules)
		if err != nil {
			return nil, fmt.Errorf("chain: %w", err)
		}
		routes = guarded
	}

	authnRoutes, rest := authn.ExemptSubtree(routes)
	var adminRoutes, protectedRoutes []pkgcore.MountedRoute
	for _, route := range rest {
		if cfg.adminPrefix != "" && routeIsBelow(route.Path, cfg.adminPrefix) {
			adminRoutes = append(adminRoutes, route)
			continue
		}
		protectedRoutes = append(protectedRoutes, route)
	}
	if len(authnRoutes) == 0 {
		return nil, fmt.Errorf("chain: no mounted route lies at or below authn's mount point; authn's subtree must be mounted ahead of the tenancy chain, and a host composing Standard has an authn module")
	}
	if cfg.adminPrefix != "" && len(adminRoutes) == 0 {
		return nil, fmt.Errorf("chain: the admin prefix %q matches no mounted route; the module mounting that surface must be in the registry's module set", cfg.adminPrefix)
	}
	pkgcore.MountRoutes(protected, protectedRoutes...)

	handler, err := Chain(Config{
		Verifier:             verifier,
		Protected:            protected,
		AuthnRoutes:          authnRoutes,
		AdminRoutes:          adminRoutes,
		Impersonation:        cfg.impersonation,
		TenantStatusResolver: cfg.tenantStatusResolver,
		ExtraAllowlist:       cfg.extraAllowlist,
	})
	if err != nil {
		return nil, err
	}

	// The middleware face's layer, outside the fixed chain: applied in
	// reverse so the first registered middleware ends up outermost (see
	// MiddlewareRegistrar's contract). The reading happens in the
	// assembling component's Serve stage (the http component calls Standard
	// between its own Init and the close), one full stage after every Init
	// turn has run: the face's write gate refuses any declaration landing
	// later than Init, so the accumulated set is complete where it is read.
	for _, mw := range slices.Backward(src.Middlewares()) {
		handler = mw(handler)
	}
	return handler, nil
}

// routeIsBelow reports whether path is the prefix itself or sits below it.
// The boundary check on the prefix's trailing separator is what keeps a
// sibling path sharing a textual start ("/api/v1/administrators" against
// "/api/v1/admin") out of the subtree.
func routeIsBelow(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// standardConfig is the option set one Standard call assembles from.
type standardConfig struct {
	az                   rbac.Authorizer
	rules                []rbac.RouteRule
	adminPrefix          string
	impersonation        func(http.Handler) http.Handler
	tenantStatusResolver tenancy.TenantStatusResolver
	extraAllowlist       []tenancy.MiddlewareOption
}

// Option customises Standard's derivation of the fixed chain.
type Option func(*standardConfig)

// WithAuthorization declares the route-authorization domain: az is the
// authorizer every gated route's permission check runs against, and rules is
// the host's route table -- one entry per route its modules mount, naming the
// permission each requires or declaring it public. The pair travels to
// rbac.GuardRoutes, so the table is checked for exhaustiveness in both
// directions and each gated route is wrapped in rbac's fail-closed gate
// before it is mounted. Both are required together: a host with no
// authorization domain omits the option entirely and its routes are mounted
// unguarded.
func WithAuthorization(az rbac.Authorizer, rules []rbac.RouteRule) Option {
	return func(c *standardConfig) {
		c.az = az
		c.rules = rules
	}
}

// WithAdminPrefix declares the mount prefix of the admin module's surface --
// admin.APIPath -- so the admin subtree is dispatched ahead of both the
// impersonation decorator and the tenancy chain (admin's permissions are
// evaluated against the caller's own real, unsubstituted Principal). A host
// with no admin module omits the option and no admin branch is built.
func WithAdminPrefix(prefix string) Option {
	return func(c *standardConfig) { c.adminPrefix = prefix }
}

// WithImpersonation is the impersonation decorator applied between
// authn.Middleware and the tenancy chain -- its one correct position. See
// Config.Impersonation for the decorator's contract.
func WithImpersonation(fn func(http.Handler) http.Handler) Option {
	return func(c *standardConfig) { c.impersonation = fn }
}

// WithTenantStatusResolver wires the tenancy chain's tenant-status gate. See
// Config.TenantStatusResolver for the seam's contract.
func WithTenantStatusResolver(r tenancy.TenantStatusResolver) Option {
	return func(c *standardConfig) { c.tenantStatusResolver = r }
}

// WithExtraAllowlist adds the host's own entries to the pre-auth allowlist
// the tenancy chain appends to app.PreAuthAllowlist(). See
// Config.ExtraAllowlist for what belongs here.
func WithExtraAllowlist(opts ...tenancy.MiddlewareOption) Option {
	return func(c *standardConfig) { c.extraAllowlist = append(c.extraAllowlist, opts...) }
}
