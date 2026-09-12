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

// MountRoute mounts one route through the http component's route face
// (RouteRegistrar) when the composition carries one, and does nothing when
// it does not. It is the declaration body's one-line form of the optional
// route dependency: a module declares
// Requires{Token: (*RouteRegistrar)(nil), Optional: true} and calls this
// from its Init turn, so a composition that serves HTTP mounts the route on
// the component's accumulated face, while a pure-background composition --
// no http component -- constructs the module and serves nothing, which is a
// legitimate configuration rather than an error. An error is returned only
// when the by-type context holds several registrars (an ambiguous
// assembly); a mount the face refuses for its stage panics, exactly as the
// face's own contract states.
func MountRoute(reg *ComponentRegistry, path string, handler http.Handler) error {
	registrar, ok, err := GetOptional[RouteRegistrar](reg)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	registrar.Mount(path, handler)
	return nil
}
