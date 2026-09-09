//go:build ignore

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so dbkit.Open below has a driver to build from -- this skeleton's own
	// database always speaks SQLite, regardless of which deployment mode
	// its other infrastructure seams compose under (see buildServer's own
	// kernel-wiring comment below for the full reasoning).
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

const (
	// healthzPath is one of the routes exempted from tenant resolution: the
	// tenancy allowlist in buildServer below names the four non-authn
	// routes that must work before a Principal exists -- healthzPath,
	// metricsPath and config's two pre-auth display endpoints, each for GET
	// and HEAD. An orchestrator's liveness probe must never depend on
	// tenant resolution succeeding.
	healthzPath = "/healthz"

	// metricsPath is the Prometheus scrape endpoint, exempted from tenant
	// resolution for exactly the same reason healthzPath is: a scraper has
	// no tenant to name and must not depend on one.
	metricsPath = "/metrics"
)

// buildServer wires this project's Kernel, the modules the generator
// selected for it, their migrations, and the middleware chain into a
// single http.Handler -- the generated project's only composition point,
// mirroring examples/reference-app/cmd/server/server.go with every
// demo-specific piece removed. It returns the composed handler and a
// cleanup function that closes everything buildServer opened (the attached
// services and the underlying database connection); the caller must call
// cleanup once done with the handler.
//
// This composition wires the authn, config and rbac modules (the
// generator's --with set for this project; the README's environment table
// and this project's go.mod show which module set a differently-generated
// project carries) plus pki, which is not part of the --with set at all --
// it follows authn silently, supplying the KeySource
// authn's signing keys now live behind. Migrations register in the same order Bootstrap runs,
// so every Register-time declaration (authn's config items, permissions
// and events first, then config's own Register, and rbac's Attach-time
// snapshot last) lands before the step that freezes it. The middleware
// chain is authn.Middleware(verifier) then
// tenancy.Middleware(authn.NewPrincipalResolver()): authn first, so each
// token is verified exactly once and the tenant comes from the verified
// Principal's claims, never a Host header. authn.Middleware is optional
// auth (a bad token is a 401, an
// absent one stays anonymous), so tenancy.Middleware's fail-closed default
// is what protects every route this file does NOT allowlist: such a route
// answers 403 without a valid Principal and needs no per-route wrapping.
// The allowlist covers only the non-authn routes that must work before a
// Principal exists: healthz, metrics and config's two pre-auth display
// endpoints. authn's own pre-auth operations need NO allowlist entries at
// all -- every route under authn's API path is mounted ahead of
// tenancy.Middleware (see mountModuleRoutes), which is also what lets
// enterprise SSO's dynamic per-tenant provider names ("oidc:<tenant>",
// authn.ProviderOIDCPrefix + a tenant id) work: no allowlist could
// enumerate them, and authn's handler decides per operation which of its
// routes require a Principal (go/authn/handler.go).
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
	// process-global (authn.RegisterPIISerializer's own doc comment). The
	// cipher comes from cfg.AuthnPIICipherKey -- APP_AUTHN_PII_CIPHER_KEY
	// when set, devPIICipherKey's fallback otherwise (config.go's own doc
	// comment for both).
	piiCipher, err := dbkit.NewCipher(cfg.AuthnPIICipherKey)
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
	// comment). The cipher comes from cfg.PKILocalKeyCipherKey --
	// APP_PKI_LOCAL_KEY_CIPHER_KEY when set, devPKILocalKeyCipherKey's
	// fallback otherwise (config.go's own doc comment for both).
	pkiLocalKeyCipher, err := dbkit.NewCipher(cfg.PKILocalKeyCipherKey)
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

	// configService and rbacService are filled by their modules' Attach
	// calls below (nil until then); redisBus and redisClient are filled by
	// the conditional Redis wiring further down (nil unless cfg.RedisAddr
	// is set). cleanup closes the attached services and the injected Redis
	// client first, then the database, last; every close is attempted even
	// when an earlier one failed, and the first error wins.
	var (
		configService *config.Service
		rbacService   *rbac.Service
		redisBus      *eventbusredis.EventBus
		redisClient   *redis.Client
	)

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
		if rbacService != nil {
			keepErr(rbacService.Close())
		}
		if redisBus != nil {
			redisBus.Close()
		}
		if redisClient != nil {
			keepErr(redisClient.Close())
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
	// -- see config.go's devPKILocalKeyCipherKey doc comment for what seals
	// that stored key. pki is not part of this generator's --with selection
	// set: it follows authn silently,
	// which is why it is wired here rather than offered as its own choice.
	pkiModule := pki.NewModule(db)

	// authn's "SMS sender" seam follows the same conditional-injection shape
	// as every other seam this file wires: a configured gateway URL always
	// wins, under either deployment mode, and composes the real HTTP SMS
	// transport (pkgcore.NewHTTPSMSSender); absent that, the standalone
	// deployment mode falls back to the console transport, while the
	// distributed deployment mode is left deliberately UNWIRED -- authn.NewModule's own newOptions then
	// fails closed with authn.ErrMissingDistributedSMSSender rather than
	// this composition silently keeping a console sender nobody in a
	// distributed replica pool is reading (see config.go's smsGatewayURLEnv
	// doc comment). See buildServer's doc comment above for the
	// MembershipReader absence.
	authnOpts := []authn.Option{
		authn.WithKeySource(pkiModule.Service()),
		authn.WithBlindIndexKey(cfg.AuthnBlindIndexKey),
		authn.WithDeploymentMode(cfg.DeploymentMode),
	}
	switch {
	case cfg.SMSGatewayURL != "":
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewHTTPSMSSender(cfg.SMSGatewayURL)))
	case cfg.DeploymentMode != pkgcore.DeploymentModeDistributed:
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewConsoleSMSSender(os.Stdout)))
	}
	authnModule, err := authn.NewModule(db, authnOpts...)
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

	// rbac needs nothing from this host but a database: it declares its own
	// permissions during Register and reads EVERY module's declarations
	// once, in Attach, after Bootstrap. This skeleton mounts no protected
	// routes, so rbac's permission gates are unused today; when the owner
	// adds the first route that needs one, the gate belongs at mount time
	// in mountModuleRoutes below (its doc comment says where).
	rbacModule := rbac.NewModule(db)

	migrationRegistry := dbkit.NewMigrationRegistry()
	for _, m := range []pkgcore.Module{pkiModule, authnModule, configModule, rbacModule} {
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
	// mode and the implementation composition are orthogonal axes, and the validation refuses a
	// composition the declared mode cannot run, naming the seam, the
	// implementation and the missing capability.
	//
	// Kernel.Bootstrap always resolves and validates all four registered
	// seams (eventbus, kv, mailer, objectstore) regardless of which modules
	// this selection wires -- a distributed boot still fails closed on
	// whichever seam the Preset would otherwise resolve to an in-process
	// default, since every resolved seam must satisfy
	// DeploymentModeDistributed's RequiredCapabilities (MultiReplicaSafe).
	// The four conditional injections below follow the exact shape
	// examples/reference-app/cmd/server/server.go's own kernel-wiring
	// comment documents at length: an unset env var leaves that seam on the
	// Preset's in-process default, so `go run ./cmd/server` stays
	// byte-for-byte unaffected, and a configured one injects a real
	// implementation declaring the capability bits that implementation
	// genuinely carries. One Redis client backs both "eventbus" and "kv" --
	// see redisAddrEnv's own doc comment in config.go for why wiring only
	// one of the two can never let a distributed composition pass
	// Bootstrap.
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(cfg.DeploymentMode)}
	if cfg.RedisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		redisBus = eventbusredis.NewEventBus(redisClient)
		kernelOptions = append(kernelOptions,
			pkgcore.WithEventBus(redisBus, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
		kernelOptions = append(kernelOptions,
			pkgcore.WithKVStore(kvredis.NewKVStore(redisClient), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.S3Endpoint != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(objectstores3.NewObjectStore(objectstores3.Config{
				Endpoint:  cfg.S3Endpoint,
				Bucket:    cfg.S3Bucket,
				AccessKey: cfg.S3AccessKey,
				SecretKey: cfg.S3SecretKey,
				Region:    cfg.S3Region,
				UseSSL:    cfg.S3UseSSL,
			}), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.SMTPHost != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithMailer(pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
				Host:     cfg.SMTPHost,
				Port:     cfg.SMTPPort,
				Username: cfg.SMTPUsername,
				Password: cfg.SMTPPassword,
			}), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, pkiModule, authnModule, configModule, rbacModule)
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
	// rbac's Attach freezes the snapshot of every permission every module
	// declared -- taken any earlier it would be missing whatever registered
	// after it, and a permission missing from the catalog cannot be granted
	// at all.
	rbacService, err = rbacModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: attach the rbac module: %w", err)
	}

	// Route mounting: every route reg's modules mounted goes to one of
	// two muxes, decided in mountModuleRoutes by the route's path --
	// authn's own subtree (authnAPIPath) to authnMux, everything else to
	// moduleMux -- never by a per-route enumeration.
	moduleMux := http.NewServeMux()
	moduleMux.HandleFunc(healthzPath, healthzHandler)
	moduleMux.HandleFunc(metricsPath, metricsHandler)
	authnMux := http.NewServeMux()
	if err := mountModuleRoutes(authnMux, moduleMux, reg); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: mount module routes: %w", err)
	}

	// The middleware chain: authn first, then tenancy (see buildServer's
	// doc comment above for the order reasoning). What tenancy.Middleware
	// wraps is moduleMux only: topMux dispatches authn's own subtree
	// (authnAPIPath) straight from authn.Middleware's output, exempt from
	// tenant resolution BY STRUCTURE rather than by allowlist entry.
	// Everything under authnAPIPath must work before a Principal exists --
	// registration, every sign-in entry point, token refresh and the
	// social authorize/callback pair -- and authn's own handler decides,
	// operation by operation, which of its routes require a Principal (see
	// go/authn/handler.go). Enterprise SSO makes the structural exemption
	// the only correct one: its provider value is the dynamic per-tenant
	// name "oidc:<tenant>" (authn.ProviderOIDCPrefix + a tenant id), which
	// no fixed allowlist could enumerate -- so any allowlist-shaped wiring
	// would refuse an SSO-configured tenant's login-start request with 403
	// tenancy.tenant_unresolved before authn's own OIDC logic ever saw it.
	// The tenancy allowlist below therefore names only the NON-authn
	// routes that must work with no Principal: healthz, metrics and
	// config's two pre-auth display endpoints.
	topMux := http.NewServeMux()
	topMux.Handle(authnAPIPath, authnMux)
	topMux.Handle(authnAPIPath+"/", authnMux)
	topMux.Handle("/", tenancy.Middleware(authn.NewPrincipalResolver(),
		tenancy.WithAllowlist(http.MethodGet, healthzPath),
		tenancy.WithAllowlist(http.MethodHead, healthzPath),
		tenancy.WithAllowlist(http.MethodGet, metricsPath),
		tenancy.WithAllowlist(http.MethodHead, metricsPath),
		tenancy.WithAllowlist(http.MethodGet, config.PathPublic),
		tenancy.WithAllowlist(http.MethodHead, config.PathPublic),
		tenancy.WithAllowlist(http.MethodGet, config.PathSystemFeatures),
		tenancy.WithAllowlist(http.MethodHead, config.PathSystemFeatures),
	)(moduleMux))
	handler := authn.Middleware(authnModule.Service().Verifier())(topMux)
	return handler, cleanup, nil
}

