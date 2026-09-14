// Package stdmux is the entry point module with the standard library's
// ServeMux as its routing engine.
//
// It is the zero-dependency implementation of the seam the parent package
// defines: ServeMux matches on method, path and wildcards and has explicit
// precedence rules, so a host that wants routing without a routing library
// imports this package and gets an entry point assembled from its
// configuration. A host that needs what ServeMux does not do — route groups, a
// sub-router mounted on a prefix — imports another subpackage instead, and
// nothing in the modules that register routes changes.
//
// Besides the engine itself, the seam asks this package to attribute a refused
// mount to the mounted pattern it clashes with; the engine here answers that
// with the multiplexer's own pairwise judgement.
//
// In this package net/http keeps its own name and the parent package takes the
// alias, the opposite of the direction inside the parent package itself, which
// is named http and has to alias the standard library.
package stdmux

import (
	"net/http"

	"github.com/vislake/speed/pkg/core"
	speedhttp "github.com/vislake/speed/pkg/http"
)

// moduleName is this module's name in the registry: the release unit's name
// plus this subpackage's, which is the form the design gives an implementation
// subpackage. A diagnostic line naming it therefore says both which entry point
// it is about and which engine is behind it, and two entry point
// implementations in one process are two distinct registrations rather than a
// duplicate name at init time.
//
// The configuration namespace is a different thing and stays "http". Input
// items hang from the namespace a schema declares and the module name takes no
// part in those paths, so this name does not move the configuration section.
// Both implementation subpackages consequently read the same namespace, which
// is one of the reasons the design gives for the exclusive delivery of Router
// seldom being where a second entry point is caught: two of them declare the
// same input items, and the manifest config collects fails over that before
// resolution is reached.
const moduleName = "http.stdmux"

// init registers this module with the process registry, so a host that imports
// this package has its entry point assembled from its configuration.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
func Module() core.Module { return speedhttp.Module(moduleName, newEngine) }

// engine is this subpackage's routing engine: the standard library's
// multiplexer, plus the attribution the parent's mounting point asks of the
// layer that binds an engine.
//
// The type is unexported and nothing outside this package names it. What
// crosses the seam is the parent's Engine interface; the attribution method is
// answered from here, where the engine is.
type engine struct{ mux *http.ServeMux }

// newEngine builds the routing engine of one endpoint. A fresh multiplexer per
// endpoint: the routes of the management endpoint and those of the public one
// are separate sets, which is the whole reason an assembly has more than one
// endpoint.
func newEngine() speedhttp.Engine { return &engine{mux: http.NewServeMux()} }

// ServeHTTP dispatches the request through the multiplexer.
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.mux.ServeHTTP(w, r) }

// Handle binds h to a pattern on the multiplexer.
func (e *engine) Handle(pattern string, h http.Handler) { e.mux.Handle(pattern, h) }

// AttributeRefusal names the pattern already mounted that a refused one cannot
// stand beside, by offering the pair to a multiplexer of its own. ServeMux
// carries the mounted patterns as a set, but judges a registration against
// them one at a time — that is what its own complaint quotes — so the partner
// is the one mounted pattern whose pair reproduces the refusal.
//
// The mounted patterns arrive in registration order, and the scan follows it,
// so one set of routes always names the same pair. A pattern that cannot be
// mounted on its own is skipped: it reproduces nothing, and the mount already
// bound it.
//
// An engine that judged a set as a whole could only answer differently, and
// the parent's seam leaves that to the engine; this one judges pairs, so an
// unanswerable refusal is not a case it has.
func (e *engine) AttributeRefusal(refused string, mounted []string) (string, bool) {
	for _, candidate := range mounted {
		mux := http.NewServeMux()
		if refusedByMux(mux, candidate) {
			continue
		}
		if refusedByMux(mux, refused) {
			return candidate, true
		}
	}
	return "", false
}

// refusedByMux mounts the pattern on the multiplexer and reports whether the
// engine refused it. The pattern is mounted on a fresh multiplexer, so a
// refusal is about this pattern beside nothing else.
func refusedByMux(mux *http.ServeMux, pattern string) (refused bool) {
	defer func() { refused = recover() != nil }()
	mux.Handle(pattern, http.NotFoundHandler())
	return false
}
