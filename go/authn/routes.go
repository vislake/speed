package authn

import (
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

// ExemptSubtree partitions a module route set into authn's own HTTP subtree
// and every other mounted route, so a host can dispatch this module's
// surface on a branch of its own. Every route whose Path is apiPath (the
// mount point this module's Register uses, module.go) or sits below it
// belongs to the subtree; everything else is returned in rest. Both slices
// preserve the input's order and the input itself is not modified.
//
// The partition exists because authn's HTTP surface must never sit
// downstream of tenancy.Middleware: every authn operation resolves its
// tenant from the verified Principal's own claim, per operation, never from
// a tenant a middleware guessed. Composing it the ordinary way is also the
// only shape in which enterprise SSO's login start works at all -- its
// provider value is the dynamic "oidc:<tenant>" string an exact
// (method, path) allowlist (tenancy.WithAllowlist) cannot enumerate, and the
// path carries no Principal (it is the first step of a sign-in), so a
// tenancy.Middleware in front of it would refuse every such request with
// tenancy.tenant_unresolved before authn's own OIDC logic saw it. A host
// therefore mounts the returned subtree behind authn.Middleware (and its
// optional verification) alone, outside the tenancy chain; authn's own
// Handler decides, operation by operation, whether a Principal is required
// (handler.go's requirePrincipal).
//
// The subtree it returns may hold more than one route in principle; this
// module currently mounts its whole surface as the single route at apiPath.
// A host with no authn module in its composition gets an empty subtree and
// the input unchanged as rest, and decides for itself whether that absence
// is an error -- not every composition boots authn.
func ExemptSubtree(routes []pkgcore.MountedRoute) (subtree, rest []pkgcore.MountedRoute) {
	subtree = make([]pkgcore.MountedRoute, 0, 1)
	rest = make([]pkgcore.MountedRoute, 0, len(routes))
	for _, route := range routes {
		if route.Path == apiPath || strings.HasPrefix(route.Path, apiPath+"/") {
			subtree = append(subtree, route)
			continue
		}
		rest = append(rest, route)
	}
	return subtree, rest
}
