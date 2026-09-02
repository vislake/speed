// Package main is examples/reference-app's minimal starter skeleton --
// exactly the kind of "minimal starter skeleton...freely editable by
// consumers" root CLAUDE.md's "Shape" section describes, not a business
// module. It never goes through pkgcore.Module.Register itself; its whole
// job is wiring one together (see buildServer below) and running it.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

const (
	// defaultPort is used when the PORT environment variable is unset.
	defaultPort = "8080"

	// defaultSQLitePath is used when SPEED_DB_PATH is unset. It is a
	// relative path so `go run ./cmd/server` works with zero setup, per
	// root CLAUDE.md's "task dev must work in the demo profile" rule
	// applied to this example's own entry point.
	defaultSQLitePath = "reference-app.db"

	// shutdownTimeout bounds how long graceful shutdown waits for
	// in-flight requests to finish before giving up.
	shutdownTimeout = 10 * time.Second

	// readHeaderTimeout bounds how long the server waits to receive a
	// request's headers before aborting the connection -- protects
	// against slow-header (Slowloris-style) connections that trickle
	// bytes to hold a socket open indefinitely.
	readHeaderTimeout = 5 * time.Second

	// healthzPath is the one route exempted from tenant resolution -- see
	// buildServer's use of tenancy.WithAllowlist.
	healthzPath = "/healthz"

	// metricsPath is the demo profile's Prometheus scrape endpoint,
	// exempted from tenant resolution for exactly the same reason
	// healthzPath is: a scraper (or a human's browser, per
	// docs/internal/09-observability.md's own description of the demo
	// profile) has no demo Host to send and must not depend on one.
	metricsPath = "/metrics"
)

// demoHostTenants is a hard-coded, obviously-temporary Host -> TenantID
// lookup standing in for the Resolver authn will eventually supply for
// authenticated requests -- see go/tenancy/AGENTS.md's "Why there is no
// JWTResolver here". It exists only so this reference app has *some* way
// to demonstrate real multi-tenant behavior end to end before authn
// exists.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real Resolver must derive the tenant from a source the server itself
// controls (a verified token's claims, a database-backed custom-domain
// table) -- never an unauthenticated, static Host map like this one, which
// anyone can trigger just by setting the Host header on an HTTP request.
// See go/tenancy/resolver.go's own Resolver doc comment for the same rule
// stated as a hard requirement on every implementation.
//
// An unrecognized Host deliberately has no entry here: strictHostResolver
// below (not tenancy.DomainResolver) is what buildServer wires up in front
// of this app's real routes, and it fails the request rather than invent a
// tenant for a Host that is missing from this map -- see
// strictHostResolver's own doc comment for why.
var demoHostTenants = map[string]pkgcore.TenantID{
	"acme.demo.localhost":   "tenant-acme",
	"globex.demo.localhost": "tenant-globex",
}

// strictHostResolver resolves a request's tenant from its Host header
// against a fixed lookup table, failing the request (a non-nil error,
// which tenancy.Middleware turns into 403 Forbidden) when the Host does
// not match any configured tenant.
//
// buildServer wires this, not tenancy.DomainResolver, in front of the
// notes Module's real routes -- on purpose. DomainResolver's own doc
// comment scopes its fallback-to-a-default-tenant behavior to
// UNAUTHENTICATED, pre-auth display decisions only ("rendering the right
// brand on a login page... it grants no data access"), and
// go/tenancy/AGENTS.md's "Rules" section says the same thing as a hard
// requirement on every other Resolver: "Do not copy DomainResolver's
// empty-Host-falls-back-to-default behavior into a general pattern...
// Every other Resolver should return a non-nil error rather than invent or
// default a tenant." This app's notes API is real, persisted CRUD data,
// not a pre-auth display decision, and this app has no login page or
// other pre-auth route that would need DomainResolver's leniency --
// /healthz needs no tenant at all, resolved or not, which is exactly what
// its WithAllowlist entry below is for. So an unrecognized Host must fail
// closed here exactly like every other resolution failure, instead of
// landing in a shared, unauthenticated bucket that any anonymous caller
// could read from and write to just by setting an arbitrary Host header.
//
// strictHostResolver stands in for the Resolver authn will eventually
// supply from a verified access token (see demoHostTenants' own doc
// comment); unlike tenancy.DomainResolver, it is the closer approximation
// of the two, since authn's resolver will also fail closed on a caller it
// cannot identify rather than default them into a shared tenant.
type strictHostResolver struct {
	hostTenants map[string]pkgcore.TenantID
}

