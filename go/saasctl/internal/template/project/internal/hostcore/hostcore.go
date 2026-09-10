//go:build ignore

// Package hostcore is the host-neutral kernel every speed application
// shares: the liveness endpoints and their four pre-auth allowlist
// entries, the module-route mounting rule, the mounted-route label seed
// and the serve/graceful-shutdown lifecycle.
//
// The package exists as ONE file, present byte-identically in the two
// hosts this repository maintains -- a generated project's own
// internal/hostcore (materialized from this very file by `saasctl new`)
// and examples/reference-app/internal/hostcore -- with
// tools/check_host_core_parity.py as the gate that keeps them identical:
// an edit to either copy without the other fails that check, so the
// shared kernel cannot drift back into two hand-maintained variants. To
// change shared host behavior, edit both copies (the reference app's
// copy path is named in the checker's output) in one change.
//
// What belongs here is exactly what is host-neutral: every symbol below
// composes only platform contracts (pkgcore's route table, tenancy's
// middleware options, config's pre-auth paths, observability's handler
// and middleware) and stdlib HTTP, and none of it names a tenant, a
// module set, a demo layer or an application. What stays host-specific
// by design: the assembly itself (each host's buildServer/BuildServer),
// the middleware chain's host-owned additions (an application's tenant
// status resolver, impersonation decorator, rbac/org route guards, the
// demo identity layer), the bootstrap configuration surface, and every
// business route.
package hostcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vislake/speed/go/config"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

const (
	// HealthzPath is the liveness endpoint every host mounts for its
	// orchestrator's probe; HealthzHandler always answers 200 and no
	// tenant is ever required, which is also why PreAuthAllowlist
	// carries it (a probe must not depend on tenant resolution).
	HealthzPath = "/healthz"

	// MetricsPath is the Prometheus scrape endpoint; MetricsHandler
	// serves obs.MetricsHandler()'s current value, and PreAuthAllowlist
	// exempts it for the same reason it exempts HealthzPath.
	MetricsPath = "/metrics"

	// AuthnAPIPath is authn's own HTTP mount point: the platform
	// convention go/authn/module.go mounts its whole API surface at, and
	// the prefix each host's router uses to dispatch that subtree onto
	// its own branch (authn's routes resolve the tenant from the
	// Principal's own claim per operation and must never sit behind
	// ordinary tenant resolution -- see go/authn/AGENTS.md). It is
	// restated here rather than imported because authn's own constant is
	// unexported; a rename there is caught by the route-mounting paths
	// this constant feeds (each host's router would stop matching
	// authn's real mount point).
	AuthnAPIPath = "/api/v1/authn"

	// ReadHeaderTimeout bounds how long the server waits to receive a
	// request's headers before aborting the connection -- protects
	// against slow-header (Slowloris-style) connections that trickle
	// bytes to hold a socket open indefinitely. ServeUntilShutdown
	// applies it to the http.Server it builds.
	ReadHeaderTimeout = 5 * time.Second

	// ShutdownTimeout bounds how long graceful shutdown waits for
	// in-flight requests to finish before giving up; ServeUntilShutdown
	// applies it to srv.Shutdown.
	ShutdownTimeout = 10 * time.Second
)

// HealthzHandler always returns 200 with no tenant required. It is
// allowlisted in PreAuthAllowlist, so an orchestrator's liveness probe
// never depends on tenant resolution succeeding.
func HealthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// MetricsHandler serves whatever obs.MetricsHandler() currently returns
// -- a real Prometheus scrape endpoint once obs.Init has run, or a 404
// explaining why before that (see MetricsHandler's own doc comment). It
// is fetched fresh on every request rather than captured once when a
// host mounts the route, so this route's behavior does not depend on
// Init having already run by mount time: the ordinary boot calls Init
// before serving, but the indirection keeps that an implementation
// detail of the boot sequence rather than a hidden requirement on the
// assembly -- a test that builds a mux by hand and mounts this handler
// can assert on it without needing to care whether obs.Init has run yet
// in this process, or ever will.
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	obs.MetricsHandler().ServeHTTP(w, r)
}

// MountLiveness registers the two host-owned liveness routes --
// HealthzPath and MetricsPath -- on mux as GET patterns, which is what
// orchestrator probes and Prometheus scrapers send. net/http's ServeMux
// serves HEAD from a registered "GET "+path pattern automatically, and
// any other method answers 405 Method Not Allowed rather than reaching a
// handler that has nothing to say to it.
func MountLiveness(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodGet+" "+HealthzPath, HealthzHandler)
	mux.HandleFunc(http.MethodGet+" "+MetricsPath, MetricsHandler)
}

// PreAuthAllowlist returns the tenancy.Middleware options that exempt
// the platform's fixed pre-auth surface -- healthz, metrics and config's
// two pre-auth display endpoints -- for both GET and HEAD. These are the
// routes that must work before a Principal exists (a probe, a scraper
// and the login page a sign-in flow presupposes); every other route a
// host mounts is refused by tenancy.Middleware's fail-closed default
// until a verified Principal resolves a tenant, with no per-route
// wrapping needed.
//
// Both GET and HEAD are listed: net/http's ServeMux automatically serves
// HEAD from a registered "GET "+path pattern (Go's long-standing
// GET-implies-HEAD convenience), but tenancy.Middleware does NOT extend
// WithAllowlist's exemption the same way, so allowlisting GET alone
// would leave HEAD one middleware change away from a 403 the moment
// anything probes it with HEAD instead of GET.
//
// A host with its own pre-auth routes (a public share link, an
// unauthenticated whoami endpoint) appends its own tenancy.WithAllowlist
// entries to this slice. authn's own subtree is deliberately absent: it
// never sits behind tenancy.Middleware at all -- each host's router
// dispatches AuthnAPIPath onto a branch of its own (see that constant's
// doc comment), which is also the only shape that lets enterprise SSO's
// dynamically named "oidc:<tenant>" login-start path work, since no
// fixed (method, path) allowlist could enumerate it.
func PreAuthAllowlist() []tenancy.MiddlewareOption {
	return []tenancy.MiddlewareOption{
		tenancy.WithAllowlist(http.MethodGet, HealthzPath),
		tenancy.WithAllowlist(http.MethodHead, HealthzPath),
		tenancy.WithAllowlist(http.MethodGet, MetricsPath),
		tenancy.WithAllowlist(http.MethodHead, MetricsPath),
		tenancy.WithAllowlist(http.MethodGet, config.PathPublic),
		tenancy.WithAllowlist(http.MethodHead, config.PathPublic),
		tenancy.WithAllowlist(http.MethodGet, config.PathSystemFeatures),
		tenancy.WithAllowlist(http.MethodHead, config.PathSystemFeatures),
	}
}

