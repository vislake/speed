// Package httpserve carries the http component: the component that owns the
// process's HTTP face. It holds the route and middleware declaration faces
// (the product implementing pkgcore.RouteRegistrar and
// pkgcore.MiddlewareRegistrar), assembles the final http.Handler from the
// host's link policy in the Serve stage, opens the listener and stops
// accepting new requests first on the way down.
//
// # Why the component lives here
//
// The routes and middleware a module declares are consumed by exactly one
// component, so the consuming side defines the faces -- pkgcore keeps the
// contract tokens (RouteRegistrar, MiddlewareRegistrar, MountedRoute,
// RouteAccess, MountRoutes) and this package supplies the one implementation
// that pairs them with a listener. The component sits ABOVE go/app/chain in
// the dependency graph: it imports chain to assemble the fixed chain
// (chain.Standard, whose order contract is unchanged) and observability to
// mount the platform liveness routes and seed the route-label table. The
// placement is forced in both directions: go/app/chain imports go/app (it
// restates app.PreAuthAllowlist), so this component cannot live in go/app
// root without closing a cycle, and pkgcore cannot depend on chain without
// inverting the module graph -- which is why the link policy type lives
// here, on the consuming side, rather than in chain.
//
// # The lifecycle this component drives
//
//	New   -- reads the host link policy from the by-type context (a required
//	         dependency) and decodes its own configuration block.
//	Serve -- reads the accumulated routes and middleware, assembles the
//	         handler per the host's policy, applies the platform middleware
//	         outermost of the chain (or of the protected mux itself for a
//	         chainless policy), wraps the host's outer wrapper around the
//	         finished handler, and opens the listener. It runs after every
//	         component's Start callback, because entry traffic can reach any
//	         component while dependency order can only say "after my
//	         dependencies".
//	Stop  -- the first beat: stops accepting new requests (the listener
//	         closes; in-flight requests keep draining).
//	Close -- the blocking half: waits the drain out, bounded by the
//	         shutdown timeout.
//
// The declaration faces accept writes only while the Init stage runs; the
// face reads the assembly's own stage reading ((*pkgcore.ComponentRegistry).
// Stage) and refuses a write outside Init, so a mount that would otherwise
// land after the handler was assembled is a loud wiring error instead of a
// silently ignored route.
package httpserve
