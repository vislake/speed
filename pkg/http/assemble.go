package http

import (
	"fmt"
	"log/slog"
	nethttp "net/http"
	"strings"

	"github.com/vislake/speed/pkg/log"
)

// assemble builds this endpoint's final handler, once, from the inside out:
// the routes are mounted on the engine, the middleware is wrapped around it in
// the order the constraints and the preferences give, and the base sits outside
// all of it.
//
// injected is the logger to put in each request's context, or nil when no
// module delivers the Logger capability. logger is where this assembly's own
// diagnostics go.
func (e *endpoint) assemble(engine Engine, injected, logger *slog.Logger) (nethttp.Handler, error) {
	if err := e.mount(engine); err != nil {
		return nil, err
	}
	ordered, unlanded, err := orderLayers(e.settings.name, e.layers)
	if err != nil {
		return nil, err
	}

	var handler nethttp.Handler = engine
	// ordered runs from outermost to innermost, so it is consumed backwards:
	// the innermost layer is the first to wrap the engine and the last to see
	// a request.
	for i := len(ordered) - 1; i >= 0; i-- {
		wrapped := ordered[i].mw.Wrap(handler)
		if wrapped == nil {
			// A chain with a hole in it would kill the request on a nil
			// handler with nothing naming the layer that left it. It is
			// reported rather than panicked because this runs in Serve,
			// where a panic skips the registry's rollback and strands
			// the sockets of the endpoints already bound.
			return nil, fmt.Errorf("%w: endpoint %q: middleware %q returned a nil handler from "+
				"Wrap, so the chain would have a hole where that layer is. Wrap returns the "+
				"handler that calls the one it was given",
				ErrChainAssembly, e.settings.name, ordered[i].mw.Name)
		}
		handler = wrapped
	}

	reportUnlanded(logger, unlanded)
	logger.Info("an HTTP endpoint's chain is assembled",
		"endpoint", e.settings.name,
		"routes", len(e.routes),
		"chain", chainDescription(ordered))
	return e.base(handler, injected), nil
}

// reportUnlanded writes out the constraints that placed nothing.
//
// This is the whole of their observability. The assembly carries on — a
// capability absent from the process is a legal configuration — so the layer
// that declared itself inside authentication runs anyway, in the position its
// Order happens to give it. The cause is written with each line because the
// two causes are answered by different edits: a capability nobody stands for
// sends the reader looking for the module that should have been in the
// assembly, while a capability only the declaring layer stands for is that
// layer correcting what it said about itself.
//
// It goes to this module's own logger because core does not export a startup
// diagnostics surface. The level is Warn: written at Info it would sit beside
// the chain listing of every endpoint, which is the routine output a reader
// skims past.
func reportUnlanded(logger *slog.Logger, unlanded []unlandedConstraint) {
	for _, u := range unlanded {
		logger.Warn("an HTTP middleware constraint landed on no layer",
			"endpoint", u.endpoint,
			"middleware", u.layer,
			"declaration", fmt.Sprintf("%s[%d]", u.field, u.at),
			"capability", u.capability,
			"cause", string(u.cause))
	}
}

// mount binds the registered routes on the engine.
//
// Whether two patterns conflict is the engine's judgement, not this module's,
// and the engines this seam is made for report it by panicking as they are
// mounted. Caught here, it becomes a startup failure; left alone it would take
// the process down with a stack trace, during a stage whose failures are
// supposed to abort the startup in an orderly way.
//
// What is left to catch here is the clash: every pattern was offered to an
// engine of its own when it was registered, and one the engine refuses by
// itself never reaches this point. Which pattern the refused one clashes with
// is the engine's answer too, asked through the attribution the seam requires
// of an implementation subpackage; see refusalAttributor.
func (e *endpoint) mount(engine Engine) error {
	mounted := make([]route, 0, len(e.routes))
	for _, r := range e.routes {
		if raised := offerRoute(engine, r); raised != nil {
			return e.conflictError(engine, mounted, r, raised)
		}
		mounted = append(mounted, r)
	}
	return nil
}

// offerRoute binds one route and hands back whatever the engine raised, or nil
// when it took the pattern.
func offerRoute(engine Engine, r route) (raised any) {
	defer func() { raised = recover() }()
	engine.Handle(r.pattern, r.handler)
	return nil
}

