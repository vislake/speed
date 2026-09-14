package http

import (
	"fmt"
	"log/slog"
	nethttp "net/http"
	"slices"
	"strings"
	"sync"
)

// phase is the state of the registration gate. It is one gate for the whole
// module rather than one per endpoint: the stages it follows are the module's.
type phase int

const (
	// phaseDeclared is the state the endpoints are constructed in. The
	// surface exists and can be read, and a write is a call that arrived
	// before the stage that accepts writes.
	phaseDeclared phase = iota
	// phaseOpen is the Init stage, the one stage registrations are made in.
	phaseOpen
	// phaseSealed is everything after Init. The chains are assembled from
	// what was registered, and a later registration would reach no chain.
	phaseSealed
)

// router is this module's product and the implementation of the Router
// capability. It holds the endpoints configuration declared and the gate their
// registration surfaces share.
//
// The endpoint set is fixed at construction, so a lookup needs no lock; the
// mutex covers the gate and the registrations, which arrive during Init from
// whatever goroutine a registrant chose to use.
type router struct {
	mu        sync.Mutex
	phase     phase
	endpoints map[string]*endpoint
	// names is the endpoint names in configuration order, kept for the
	// messages that list what does exist and for the order the endpoints are
	// assembled, bound and stopped in.
	names []string

	// newEngine builds the routing engine of one endpoint. One engine per
	// endpoint: a shared one would carry every endpoint's routes, and the
	// separation of a management endpoint from a public one is the whole
	// point of having several.
	newEngine func() Engine
	// injected is the logger put into each request's context, nil when no
	// module delivers the Logger capability.
	injected *slog.Logger
	// logger is where this module's own diagnostics go. Unlike injected it
	// is never nil: with no Logger capability it is the process default.
	logger *slog.Logger
}

var _ Router = (*router)(nil)

// newRouter builds the endpoints from configuration. Nothing is bound here:
// the address is bound in Serve, and until then an endpoint is a registration
// surface with no listener behind it.
func newRouter(settings []endpointSettings, newEngine func() Engine) *router {
	r := &router{
		endpoints: make(map[string]*endpoint, len(settings)),
		newEngine: newEngine,
	}
	for _, s := range settings {
		r.endpoints[s.name] = &endpoint{settings: s, gate: r}
		r.names = append(r.names, s.name)
	}
	return r
}

// Endpoint returns a declared endpoint. It is not behind the gate: a caller may
// look an endpoint up in any stage, and Accepting is readable throughout the
// run.
//
// An undeclared name is an error rather than a panic, because the set of
// endpoints comes from configuration, which is data: a registrant naming an
// endpoint the host did not configure has made no programming error, and the
// failure belongs to the startup its caller aborts. The text lists the names
// that do exist, because picking one of them is the fixing action; there is no
// fallback to a default endpoint.
func (r *router) Endpoint(name string) (Endpoint, error) {
	e, ok := r.endpoints[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q. The endpoints this assembly declares are %s. "+
			"Either register on one of them or add %q to %s.%s in the configuration",
			ErrUnknownEndpoint, name, r.declaredNames(), name, configNamespace, endpointsItem)
	}
	return e, nil
}

// declaredNames renders the endpoint names for a message, saying so plainly
// when there are none: "the endpoints are " followed by nothing reads as a
// truncated message rather than as the state it describes.
func (r *router) declaredNames() string {
	if len(r.names) == 0 {
		return "none: the configuration declares no endpoint at all"
	}
	quoted := make([]string, 0, len(r.names))
	for _, name := range slices.Sorted(slices.Values(r.names)) {
		quoted = append(quoted, fmt.Sprintf("%q", name))
	}
	return strings.Join(quoted, ", ")
}

// open lets registrations through. It is called from this module's Migrate,
// which is the last thing that runs before the Init stage begins.
func (r *router) open() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = phaseOpen
}

// seal stops accepting registrations. It is called from this module's Start,
// which is the first thing that runs after the Init stage has ended.
//
// The pair of borrowed stages is what makes the open window exactly the Init
// stage. Opening in this module's own Init would be too late: Lookup does not
// filter by Requires, so a module that never declared a dependency on Router
// may be constructed and initialised before this one and still legally register
// in its own Init. Sealing in this module's own Serve would be too early in the
// other direction: the whole Start stage, and the Serve callbacks of every
// module ordered ahead of this one, would still be accepted, and those
// registrations reach no chain.
func (r *router) seal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = phaseSealed
}

// write performs one registration behind the gate.
//
// A registration outside the Init stage panics rather than returning an error,
// for the reason the surface returns nothing at all: it is a wiring mistake at
// the call site, and a silently dropped registration shows up as a route nobody
// can reach, long after the call that caused it.
func (r *router) write(endpoint, call string, record func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.phase {
	case phaseOpen:
		record()
	case phaseDeclared:
		panic(fmt.Sprintf("http: endpoint %q: %s was called before the Init stage, and the "+
			"registration surface accepts writes during Init alone. Move the call into the "+
			"module's Init callback: every instance exists by then, which is what makes it "+
			"the stage modules wire each other up in", endpoint, call))
	case phaseSealed:
		panic(fmt.Sprintf("http: endpoint %q: %s was called after the Init stage, and the "+
			"registration surface accepts writes during Init alone. The chain of each "+
			"endpoint is assembled once in Serve, so this registration would reach no "+
			"chain at all. Move the call into the module's Init callback", endpoint, call))
	}
}

