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
// In this package net/http keeps its own name and the parent package takes the
// alias, the opposite of the direction inside the parent package itself, which
// is named http and has to alias the standard library.
package stdmux

import (
	"net/http"

	"github.com/vislake/speed/pkg/core"
	speedhttp "github.com/vislake/speed/pkg/http"
)

// moduleName is this module's name in the registry. It is the plain "http"
// rather than the subpackage's own name: the name is what a diagnostic line
// calls the entry point, and which engine is behind it is not what a reader of
// that line is asking about. Two entry point implementations in one process
// therefore collide on the name at registration, before the exclusive delivery
// of Router could be resolved.
const moduleName = "http"

// init registers this module with the process registry, so a host that imports
// this package has its entry point assembled from its configuration.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
func Module() core.Module { return speedhttp.Module(moduleName, newEngine) }

// newEngine builds the routing engine of one endpoint. A fresh multiplexer per
// endpoint: the routes of the management endpoint and those of the public one
// are separate sets, which is the whole reason an assembly has more than one
// endpoint.
func newEngine() speedhttp.Engine { return http.NewServeMux() }
