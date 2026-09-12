package componenttest

import (
	"net/http"
	"slices"
	"sync"

	"github.com/vislake/speed/go/pkgcore"
)

// registrar.go carries the declaration-face recorders tests use when a
// component's declaration body mounts routes or declares platform
// middleware. Those faces are no longer registry seats: the http component
// (go/app/httpserve) provides them as its product, and a module reaches them
// through pkgcore.GetOptional[pkgcore.RouteRegistrar] (or
// MiddlewareRegistrar) during its Init turn. A test that drives a
// declaration body therefore puts one of these recorders into the by-type
// context first (reg.Put(NewRouteRecorder())) and reads what the body
// mounted afterwards.
//
// The recorders are deliberately gate-free: the stage gate is the http
// component's own (its registrar refuses writes outside the Init stage), and
// these recorders exist to stand in for that product, not to re-implement
// its gate. Mount ordering, copy-on-read and the middleware face's nil
// refusal match the product's contract, so a test's reading is the reading
// the real component would serve.

// RouteRecorder is a pkgcore.RouteRegistrar that records every mount, in
// mount order.
type RouteRecorder struct {
	mu     sync.Mutex
	routes []pkgcore.MountedRoute
}

// NewRouteRecorder returns an empty recorder.
func NewRouteRecorder() *RouteRecorder { return &RouteRecorder{} }

// Mount implements pkgcore.RouteRegistrar.
func (r *RouteRecorder) Mount(path string, handler http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append(r.routes, pkgcore.MountedRoute{Path: path, Handler: handler})
}

// Routes implements pkgcore.RouteRegistrar: every route mounted so far, in
// mount order.
func (r *RouteRecorder) Routes() []pkgcore.MountedRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.routes)
}

// MountedRoutes is the same reading named for go/app/chain's RouteSource
// interface, which the http component's product also answers structurally --
// so a test can hand a recorder to chain.Standard exactly where a real boot
// hands it the component's face.
func (r *RouteRecorder) MountedRoutes() []pkgcore.MountedRoute { return r.Routes() }

// MiddlewareRecorder is a pkgcore.MiddlewareRegistrar that records every
// declared middleware, in registration order, refusing a nil entry exactly
// as the contract requires.
type MiddlewareRecorder struct {
	mu  sync.Mutex
	mws []func(http.Handler) http.Handler
}

// NewMiddlewareRecorder returns an empty recorder.
func NewMiddlewareRecorder() *MiddlewareRecorder { return &MiddlewareRecorder{} }

// Add implements pkgcore.MiddlewareRegistrar.
func (r *MiddlewareRecorder) Add(mw ...func(http.Handler) http.Handler) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range mw {
		if m == nil {
			return pkgcore.ErrNilMiddleware
		}
	}
	r.mws = append(r.mws, mw...)
	return nil
}

// Middlewares implements pkgcore.MiddlewareRegistrar: every middleware
// registered so far, in registration order.
func (r *MiddlewareRecorder) Middlewares() []func(http.Handler) http.Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.mws)
}

// FaceRecorder bundles the two recorders into one value answering both
// declaration faces, the shape the http component's product has: a
// declaration body mounts through it and declares middleware through it,
// and a chain- or host-side test reads the accumulated declarations from
// the same value it handed the assembly -- including through
// go/app/chain's RouteSource interface, which the embedded reads satisfy
// structurally.
type FaceRecorder struct {
	*RouteRecorder
	*MiddlewareRecorder
}

// NewFaceRecorder returns an empty recorder with both faces open.
func NewFaceRecorder() *FaceRecorder {
	return &FaceRecorder{RouteRecorder: NewRouteRecorder(), MiddlewareRecorder: NewMiddlewareRecorder()}
}