// refusalAttributor is what the engine seam asks of an implementation
// subpackage: given the pattern an engine refused and the patterns already
// mounted on it, name the one the refused pattern cannot stand beside.
//
// The mounting point here sees only that a mount was refused. How the engine
// reaches that verdict is its own business — one that judges a set as a whole
// may not be able to answer the way one that judges pairs does — so the
// answer has to come from the layer that binds the engine. An engine that does
// not answer leaves the failure below saying so rather than naming one pattern
// as if that were the whole story.
//
// The method is exported although this interface is not: the implementation
// lives in another package, and an exported method name is the only way a type
// there can satisfy it.
type refusalAttributor interface {
	// AttributeRefusal returns the already-mounted pattern that the refused
	// one conflicts with, or false when the engine cannot single one out.
	// It is asked once, on the failing path, as the startup ends.
	AttributeRefusal(refused string, mounted []string) (conflicting string, ok bool)
}

// conflictError names both patterns and the endpoint they are on.
//
// It names neither registrant: the registration surface does not know who
// called it, which is the same reason a middleware has to declare its own
// Provides. A pattern is a literal in the source, so the two of them locate
// both calls.
//
// The second pattern comes from the engine, through the attribution the seam
// asks of it. It is not read out of what the engine raised: that value is the
// engine's own wording, and this module does not parse it.
func (e *endpoint) conflictError(engine Engine, mounted []route, refused route, raised any) error {
	if attributor, answers := engine.(refusalAttributor); answers {
		if with, found := attributor.AttributeRefusal(refused.pattern, mountedPatterns(mounted)); found {
			return fmt.Errorf("%w: endpoint %q: the patterns %q and %q cannot both be mounted: %v. "+
				"Either two registrations claim the same pattern, or the two match one request "+
				"with neither being more specific; change one of them or move it to another "+
				"endpoint", ErrRouteConflict, e.settings.name, with, refused.pattern, raised)
		}
	}
	// The engine refused this pattern without naming a partner. That is a
	// gap in the engine rather than a normal conflict: the seam requires the
	// attribution, so saying the refusal was not attributed beats naming the
	// one pattern this module is sure about and letting it read like a
	// verdict about that pattern alone.
	return fmt.Errorf("%w: endpoint %q: the routing engine refused the pattern %q beside the %d "+
		"already mounted on this endpoint and did not attribute the refusal to a pair of patterns: "+
		"%v. Naming that pair is what the engine seam asks of an implementation subpackage, so "+
		"this is that engine not answering for itself; change the pattern or move it to another "+
		"endpoint", ErrRouteConflict, e.settings.name, refused.pattern, len(mounted), raised)
}

// mountedPatterns renders the patterns already bound on one endpoint, in
// registration order, which is what the attribution is asked over.
func mountedPatterns(mounted []route) []string {
	out := make([]string, 0, len(mounted))
	for _, r := range mounted {
		out = append(out, r.pattern)
	}
	return out
}

// base is the outermost layer of the chain, the one this module puts there
// itself.
//
// The body limit is imposed here rather than in Decode, so that it holds for
// every handler on the endpoint including the ones that never call Decode, and
// so that a middleware reading the body is already reading a truncated one.
//
// The logger is injected here for the same reason of position: outside every
// layer is the only place from which each of them, and the handler, can take it
// out of the request context. First injection belongs to whoever creates the
// context rather than to a middleware — several middlewares would each believe
// they are the fallback, and the loggers they inject carry different module
// names.
//
// With no Logger capability in the assembly nothing is injected and nothing
// fails: log.FromContext then falls back to the process default logger, so the
// request path's records go to standard output instead of to the configured
// destinations, without a failure signal. That is the stated cost of the
// dependency being optional.
func (e *endpoint) base(next nethttp.Handler, injected *slog.Logger) nethttp.Handler {
	limit := e.settings.maxBodyBytes
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if limit > 0 && r.Body != nil {
			r.Body = nethttp.MaxBytesReader(w, r.Body, limit)
		}
		if injected != nil {
			r = r.WithContext(log.WithLogger(r.Context(), injected))
		}
		next.ServeHTTP(w, r)
	})
}

// chainDescription renders an assembled chain for the startup log, outermost
// first. The base layer this module puts outside every registered one is named
// too: a reader comparing the line with the code otherwise finds the body limit
// and the logger applied by something the line does not mention.
func chainDescription(ordered []layer) string {
	names := make([]string, 0, len(ordered)+1)
	names = append(names, "http:base")
	for _, l := range ordered {
		names = append(names, l.mw.Name)
	}
	return strings.Join(names, " -> ")
}
