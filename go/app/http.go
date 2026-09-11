package app

import (
	"net/http"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
)

// HTTPSpec declares the host's half of the HTTP face and the address Run
// listens on. Everything in it is host policy; the order the pieces compose
// in is the transition HTTP component's (legacy_http.go).
type HTTPSpec struct {
	// Addr is the listen address ("host:port" or ":port") Run binds. Run
	// refuses an empty address rather than binding a random port.
	Addr string

	// Middleware is the host's own middleware chain, outermost first: the
	// entries wrap the composed handler -- the mux, or Compose's product
	// when one is declared -- and the first entry listed here is the one a
	// request reaches first. A chain that needs the mux itself states that
	// with a closure (the entry receives the already-composed handler and
	// returns the chain built around it) or, when it must mount the
	// registry's routes too, through Compose below.
	Middleware []func(http.Handler) http.Handler

	// ExtraRoutes are the host's hand-written routes, mounted beside the
	// module routes exactly as a module's own routes are (pkgcore.MountRoutes
	// semantics: the handler serves Path and everything below it). A route
	// conflicting with one already mounted panics at assembly time, the
	// loudest report a wiring error can get.
	ExtraRoutes []pkgcore.MountedRoute

	// Compose, when non-nil, is the host's own composition of the
	// protected face, built over the mux the engine prepared. It receives
	// that mux -- already carrying the platform liveness routes and the
	// host's ExtraRoutes -- and returns the handler that serves them,
	// typically the product of chain.Standard, which mounts the registry's
	// guarded routes onto the mux and wraps the whole composition in the
	// fixed middleware chain. The returned handler replaces the bare mux as
	// the thing the Middleware entries wrap.
	//
	// It is the seat for a host whose HTTP face is more than a route list:
	// the platform chain must wrap the mux from the inside (authn outermost
	// around the branches, tenancy around the protected half), which a
	// plain middleware entry -- one that wraps whatever it is given -- cannot
	// express. When Compose is set the engine does NOT mount the registry's
	// routes itself; the host's composition mounts them from the registry it
	// holds, so no route is registered twice.
	Compose func(mux *http.ServeMux) (http.Handler, error)

	// SPA, when non-nil, serves a built single-page application at the
	// outermost layer of the host's own composition: requests the frontend
	// answers never reach the middleware chain or the mux, while everything
	// the frontend does not answer falls through unchanged (pkgcore/spa's
	// interception rules). It wraps outside Middleware, inside the
	// observability middleware every request is counted at.
	SPA *SPASpec
}

// SPASpec declares a single-page-application directory to serve: the
// directory holding the built frontend, and the options go/pkgcore/spa is
// built with.
type SPASpec struct {
	// Dir is the directory holding the built frontend's index.html and
	// assets.
	Dir string

	// Options customise the spa.Handler. The host declares the composed
	// server's own surface through them (spa.WithServerPrefix("/api"),
	// spa.WithServerPath(obs.HealthzPath) and the rest of the set): the
	// frontend answers every GET/HEAD path a declared option does not claim,
	// so a route left undeclared is answered from disk instead of reaching
	// the handler the engine mounted for it.
	Options []spa.Option
}

// WithHTTP declares the HTTP face the transition HTTP component composes
// during the assembly's Start stage and the address Run serves it on.
func WithHTTP(spec HTTPSpec) Option {
	return func(c *engineConfig) {
		s := spec
		c.httpSpec = &s
	}
}
