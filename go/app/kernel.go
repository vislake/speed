package app

import (
	"time"

	"github.com/vislake/speed/go/config"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

const (
	// AuthnAPIPath is authn's own HTTP mount point: the platform
	// convention go/authn/module.go mounts its whole API surface at, and
	// the prefix a host's router uses to dispatch that subtree onto its
	// own branch (authn's routes resolve the tenant from the Principal's
	// own claim per operation and must never sit behind ordinary tenant
	// resolution -- see go/authn/AGENTS.md). It is restated here rather
	// than imported because authn's own constant is unexported; a rename
	// there is caught by the route-mounting paths this constant feeds
	// (each host's router would stop matching authn's real mount point).
	AuthnAPIPath = "/api/v1/authn"

	// ReadHeaderTimeout bounds how long the server waits to receive a
	// request's headers before aborting the connection -- protects
	// against slow-header (Slowloris-style) connections that trickle
	// bytes to hold a socket open indefinitely. The engine's HTTP face
	// applies it to the http.Server it composes.
	ReadHeaderTimeout = 5 * time.Second

	// ShutdownTimeout bounds how long graceful shutdown waits for
	// in-flight requests to finish before giving up, and bounds the
	// background worker's own drain the same way; the engine's ordered
	// shutdown applies it to both steps.
	ShutdownTimeout = 10 * time.Second
)

// PreAuthAllowlist returns the tenancy.Middleware options that exempt the
// platform's fixed pre-auth surface -- healthz, metrics (their paths are
// observability's own constants) and config's two pre-auth display
// endpoints -- under both GET and HEAD. These are the routes that must
// work before a Principal exists (a probe, a scraper and the login page a
// sign-in flow presupposes); every other route a host mounts is refused by
// tenancy.Middleware's fail-closed default until a verified Principal
// resolves a tenant, with no per-route wrapping needed.
//
// Both methods are covered by one tenancy.AllowlistGETAndHEAD call because
// net/http's ServeMux automatically serves HEAD from a registered "GET
// "+path pattern (Go's long-standing GET-implies-HEAD convenience) while
// tenancy.Middleware does NOT extend WithAllowlist's exemption the same
// way -- allowlisting GET alone would leave HEAD one middleware change
// away from a 403 the moment anything probes it with HEAD instead of GET.
//
// A host with its own pre-auth routes (a public share link, an
// unauthenticated whoami endpoint) adds its own tenancy.WithAllowlist
// entries beside this set -- chain.Config.ExtraAllowlist is the place.
// authn's own subtree is deliberately absent: it never sits behind
// tenancy.Middleware at all -- a host's router dispatches AuthnAPIPath
// onto a branch of its own (see that constant's doc comment), which is
// also the only shape that lets enterprise SSO's dynamically named
// "oidc:<tenant>" login-start path work, since no fixed (method, path)
// allowlist could enumerate it.
func PreAuthAllowlist() []tenancy.MiddlewareOption {
	return []tenancy.MiddlewareOption{
		tenancy.AllowlistGETAndHEAD(
			obs.HealthzPath,
			obs.MetricsPath,
			config.PathPublic,
			config.PathSystemFeatures,
		),
	}
}

// RegisterMountedRoutes hands every obs.Middleware this process later
// constructs the host's real route table -- obs.HealthzPath and
// obs.MetricsPath (which no module registers) plus every route reg's
// modules mounted -- so the route-label limiter the middleware builds
// (go/observability's cardinality bound on http.route:
// obs.MaxRouteLabelValues distinct values, then a fixed overflow bucket)
// reserves a slot for each real route BEFORE any request traffic arrives.
// Without the reservation the budget is first-come-first-served: an
// attacker sending enough distinct garbage paths right after startup fills
// it, and every genuine route first requested afterwards is recorded under
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
		{Path: obs.HealthzPath},
		{Path: obs.MetricsPath},
	}, reg.Routes.Routes()...))
}