// MountRoute registers handler on mux for path and for everything nested
// below it -- the mounting rule every module route follows.
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match
// pattern from a subtree pattern (one ending in "/", matching everything
// below it): registering only the subtree pattern would make ServeMux
// redirect a bare request for the exact path with an HTTP redirect
// instead of serving it directly -- which would silently break a POST,
// since a redirect is not guaranteed to preserve the method or body
// across every client. pkgcore.MountedRoute's own doc comment says the
// Handler "serves every request below Path", meaning it must be
// reachable at Path itself AND at everything nested below it -- so both
// patterns are registered explicitly here, pointing at the same Handler,
// instead of relying on ServeMux's implicit redirect-on-missing-slash
// behavior.
func MountRoute(mux *http.ServeMux, path string, handler http.Handler) {
	mux.Handle(path, handler)
	if !strings.HasSuffix(path, "/") {
		mux.Handle(path+"/", handler)
	}
}

// RegisterMountedRoutes hands every obs.Middleware this process later
// constructs the host's real route table -- HealthzPath and MetricsPath
// (which no module registers) plus every route reg's modules mounted --
// so the route-label limiter the middleware builds (go/observability's
// cardinality bound on http.route: obs.MaxRouteLabelValues distinct
// values, then a fixed overflow bucket) reserves a slot for each real
// route BEFORE any request traffic arrives. Without the reservation the
// budget is first-come-first-served: an attacker sending enough distinct
// garbage paths right after startup fills it, and every genuine route
// first requested afterwards is recorded under
// obs.RouteLabelOverflowValue for the life of the process, per-route
// metrics gone even though no bound was violated. A seeded route keeps
// its slot whatever garbage arrives later (RegisterMountedRoutes' own
// doc comment; the mechanism's behavioral proof is
// go/observability/middleware_test.go's
// TestMiddleware_RealRoutesSurviveGarbage_WhenSeeded).
//
// The call is a snapshot consumed at obs.Middleware CONSTRUCTION, so a
// host must make it at assembly time, after every module registered its
// routes and before the middleware that serves traffic is built -- not
// after the server starts listening.
func RegisterMountedRoutes(reg *pkgcore.Registry) {
	obs.RegisterMountedRoutes(append([]pkgcore.MountedRoute{
		{Path: HealthzPath},
		{Path: MetricsPath},
	}, reg.Routes.Routes()...))
}

// ServeUntilShutdown wraps handler in obs.Middleware and serves it on
// addr ("host:port" or ":port") until ctx is done (the caller's signal
// context) or the listener fails, then drains in-flight requests within
// ShutdownTimeout. appName is carried as the app_name attribute on every
// log line and prefixes the errors this function returns
// ("<appName>: serve: ..."), so each host stays attributable without
// bending the logging discipline (the message is a constant string; the
// host name is an attribute); deploymentMode is attached as the listing
// line's deployment_mode attribute.
//
// ctx must be the signal-derived context (which SHOULD observe
// cancellation), baseCtx the caller's logger-carrying base context
// (which must NOT): net/http's Server.BaseContext hands baseCtx,
// uncancelled, as the ancestor of every request's own context -- if it
// were the signal-derived ctx instead, every in-flight request's context
// would already be Done() the instant a shutdown signal arrived, racing
// handler code that checks ctx.Err() against the graceful drain
// srv.Shutdown is supposed to perform. Handing baseCtx here is also what
// makes obs.FromContext(r.Context()) inside a handler find the JSON
// logger the process attached at startup, in addition to the
// trace_id/tenant_id the middleware layers add per request: without it,
// net/http defaults every request's root context to a bare
// context.Background() and the logger attachment would never reach
// request-handling code at all.
//
// obs.Middleware wraps OUTSIDE the host's own middleware chain (built by
// the caller into handler): its position costs one real thing (a tenant
// is not yet known this far out -- see obs.AnnotateTenant, called from a
// handler once the host's chain has resolved one) and buys the useful
// one: every request gets a span and is counted here, including ones the
// inner chain goes on to reject with 401/403, which matters for spotting
// a flood of them.
func ServeUntilShutdown(ctx, baseCtx context.Context, handler http.Handler, addr, appName, deploymentMode string) error {
	instrumented := obs.Middleware(handler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           instrumented,
		ReadHeaderTimeout: ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}

	serveErr := make(chan error, 1)
	go func() {
		obs.FromContext(ctx).Info("server listening", "app_name", appName, "addr", srv.Addr, "deployment_mode", deploymentMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		obs.FromContext(ctx).Info("shutdown signal received", "app_name", appName)
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("%s: serve: %w", appName, err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s: graceful shutdown: %w", appName, err)
	}
	obs.FromContext(ctx).Info("server stopped cleanly", "app_name", appName)
	return nil
}