// route is one pattern bound to a handler, as the registration recorded it.
// Whether two patterns conflict is the routing engine's judgement and is made
// when they are mounted in Serve, not here.
type route struct {
	pattern string
	handler nethttp.Handler
}

// endpoint is one listening endpoint: the registration surface modules write
// to, and the listener the chain is served from once Serve has bound it.
type endpoint struct {
	settings endpointSettings
	// gate is the module-wide registration gate. The endpoint's own
	// registrations are recorded under its mutex.
	gate *router

	routes []route
	layers []layer

	// socket carries everything that exists only between Serve and Close:
	// the server, the listening socket and the drain.
	socket listener
}

var _ Endpoint = (*endpoint)(nil)

// Route binds h to a pattern on this endpoint.
//
// The pattern's grammar is the routing engine's, and the engine is asked here
// whether it takes this pattern at all. A pattern the engine refuses on its own
// is a mistake in this one call, knowable from this one registration, which is
// the class of failure the surface panics for, alongside a nil handler and an
// illegal capability token.
//
// A conflict between two patterns is not of that class and is not judged here:
// a registration records a declaration, and whether the set of them conflicts
// is a property of the assembled set, reported as a startup failure when the
// chain is built.
func (e *endpoint) Route(pattern string, h nethttp.Handler) {
	if h == nil {
		panic(fmt.Sprintf("http: endpoint %q: Route(%q, nil) has no handler to bind. "+
			"Pass the handler this route is served by", e.settings.name, pattern))
	}
	if refused := patternRefusal(e.gate.newEngine, pattern, h); refused != nil {
		panic(fmt.Sprintf("http: endpoint %q: Route(%q, ...) was given a pattern the routing "+
			"engine refuses on its own: %v. The grammar is the engine's, not this "+
			"package's; correct the pattern at this call", e.settings.name, pattern, refused))
	}
	e.gate.write(e.settings.name, "Route", func() {
		e.routes = append(e.routes, route{pattern: pattern, handler: h})
	})
}

// patternRefusal offers the pattern to an engine of its own and reports what
// the engine raised, or nil when it took it. The engines this seam is made for
// refuse a pattern by panicking, whether the pattern is malformed or clashes
// with another one, and the two are told apart by what else is on the engine:
// nothing is on this one, so anything raised here is about this pattern alone.
//
// The engine is thrown away with the call. It costs one engine per route
// registered, once, during Init.
func patternRefusal(newEngine func() Engine, pattern string, h nethttp.Handler) (raised any) {
	defer func() { raised = recover() }()
	newEngine().Handle(pattern, h)
	return nil
}

// Use registers a middleware layer on this endpoint. The layer applies to every
// route on the endpoint; a layer that should apply to some of them is wrapped
// around those handlers by their own registrant, or those routes are moved to
// another endpoint.
//
// The declaration is checked here rather than when the chain is ordered, so an
// illegal capability token names the call that wrote it while that call is
// still on the stack.
func (e *endpoint) Use(mw Middleware) {
	if mw.Wrap == nil {
		panic(fmt.Sprintf("http: endpoint %q: Use registered middleware %q with a nil Wrap, "+
			"so the layer would wrap nothing. Give Wrap the func(http.Handler) http.Handler "+
			"this layer is made of", e.settings.name, mw.Name))
	}
	for j, token := range mw.After {
		capabilityKey(token, declarationSite(e.settings.name, mw.Name, "After", j))
	}
	for j, token := range mw.Before {
		capabilityKey(token, declarationSite(e.settings.name, mw.Name, "Before", j))
	}
	e.gate.write(e.settings.name, "Use", func() {
		e.layers = append(e.layers, registeredLayer(mw))
	})
}

// registeredLayer turns a registration into the graph node the ordering reads.
//
// The capabilities the registrant delivers are what an After or a Before
// constraint of another layer resolves against, and they are empty here: the
// registration surface carries no module identity, and Middleware declares no
// capability of its own, so nothing on this path can say who registered a
// layer. Every constraint therefore finds no provider and drops, which is the
// same silence the design accepts for a capability absent from the assembly —
// except that here it holds even when the provider is present. Filling the
// field is the whole of the change once the design settles where that identity
// comes from.
func registeredLayer(mw Middleware) layer {
	return layer{mw: mw}
}

// Accepting reports whether this endpoint is still taking requests. It is not
// behind the gate and is safe to call from any goroutine at any moment: before
// the address is bound and after Stop it reports false.
func (e *endpoint) Accepting() bool { return e.socket.isAccepting() }
