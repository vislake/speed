package http

import (
	nethttp "net/http"

	"github.com/vislake/speed/pkg/core"
)

// Router is the capability this module delivers. It is taken up through the
// registry, written core.Resolve[http.Router](reg), and it hands out the
// endpoints configuration declared.
//
// The module delivers it exclusively. Two entry point implementations running
// at once would leave a registrant unable to tell which one it registered
// with, and "take them all out" means nothing for a registration, so the
// conflict is named during resolution rather than showing up at run time as a
// route nobody can reach.
type Router interface {
	// Endpoint returns a declared endpoint. It may be called in any stage.
	// An undeclared name is an error wrapping ErrUnknownEndpoint, because
	// the set of endpoints comes from configuration, which is data.
	Endpoint(name string) (Endpoint, error)
}

// Endpoint is the registration surface of one listening endpoint.
//
// Route and Use accept writes during the Init stage only; a write outside it
// panics. They return nothing: the only way a registration can fail is a
// programming error at the call site, and handing back an error a registrant
// is free to drop would leave a route silently unbound. A conflict between two
// route patterns is not of that kind — it can only be judged once the chain is
// assembled in Serve, and it is reported there as a startup failure.
//
// Reads are not behind that gate: Accepting may be called in any stage.
type Endpoint interface {
	// Route binds h to a pattern on this endpoint. The pattern's grammar is
	// the routing engine's, not this package's.
	Route(pattern string, h nethttp.Handler)
	// Use registers a middleware layer on this endpoint. The layer applies
	// to every route on the endpoint; there is no route-scoped middleware.
	Use(mw Middleware)
	// Accepting reports whether this endpoint is still taking requests. It
	// returns false before the address is bound and after Stop, and it is
	// safe to call from any goroutine at any moment.
	Accepting() bool
}

// Middleware is one layer together with what it declares about its own
// position.
//
// After and Before are hard constraints and decide the topological order;
// Order is a preference that only picks among the positions the constraints
// already allow. The two cannot contradict each other, because Order never
// selects outside the ready set.
//
// Provides is what makes those constraints land: After and Before name
// capabilities, and which layer on the chain stands for a capability can only
// be said by that layer itself. The registration surface cannot supply it —
// Endpoint carries no registrant identity, core offers no "the module
// currently in Init" query, and a registration may be made from a goroutine,
// so the call stack says nothing either.
type Middleware struct {
	// Name is for diagnostics only: the panic text and the chain listed at
	// startup. It is not an identity this module matches anything against.
	Name string
	// Provides names the capabilities this layer stands for on the chain, so
	// that another layer's After or Before can point at it. Leaving it empty
	// is legal, at the cost that nobody can point at this layer.
	//
	// It is not checked against the Provides of the registrant's own module
	// descriptor, for the same reason it has to be declared here at all:
	// this module does not know who registered the layer. A layer may
	// therefore claim a capability its module does not deliver.
	Provides []core.Token
	// After places this layer inside the middleware of these capabilities.
	// A capability no other layer stands for drops the constraint, and the
	// dropped constraint is listed in the startup diagnostics with the cause
	// it dropped for: no layer on the endpoint represents the capability, or
	// only this one does, and that edge to itself is dropped too.
	After []core.Token
	// Before places this layer outside the middleware of these
	// capabilities, with the same treatment of an absent provider.
	Before []core.Token
	// Order is the preference where the constraints leave a choice; a
	// smaller value sits further out. Layers with equal Order and no
	// constraint between them keep their registration order.
	Order int
	// Wrap is the layer itself. A nil Wrap panics at registration; a Wrap
	// that hands back a nil handler is a chain assembly failure in Serve,
	// wrapping ErrChainAssembly, because a panic there would skip the
	// registry's rollback.
	Wrap func(nethttp.Handler) nethttp.Handler
}

// Spec is the resource type a module declares its own OpenAPI fragment as.
// This module stores it and checks its shape; it takes no part in routing.
//
// A reader takes every fragment with core.Resources[http.Spec](reg), each one
// carrying the name of the module that declared it, so a merge conflict can
// name its sources. Merging the fragments and publishing the result belong to
// the reader, not here.
type Spec struct {
	// Endpoint names the listening endpoint whose API this fragment
	// describes. A name that was never declared is not an error: a resource
	// is a static declaration, and this module does not interpret what it
	// describes.
	Endpoint string
	// Document is the OpenAPI fragment, as JSON. A module usually reads its
	// own openapi.json in with //go:embed.
	Document []byte
}

// Engine is the seam an implementation subpackage fills: a routing engine that
// takes pattern registrations and answers as a handler. The standard library's
// *nethttp.ServeMux satisfies it as it stands.
//
// The engine's own type never appears on this package's surface, which is what
// keeps a routing library out of the dependency list of every module that
// registers a route.
//
// The seam asks one thing of an implementation subpackage beyond this
// interface: when the engine refuses to mount a pattern beside the others, the
// subpackage has to attribute that refusal to a pair — the refused pattern and
// the mounted one it cannot stand beside — because only the layer holding the
// engine can ask it. The subpackage's engine answers
// AttributeRefusal(refused string, mounted []string) (string, bool). An engine
// that does not answer leaves the startup failure saying the refusal was not
// attributed, rather than naming one pattern as though that were the verdict:
// a diagnostic that holds for one engine alone goes quietly wrong on the next
// one.
type Engine interface {
	nethttp.Handler
	// Handle binds h to a pattern. The engine refuses a pattern however it
	// sees fit, including by panicking, and the caller catches that at both
	// moments it asks: a pattern offered alone at registration, where a
	// refusal is a malformed pattern and panics at the call, and a pattern
	// mounted beside the others in Serve, where a refusal is a conflict and
	// becomes a startup failure wrapping ErrRouteConflict.
	Handle(pattern string, h nethttp.Handler)
}
