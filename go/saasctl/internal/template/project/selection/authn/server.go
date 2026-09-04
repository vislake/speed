//go:build ignore

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so dbkit.Open below has a driver to build from -- this skeleton runs
	// only in standalone deployment mode.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
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

// devBlindIndexKey, devPIICipherKey and devPKILocalKeyCipherKey are authn's
// (and pki's) own committed-key placeholders, the same documented trade-off
// as config.go's devConfigKey: recognizable constants for zero-setup
// development, never secrets -- a real deployment must replace every one of
// them with real secret-manager material.
//
// Each protects something different and each must stay stable across
// restarts for a different reason: the blind-index key must stay IDENTICAL
// across restarts or every already-stored email/phone blind index becomes
// unfindable; devPIICipherKey seals authn's encrypted PII columns (email,
// phone, TOTP secrets) via authn.RegisterPIISerializer; and
// devPKILocalKeyCipherKey seals go/pki's LocalSigner private-key column
// (pki_local_keys, via pki.RegisterLocalKeySerializer) -- go/pki's signing
// keys themselves need no separate dev-seed derivation the way the deleted
// authn.KeySet default once did: they are generated once by
// pki.Service.EnsurePurpose (this file's authn.WithKeySource wiring below)
// and PERSIST in cfg.SQLitePath across restarts, exactly the durability
// docs/internal/22-pki.md's post-integration column describes -- a fresh key on
// every restart is no longer even possible, since the key now lives in the
// database rather than this process's memory. Each key here is
// deliberately a DIFFERENT value from every other one, because dbkit's
// key-separation rule (never let one key double as two different AEAD
// constructions) applies across modules, not only within one.
var (
	devBlindIndexKey = []byte{
		0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47,
		0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
		0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57,
		0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f,
	}
	devPIICipherKey = []byte{
		0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
		0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	}
	devPKILocalKeyCipherKey = []byte{
		0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
		0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
		0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
	}
)

// buildServer wires this project's Kernel, the modules the generator
// selected for it, their migrations, and the middleware chain into a
// single http.Handler -- the generated project's only composition point,
// mirroring examples/reference-app/cmd/server/server.go with every
// demo-specific piece removed. It returns the composed handler and a
// cleanup function that closes everything buildServer opened (the attached
// service and the underlying database connection); the caller must call
// cleanup once done with the handler.
//
// This composition wires the authn and config modules (the generator's
// --with set for this project; the README's environment table and this
// project's go.mod show which module set a differently-generated project
// carries) plus pki, which is not part of the --with set at all -- it
// follows authn silently (docs/internal/22-pki.md's section on where
// pki sits in saasctl's module selection set), supplying the KeySource
// authn's signing keys now live behind. Migrations register in the same order Bootstrap runs, so every
// Register-time declaration (authn's config items, permissions and events
// first, then config's own Register) lands before the step that freezes
// it. The middleware chain is authn.Middleware(verifier) then
// tenancy.Middleware(authn.NewPrincipalResolver()): authn first, so each
// token is verified exactly once and the tenant comes from the verified
// Principal's claims, never a Host header -- go/authn/AGENTS.md's "The
// middleware chain is authn, then tenancy" section carries the full
// reasoning. authn.Middleware is optional auth (a bad token is a 401, an
// absent one stays anonymous), so tenancy.Middleware's fail-closed default
// is what protects every route this file does NOT allowlist: such a route
// answers 403 without a valid Principal and needs no per-route wrapping.
// The allowlist covers healthz, metrics, config's two pre-auth display
// endpoints and authn's own pre-auth operations (authnPreAuthAllowlist
// below) -- the routes that must work before anyone has signed in.
//
// Host seams deliberately left unwired, each failing closed per the owning
// module's contract and each the owner's first task: authn's
// MembershipReader (session refresh re-verifies the session's current
// tenant against it, and an absent reader refuses rather than defaulting
// -- a freshly signed-in user has no tenant until the owner builds one);
// and the config resolver's host map, which deliberately matches NOTHING
// so unmatched hosts read platform defaults, never an error (the
// login-page rule; a static unauthenticated Host map would violate
// tenancy's own Resolver contract, go/tenancy/resolver.go).
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, error) {
	// authn's PII columns (email, phone, TOTP secrets) must have their
	// serializer registered BEFORE dbkit.Open: GORM resolves a model's
	// serializer while it parses the schema, and the registration is
	// process-global (authn.RegisterPIISerializer's own doc comment).
	piiCipher, err := dbkit.NewCipher(devPIICipherKey)
	if err != nil {
		return nil, nil, fmt.Errorf("__APP_NAME__: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return nil, nil, fmt.Errorf("__APP_NAME__: register authn's PII serializer: %w", regErr)
	}

	// go/pki's LocalSigner private-key column needs its own serializer
	// registered before dbkit.Open too, for the identical reason
	// authn.RegisterPIISerializer does -- GORM resolves a model's serializer
	// while it parses the schema (pki.RegisterLocalKeySerializer's own doc
	// comment).
	pkiLocalKeyCipher, err := dbkit.NewCipher(devPKILocalKeyCipherKey)
	if err != nil {
		return nil, nil, fmt.Errorf("__APP_NAME__: build pki's local-key cipher: %w", err)
	}
	if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
		return nil, nil, fmt.Errorf("__APP_NAME__: register pki's local-key serializer: %w", regErr)
	}

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

	// pki owns authn's signing-key lifecycle: LocalSigner (its own
	// zero-external-dependency default, exactly what this standalone
	// composition needs) generates and stores the key in cfg.SQLitePath,
	// so it persists across restarts with no dev-seed derivation required
	// -- see devPKILocalKeyCipherKey's own doc comment. pki is not part of
	// this generator's --with selection set (docs/internal/22-pki.md's
	// section on where pki sits in saasctl's module selection set): it follows authn silently,
	// which is why it is wired here rather than offered as its own choice.
	pkiModule := pki.NewModule(db)

	// authn's SMS sender is deliberately not wired: the module defaults an
	// absent sender to its console sender, the right zero-setup transport
	// for this standalone composition -- and under the distributed
	// deployment mode the same validation fails with
	// ErrMissingDistributedSMSSender naming the missing option, the honest
	// signal that a real deployment must wire a transport. See buildServer's
	// doc comment above for the MembershipReader absence.
	authnModule, err := authn.NewModule(db,
		authn.WithKeySource(pkiModule.Service()),
		authn.WithBlindIndexKey(devBlindIndexKey),
		authn.WithDeploymentMode(cfg.DeploymentMode),
	)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: build the authn module: %w", err)
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
	for _, m := range []pkgcore.Module{pkiModule, authnModule, configModule} {
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
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, pkiModule, authnModule, configModule)
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

	// The middleware chain: authn first, then tenancy (see buildServer's
	// doc comment above for the order reasoning, and authnPreAuthAllowlist
	// below for the routes that must work before anyone has signed in).
	handler := authn.Middleware(authnModule.Service().Verifier())(
		tenancy.Middleware(authn.NewPrincipalResolver(), append([]tenancy.MiddlewareOption{
			tenancy.WithAllowlist(http.MethodGet, healthzPath),
			tenancy.WithAllowlist(http.MethodHead, healthzPath),
			tenancy.WithAllowlist(http.MethodGet, metricsPath),
			tenancy.WithAllowlist(http.MethodHead, metricsPath),
			tenancy.WithAllowlist(http.MethodGet, config.PathPublic),
			tenancy.WithAllowlist(http.MethodHead, config.PathPublic),
			tenancy.WithAllowlist(http.MethodGet, config.PathSystemFeatures),
			tenancy.WithAllowlist(http.MethodHead, config.PathSystemFeatures),
		}, authnPreAuthAllowlist()...)...)(mux),
	)
	return handler, cleanup, nil
}

