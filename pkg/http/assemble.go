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
	ordered, err := orderLayers(e.settings.name, e.layers)
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
			// Undecided by the design: a Wrap that hands back nothing is
			// treated here as the call-site error a nil Wrap is, rather
			// than as a startup failure of its own. Letting it through
			// would serve a chain with a hole in it, and the request
			// would die on a nil handler with nothing naming the layer.
			panic(fmt.Sprintf("http: endpoint %q: middleware %q returned a nil handler from Wrap, "+
				"so the chain would have a hole where that layer is. Wrap returns the handler "+
				"that calls the one it was given", e.settings.name, ordered[i].mw.Name))
		}
		handler = wrapped
	}

	logger.Info("an HTTP endpoint's chain is assembled",
		"endpoint", e.settings.name,
		"routes", len(e.routes),
		"chain", chainDescription(ordered))
	return e.base(handler, injected), nil
}

// mount binds the registered routes on the engine.
//
// Whether two patterns conflict is the engine's judgement, not this module's,
// and the engines this seam is made for report it by panicking as they are
// mounted. Caught here, it becomes a startup failure; left alone it would take
// the process down with a stack trace, during a stage whose failures are
// supposed to abort the startup in an orderly way.
func (e *endpoint) mount(engine Engine) error {
	for _, r := range e.routes {
		if err := mountRoute(engine, e.settings.name, r); err != nil {
			return err
		}
	}
	return nil
}

// mountRoute binds one route, turning a panic from the engine into an error.
// The message carries the pattern being mounted and what the engine said, which
// is where the pattern it clashes with is named.
func mountRoute(engine Engine, name string, r route) (err error) {
	defer func() {
		if raised := recover(); raised != nil {
			err = fmt.Errorf("%w: endpoint %q: the routing engine refused the pattern %q: %v. "+
				"Two registrations claim the same pattern, or two patterns match one request "+
				"with neither being more specific; change one of them or move it to another "+
				"endpoint", ErrRouteConflict, name, r.pattern, raised)
		}
	}()
	engine.Handle(r.pattern, r.handler)
	return nil
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
