//go:build ignore

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

const (
	// healthzPath is the one route exempted from tenant resolution -- an
	// orchestrator's liveness probe must never depend on tenant resolution
	// succeeding.
	healthzPath = "/healthz"

	// metricsPath is the Prometheus scrape endpoint, exempted from tenant
	// resolution for exactly the same reason healthzPath is: a scraper has
	// no tenant to name and must not depend on one.
	metricsPath = "/metrics"
)

// buildServer wires this project's Kernel, the modules the generator
// selected for it, their migrations, and the handler into a single
// http.Handler -- the generated project's only composition point,
// mirroring examples/reference-app/cmd/server/server.go with every
// demo-specific piece removed. It returns the composed handler and a
// cleanup function that closes everything buildServer opened (the attached
// service and the underlying database connection); the caller must call
// cleanup once done with the handler.
//
// This composition wires ONLY the config module (the generator's --with
// set for this project; the README's environment table and this project's
// go.mod show which module set a differently-generated project carries) --
// the module every composition requires, whose two pre-auth display
// endpoints render the login page a sign-in flow presupposes. It is the
// empty selection: no authn, no org, no rbac.
//
// There is deliberately NO middleware chain here. A middleware chain exists
// to turn a verified caller into tenant context, and this composition has
// no authn module and therefore no verification step and no Principal:
// authn.Middleware's verifier has nothing to verify, and
// tenancy.Middleware's resolver has no claim to resolve a tenant from.
// Wrapping the mux in tenancy.Middleware anyway, with a resolver that can
// never succeed, would fail closed every route below it for no gain --
// fail-closed protection is worth exactly what it protects, and none of
// the routes this composition serves (healthz, metrics, config's two
// pre-auth display endpoints) needs a tenant to answer correctly: each is
// pre-auth by its owning module's design, and config's endpoints read the
// platform-default tier when the host map matches nothing (their own
// contract, below). Every other route in this process is whatever the
// owner mounts on the returned handler later -- and the owner's first step
// toward anything tenant-shaped is regenerating with --with authn (or
// hand-wiring the chain from an authn-wiring selection's server.go), not
// hand-rolling a weaker chain here.
//
// Host seams deliberately left unwired, each failing closed per the owning
// module's contract and each the owner's first task: the config resolver's
// host map, which deliberately matches NOTHING so the display endpoints
// serve platform defaults to every caller, never an error (the login-page
// rule; a static unauthenticated Host map would violate tenancy's own
// Resolver contract, go/tenancy/resolver.go).
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, error) {
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		return nil, nil, fmt.Errorf("__APP_NAME__: open database: %w", err)
	}

	// configService is filled by configModule.Attach below (nil until
	// then). cleanup closes the attached service first, then the database,
	// last; every close is attempted even when an earlier one failed, and
	// the first error wins.
	var configService *config.Service

	cleanup := func() error {
		var firstErr error
		keepErr := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if configService != nil {
			keepErr(configService.Close())
		}
		sqlDB, dbErr := db.DB()
		keepErr(dbErr)
		if sqlDB != nil {
			keepErr(sqlDB.Close())
		}
		return firstErr
	}

	cipher, err := dbkit.NewCipher(cfg.ConfigKey)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: build the config master cipher: %w", err)
	}

	// The config module is required in every generated composition (its two
	// pre-auth display endpoints render the login page a sign-in flow
	// presupposes). Its resolver's lookup deliberately matches NOTHING, so
	// the display endpoints serve platform defaults to every caller until
	// the owner wires a real host-to-tenant source; an empty default tenant
	// maps unmatched hosts onto the "platform defaults" tier rather than an
	// error -- exactly the login-page rule (see buildServer's doc comment).
	configModule := config.NewModule(db,
		config.WithCipher(cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				return "", false
			},
			"",
		)),
	)

	migrationRegistry := dbkit.NewMigrationRegistry()
	for _, m := range []pkgcore.Module{configModule} {
		if regErr := migrationRegistry.Register(m); regErr != nil {
			_ = cleanup()
			return nil, nil, fmt.Errorf("__APP_NAME__: register migrations: %w", regErr)
		}
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: apply migrations: %w", applyErr)
	}

	// Bootstrap registers the selected modules in argument order, matching
	// the migration order above -- see buildServer's doc comment.
	// WithDeploymentMode declares the topology the assembled composition is
	// validated against; it never selects an implementation (the deployment
	// mode and the implementation composition are orthogonal axes --
	// docs/internal/03-deployment-modes.md), and the validation refuses a
	// composition the declared mode cannot run, naming the seam, the
	// implementation and the missing capability.
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(cfg.DeploymentMode)}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, configModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: bootstrap kernel: %w", err)
	}

	// Attach runs strictly after Bootstrap, exactly once: what it freezes
	// is the schema snapshot of every config item and feature flag the
	// modules declared during Register (config's own Attach doc comment).
	configService, err = configModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: attach the config module: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, healthzHandler)
	mux.HandleFunc(metricsPath, metricsHandler)
	if err := mountModuleRoutes(mux, reg); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: mount module routes: %w", err)
	}

	// The bare mux IS the handler: no authn module means no verifier and no
	// Principal, so there is no middleware chain to wrap it in -- see
	// buildServer's doc comment above for the full reasoning.
	return mux, cleanup, nil
}

// mountModuleRoutes copies every route reg's modules mounted onto mux.
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// from a subtree pattern (one ending in "/", matching everything below
// it): registering only the subtree pattern would make ServeMux redirect a
// bare request for the exact path with an HTTP redirect instead of serving
// it directly -- which would silently break a POST, since a redirect is
// not guaranteed to preserve the method or body across every client.
// pkgcore.MountedRoute's own doc comment says the Handler "serves every
// request below Path", meaning it must be reachable at Path itself AND at
// everything nested below it -- so both patterns are registered explicitly
// here, pointing at the same Handler, instead of relying on ServeMux's
// implicit redirect-on-missing-slash behavior.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry) error {
	for _, route := range reg.Routes.Routes() {
		mux.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", route.Handler)
		}
	}
	return nil
}

// healthzHandler always returns 200 with no tenant required.
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
// Init first (see main.go), but the indirection keeps that an
// implementation detail of main.go rather than a hidden requirement on
// buildServer's caller -- a test that calls buildServer directly can mount
// the route and assert on it without needing to care whether obs.Init has
// run yet in this process, or ever will.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	obs.MetricsHandler().ServeHTTP(w, r)
}