// authnAPIPath is authn's own HTTP mount point -- duplicated from
// go/authn/module.go's private apiPath constant of the same value, because
// this project's wiring, not authn's package, is what needs to name
// individual routes under it: buildServer mounts the whole subtree ahead
// of tenancy.Middleware (see buildServer's doc comment for why every route
// under it must work before a Principal exists, enterprise SSO's dynamic
// per-tenant provider names included), and mountModuleRoutes uses the
// prefix to decide which mux a mounted route belongs on. No per-route
// pre-auth allowlist exists anymore: the structural exemption replaced
// the fixed (method, path) enumeration that could never name an
// "oidc:<tenant>" provider.
const authnAPIPath = "/api/v1/authn"

// mountModuleRoutes copies every route reg's modules mounted onto one of
// the two muxes, decided by path prefix: a route under authnAPIPath --
// authn's own subtree, which authn mounts as a single handler at its API
// path -- goes to authnMux, mounted by buildServer on topMux ahead of
// tenancy.Middleware; every other route goes to protectedMux, the
// tenancy-wrapped one. Routing by the module's own mount path rather than
// by an enumerated route list is what keeps the exemption structural: a
// future route under authn's API path is exempt the day it mounts, and
// nothing outside that subtree ever is. (authn owns the whole prefix: no
// other module mounts under /api/v1/authn.)
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

// rbac's permission gate belongs at this same mount point, wrapped around
// a route's Handler before it reaches the mux (RequirePermissionFunc's own
// doc comment shows the shape): a route that reaches the mux ungated is
// served ungated. This skeleton wires no protected routes, so there is no
// gate to apply yet -- when the owner adds the first one, it goes here.

func mountModuleRoutes(authnMux, protectedMux *http.ServeMux, reg *pkgcore.Registry) error {
	for _, route := range reg.Routes.Routes() {
		target := protectedMux
		if strings.HasPrefix(route.Path, authnAPIPath) {
			target = authnMux
		}
		target.Handle(route.Path, route.Handler)
		if !strings.HasSuffix(route.Path, "/") {
			target.Handle(route.Path+"/", route.Handler)
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