// Resolve implements tenancy.Resolver.
func (s strictHostResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	if tid, ok := s.hostTenants[r.Host]; ok && tid != "" {
		return tid, nil
	}
	return "", fmt.Errorf("reference-app: no tenant configured for host %q", r.Host)
}

// compile-time check that strictHostResolver satisfies tenancy.Resolver.
var _ tenancy.Resolver = strictHostResolver{}

// serverConfig is main.go's own wiring configuration. It is a plain
// struct, not pkgcore/config's dynamic configuration (which is not a real
// module yet -- see root CLAUDE.md's M0 status): this is main.go, the
// minimal starter skeleton, never a business module, so it never goes
// through Module.Register either.
type serverConfig struct {
	Profile     pkgcore.Profile
	Port        string
	SQLitePath  string
	HostTenants map[string]pkgcore.TenantID
}

// configFromEnv reads serverConfig from the environment, defaulting to the
// demo profile on SQLite so `go run ./cmd/server` genuinely starts a
// working server with zero external dependencies.
func configFromEnv() (serverConfig, error) {
	profileStr := os.Getenv("SPEED_PROFILE")
	if profileStr == "" {
		profileStr = string(pkgcore.ProfileDemo)
	}
	profile, err := pkgcore.ParseProfile(profileStr)
	if err != nil {
		return serverConfig{}, err
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv("SPEED_DB_PATH")
	if dbPath == "" {
		dbPath = defaultSQLitePath
	}

	return serverConfig{
		Profile:     profile,
		Port:        port,
		SQLitePath:  dbPath,
		HostTenants: demoHostTenants,
	}, nil
}

// buildServer wires the reference app's Kernel, the notes Module, its
// migrations, and tenancy's middleware into a single http.Handler. It is
// the one place that wiring logic lives -- both main() and
// server_test.go's end-to-end test call it, so the two can never drift
// into testing a different wiring than the one that actually runs.
//
// It returns the composed handler and a cleanup function that closes the
// underlying database connection; the caller must call cleanup once done
// with the handler.
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, error) {
	if cfg.Profile != pkgcore.ProfileDemo {
		return nil, nil, fmt.Errorf(
			"reference-app: profile %q is not wired in this example yet; only %q is supported until production infrastructure (PostgreSQL, Redis, ...) lands",
			cfg.Profile, pkgcore.ProfileDemo)
	}

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		return nil, nil, fmt.Errorf("reference-app: open database: %w", err)
	}
	cleanup := func() error {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return dbErr
		}
		return sqlDB.Close()
	}

	notesModule := notes.NewModule(db)

	migrationRegistry := dbkit.NewMigrationRegistry()
	if regErr := migrationRegistry.Register(notesModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: apply migrations: %w", applyErr)
	}

	reg, err := pkgcore.NewKernel(cfg.Profile).Bootstrap(ctx, notesModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: bootstrap kernel: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)
	mountModuleRoutes(mux, reg)

	// strictHostResolver -- not tenancy.DomainResolver -- gates the mux:
	// see its own doc comment above for why an unrecognized Host must fail
	// closed here instead of falling back to a shared default tenant.
	resolver := strictHostResolver{hostTenants: cfg.HostTenants}

	// The entire mux -- including /healthz -- runs behind
	// tenancy.Middleware; tenancy.WithAllowlist is what lets /healthz work
	// with no tenant resolved at all, rather than assembling a second,
	// unprotected mux by hand alongside it. Every route a real module
	// mounts stays fail-closed by default; only the one route named here
	// is exempt.
	//
	// This allowlist is not a theoretical safeguard here: strictHostResolver
	// genuinely fails resolution for any Host that is not
	// acme.demo.localhost or globex.demo.localhost -- including the Host a
	// plain `curl localhost:8080/healthz` sends, and a request with no Host
	// header at all -- so /healthz depends on this allowlist, in real
	// production wiring, to return 200 for exactly the orchestrators and
	// humans most likely to probe it without ever setting a demo Host.
	//
	// Both GET and HEAD are allowlisted for it, not GET alone: net/http's
	// ServeMux automatically serves HEAD /healthz from the "GET
	// "+healthzPath pattern registered above (Go's long-standing
	// GET-implies-HEAD convenience), but tenancy.Middleware does NOT apply
	// that same convenience to WithAllowlist -- its own doc comment says
	// so explicitly: "allowlist http.MethodHead explicitly if a health
	// check needs it too." Allowlisting GET alone would leave HEAD
	// /healthz 403ing under this exact resolver the moment anything (a
	// load balancer, a script) probes it with HEAD instead of GET --
	// exactly the class of bug this allowlist exists to prevent, just on
	// the one method easy to forget because the mux layer papers over it.
	handler := tenancy.Middleware(resolver,
		tenancy.WithAllowlist(http.MethodGet, healthzPath),
		tenancy.WithAllowlist(http.MethodHead, healthzPath),
		tenancy.WithAllowlist(http.MethodGet, metricsPath),
		tenancy.WithAllowlist(http.MethodHead, metricsPath),
	)(mux)
	return handler, cleanup, nil
}

