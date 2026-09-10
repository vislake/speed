// This file mounts the module set on the router: every route the registry holds is
// admitted through the app's route-authorization table on the way out.
package app

import (
	"fmt"
	"net/http"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/hostcore"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// mountModuleRoutes copies every route reg's modules mounted onto mux,
// with TWO deliberate exceptions, each mounted by BuildServer on its own
// topMux branch directly behind authn.Middleware and nothing else -- see
// BuildServer's own composition comment -- and each returned here instead
// of mounted into mux:
//
//   - admin's own mounted route (AdminRoutePath): admin's HTTP surface
//     must not sit behind ordinary tenancy.Middleware tenant resolution,
//     and must not sit behind admin.ImpersonationMiddleware's identity
//     substitution either -- go/admin/AGENTS.md's wiring-contract section
//     states both explicitly ("admin's OWN routes ... do not sit behind
//     ImpersonationMiddleware -- that decorator's effect is on the REST of
//     the application's routes only"; "does NOT go through ordinary
//     tenancy.Middleware tenant resolution").
//   - authn's own subtree, split out by authn.ExemptSubtree: the module
//     owns the knowledge of which mounted routes are its own (everything at
//     or below its mount point), and its doc comment (go/authn/routes.go)
//     states why that subtree must sit outside the tenancy chain -- authn's
//     routes resolve the tenant from the Principal's own claim per
//     operation, and the enterprise-OIDC login start's dynamic
//     "oidc:<tenant>" path is not even expressible as an exact (method,
//     path) allowlist entry. authn.Middleware still runs outside everything
//     (it wraps topMux itself), so a genuinely invalid bearer still 401s
//     before this branch is reached; authn's Handler itself decides,
//     operation by operation, whether a Principal is required
//     (go/authn/handler.go's requirePrincipal).
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// ("/api/v1/notes") from a subtree pattern ("/api/v1/notes/", matching
// everything below it): registering only the subtree pattern would make
// ServeMux redirect a bare request for the exact path with an HTTP
// redirect instead of serving it directly -- which would silently break a
// POST, since a redirect is not guaranteed to preserve the method or body
// across every client. pkgcore.MountedRoute's own doc comment says the
// Handler "serves every request below Path", meaning it must be reachable
// at Path itself AND at everything nested below it; that contract's one
// implementation is pkgcore.MountRoutes (see its doc comment), which is
// what every plain route below mounts through.
//
// Every route is admitted through the route table on the way out: the
// table is DemoRouteRules (demo/demo_subject.go), applied by rbac.GuardRoutes,
// which wraps each gated route in rbac's permission gate and refuses the
// whole set -- failing the build here rather than serving the route -- when
// a mounted path has no declared decision. The two excepted paths are
// decided by the same table (admin gated, authn public) and keep being
// covered by its exhaustiveness check; only the DESTINATION of the
// resulting handler differs. hostcore.AuthnAPIPath stays in use as the
// route table's own name for authn's mount point (demo/demo_subject.go's rules).
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry, az rbac.Authorizer, orgDeps demo.OrgRouteGuardDeps, demoHeaderDisabled bool) (adminHandler http.Handler, authnRoutes []pkgcore.MountedRoute, err error) {
	guarded, err := rbac.GuardRoutes(az, reg.Routes.Routes(), demo.DemoRouteRules(az, orgDeps, demoHeaderDisabled))
	if err != nil {
		return nil, nil, err
	}
	authnRoutes, rest := authn.ExemptSubtree(guarded)
	for _, route := range rest {
		if route.Path == demo.AdminRoutePath {
			adminHandler = route.Handler
			continue
		}
		pkgcore.MountRoutes(mux, route)
	}
	if adminHandler == nil {
		return nil, nil, fmt.Errorf("reference-app: no module mounted %q; admin.Module.Register must run for this app to compose its dedicated middleware branch", demo.AdminRoutePath)
	}
	if len(authnRoutes) == 0 {
		return nil, nil, fmt.Errorf("reference-app: no module mounted %q; authn.Module.Register must run for this app to compose its dedicated middleware branch", hostcore.AuthnAPIPath)
	}
	return adminHandler, authnRoutes, nil
}