// authnAPIPath is authn's own HTTP mount point -- duplicated from
// go/authn/module.go's private apiPath constant of the same value, because
// this project's wiring, not authn's package, is what needs to name
// individual routes under it: the module owns and mounts the routes, this
// file owns which of them a caller may reach before proving who they are.
const authnAPIPath = "/api/v1/authn"

// authnPreAuthAllowlist lists every (method, path) pair under authnAPIPath
// that must work with no Principal at all -- registration, every sign-in
// entry point, token refresh, and the social authorize/callback pair (see
// go/authn/api/openapi.yaml's own path table for the exact literals, and
// go/authn/handler.go for which operations skip requirePrincipal).
//
// tenancy.WithAllowlist matches (method, path) exactly, with no prefix or
// wildcard (see its own doc comment), so the social channel's {provider}
// path parameter cannot be allowlisted generically: every channel the
// module ships gets its own two entries here, even though no channel is
// wired with real credentials in this skeleton -- otherwise enabling one
// later would silently need a code change here too.
func authnPreAuthAllowlist() []tenancy.MiddlewareOption {
	opts := []tenancy.MiddlewareOption{
		tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/register"),
		tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/password"),
		tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms/request"),
		tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms"),
		tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/token/refresh"),
	}
	for _, provider := range []string{
		authn.ProviderGoogle, authn.ProviderGitHub, authn.ProviderWeChat,
		authn.ProviderDingTalk, authn.ProviderFeishu,
	} {
		opts = append(opts,
			tenancy.WithAllowlist(http.MethodGet, authnAPIPath+"/social/"+provider+"/authorize"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/social/"+provider+"/callback"),
		)
	}
	return opts
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
// Init first (see main.go), but the indirection keeps that an
// implementation detail of main.go rather than a hidden requirement on
// buildServer's caller -- a test that calls buildServer directly can mount
// the route and assert on it without needing to care whether obs.Init has
// run yet in this process, or ever will.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	obs.MetricsHandler().ServeHTTP(w, r)
}