// mountModuleRoutes copies every route reg's modules mounted onto mux.
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// ("/api/v1/notes") from a subtree pattern ("/api/v1/notes/", matching
// everything below it): registering only the subtree pattern would make
// ServeMux redirect a bare request for the exact path with an HTTP
// redirect instead of serving it directly -- which would silently break a
// POST, since a redirect is not guaranteed to preserve the method or body
// across every client. pkgcore.MountedRoute's own doc comment says the
// Handler "serves every request below Path", meaning it must be reachable
// at Path itself AND at everything nested below it -- so both patterns are
// registered explicitly here, pointing at the same Handler, instead of
// relying on ServeMux's implicit redirect-on-missing-slash behavior.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry) {
	for _, route := range reg.Routes.Routes() {
		mux.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", route.Handler)
		}
	}
}

// healthzHandler always returns 200 with no tenant required. It is
// allowlisted in buildServer above, so an orchestrator's liveness probe
// never depends on tenant resolution succeeding.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// metricsHandler serves whatever obs.MetricsHandler() currently returns --
// a real Prometheus scrape endpoint once main.go's run has called
// obs.Init, or a 404 explaining why before that (see MetricsHandler's own
// doc comment). It is fetched fresh on every request rather than captured
// once when buildServer constructs the mux, so this route's behavior does
// not depend on Init having already run by mount time: run() does call
// Init first (see main.go), but this indirection keeps that an
// implementation detail of main.go rather than a hidden requirement on
// buildServer's caller: a test that calls buildServer directly (as
// server_test.go's TestBuildServer_Metrics_NoTenantRequired does) can mount
// the route and assert on it without needing to care whether obs.Init has
// run yet in this process, or ever will -- see that test's own doc comment
// for exactly which weaker property it falls back to proving as a result.
// A test that instead builds its own mux around this same handler, as
// TestMetricsAllowlist_ResolutionFailure_StillReturns200 does, is free to
// call obs.Init itself first for a deterministic answer.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	obs.MetricsHandler().ServeHTTP(w, r)
}
