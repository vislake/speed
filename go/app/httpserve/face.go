package httpserve

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"

	"github.com/vislake/speed/go/pkgcore"
)

// face.go carries the component's product: the declaration faces modules
// mount their routes and middleware on, the reads the host and the chain
// derive their handler from, and the listener's state serve.go drives.

// Face is the http component's product. One value implements both
// declaration faces -- pkgcore.RouteRegistrar and pkgcore.MiddlewareRegistrar
// -- so a component that only declares platform middleware consumes the
// middleware token alone, and the route face stays out of its dependency
// set. It also satisfies chain.RouteSource (MountedRoutes / Middlewares):
// the fixed chain's derivation reads the accumulated declarations from the
// very product that recorded them.
//
// # The write gate
//
// Both declaration faces accept writes only while the Init stage runs. The
// gate is the face's own -- it reads the assembly's stage through
// (*pkgcore.ComponentRegistry).Stage -- because the declarations it guards
// feed a handler that is assembled once, in the Serve stage: a mount
// landing after assembly would be silently ignored, which is the one
// outcome worse than a loud refusal. A write outside Init is a wiring
// error, so Mount panics (its interface cannot return an error, the shape
// the retired registry seat established) and Add returns an error wrapping
// pkgcore.ErrStageViolation.
type Face struct {
	reg    *pkgcore.ComponentRegistry
	cfg    httpConfig
	policy *LinkPolicy

	mu          sync.Mutex
	routes      []pkgcore.MountedRoute
	middlewares []func(http.Handler) http.Handler

	// handler is the composed face; server and listener are the listening
	// half. All three are written by the Serve stage and read afterwards;
	// the drain machinery follows the two-beat Stop contract (serve.go).
	// listening records that the Serve stage bound the listener (it stays
	// true after Stop closes it -- the question it answers is "did this
	// face ever open its listener", not "is the socket accepting").
	handler   http.Handler
	server    *http.Server
	listener  net.Listener
	addr      string
	listening bool
	drained   chan struct{}
	drainErr  error

	stopOnce sync.Once
}

// Mount implements pkgcore.RouteRegistrar. It records the route in mount
// order; duplicate paths are not rejected here because the routing
// implementation decides how it resolves overlaps (net/http's ServeMux
// panics at mount time, which is the loudest available report of a wiring
// conflict). A call outside the Init stage panics with an error wrapping
// pkgcore.ErrStageViolation.
func (f *Face) Mount(path string, handler http.Handler) {
	if err := f.writable("route face", "Mount"); err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, pkgcore.MountedRoute{Path: path, Handler: handler})
}

// Routes implements pkgcore.RouteRegistrar: every route mounted so far, in
// mount order. Reads are legal at any point (before any mount, the result
// is nil).
func (f *Face) Routes() []pkgcore.MountedRoute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routes)
}

// MountedRoutes implements chain.RouteSource: the same reading as Routes,
// named for the chain's derivation.
func (f *Face) MountedRoutes() []pkgcore.MountedRoute { return f.Routes() }

// Add implements pkgcore.MiddlewareRegistrar. It registers platform-wide
// middleware in registration order (the first added wraps outermost); a nil
// entry is refused with pkgcore.ErrNilMiddleware, and a call outside the
// Init stage is refused with an error wrapping pkgcore.ErrStageViolation.
// Nothing is registered when the call returns an error.
func (f *Face) Add(mw ...func(http.Handler) http.Handler) error {
	if err := f.writable("middleware face", "Add"); err != nil {
		return err
	}
	for _, m := range mw {
		if m == nil {
			return pkgcore.ErrNilMiddleware
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.middlewares = append(f.middlewares, mw...)
	return nil
}

// Middlewares implements pkgcore.MiddlewareRegistrar and chain.RouteSource:
// every registered middleware, in registration order.
func (f *Face) Middlewares() []func(http.Handler) http.Handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.middlewares)
}

// writable reports whether a declaration write may land now: only while the
// Init stage runs. The refusal names the face, the method and the stage the
// assembly is actually in.
func (f *Face) writable(face, method string) error {
	stage := f.reg.Stage()
	if stage == pkgcore.StageInit {
		return nil
	}
	return fmt.Errorf("%w: the http component's %s accepts writes through %s only while the Init stage runs; current stage is %s", pkgcore.ErrStageViolation, face, method, stage)
}

// Handler returns the composed handler: the protected face (the link
// policy's outer wrapper around the fixed chain around the mux, or the
// chainless composition) the Serve stage assembled. It is nil until the
// Serve stage has run.
func (f *Face) Handler() http.Handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handler
}

// Addr returns the listen address: empty until the Serve stage has run,
// the configured address once it has composed (a compose-only face stops
// there), and the listener's own address -- the real port a :0 bind
// picked -- once the socket is bound.
func (f *Face) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

// Listening reports whether the Serve stage bound the listener: false for a
// compose-only configuration (listen disabled) and before Serve, true from
// the moment the socket is bound -- including after Stop closed it, since
// the question is whether this face ever opened its listener.
func (f *Face) Listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listening
}
