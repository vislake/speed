package pkgcore

import (
	"net/http"
	"strings"
)

// MountRoutes registers each route's Handler on mux for the route's own
// Path and for everything nested below it -- the mounting rule every
// module route follows, in the one place it is written down.
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// ("/api/v1/notes") from a subtree pattern (one ending in "/", matching
// everything below it): registering only the subtree pattern would make
// ServeMux redirect a bare request for the exact path with an HTTP redirect
// instead of serving it directly -- which would silently break a POST,
// since a redirect is not guaranteed to preserve the method or body across
// every client. MountedRoute's own doc comment says the Handler "serves
// every request below Path", meaning it must be reachable at Path itself
// AND at everything nested below it, so both patterns are registered here,
// pointing at the same Handler, instead of relying on ServeMux's implicit
// redirect-on-missing-slash behavior.
//
// A Path already ending in "/" is registered once: ServeMux reads it as the
// subtree pattern itself, and a second registration under Path+"/" would be
// a pattern no request can match.
//
// MountRoutes registers mux patterns unconditionally, so it panics exactly
// where a direct mux.Handle call would: a conflicting or malformed pattern
// is a wiring error, and ServeMux's own panic (at assembly time, before the
// server serves) is the loudest available report of it. Registering an
// empty Path is such an error -- a route no module should ever declare, and
// one that would otherwise mount as "the whole tree".
func MountRoutes(mux *http.ServeMux, routes ...MountedRoute) {
	for _, route := range routes {
		mux.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", route.Handler)
		}
	}
}
