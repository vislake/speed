//go:build ignore

package main

import (
	"context"
	"fmt"
	"net/http"

	"github.com/redis/go-redis/v9"

	speedapp "github.com/vislake/speed/go/app"
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
	"github.com/vislake/speed/go/tenancy"
)

// The liveness routes' paths and handlers are observability's
// (obs.MountLiveness), the module-route mounting rule is pkgcore's
// (pkgcore.MountRoutes), and the pre-auth allowlist set is the platform
// composition toolkit's (github.com/vislake/speed/go/app -- the same
// module the reference app composes through): this file names them
// through those packages rather than restating any of them, so a
// generated project and the reference app keep composing the same host
// surface.

// buildServer wires this project's Kernel, the modules the generator
// selected for it, their migrations, and the handler into a single
// http.Handler -- the generated project's only composition point,
// mirroring examples/reference-app/internal/app/server.go with every
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
	// then); redisBus and redisClient are filled by the conditional Redis
	// wiring further down (nil unless cfg.RedisAddr is set). cleanup closes
	// the attached service and the injected Redis client first, then the
	// database, last; every close is attempted even when an earlier one
	// failed, and the first error wins.
	var (
		configService *config.Service
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
	// mode and the implementation composition are orthogonal axes, and the validation refuses a
	// composition the declared mode cannot run, naming the seam, the
	// implementation and the missing capability.
	//
	// Kernel.Bootstrap always resolves and validates all four registered
	// seams (eventbus, kv, mailer, objectstore) regardless of which modules
	// this selection wires -- this composition uses none of them directly,
	// but a distributed boot still fails closed on whichever seam the
	// Preset would otherwise resolve to an in-process default, since every
	// resolved seam must satisfy DeploymentModeDistributed's
	// RequiredCapabilities (MultiReplicaSafe). The four conditional
	// injections below follow the exact shape
	// examples/reference-app/internal/app/server.go's own kernel-wiring
	// comment documents at length: an unset env var leaves that seam on the
	// Preset's in-process default, so `go run ./cmd/server` stays
	// byte-for-byte unaffected, and a configured one injects a real
	// implementation declaring the capability bits that implementation
	// genuinely carries. One Redis client backs both "eventbus" and "kv" --
	// see config.go's RedisAddr field doc comment for why wiring only
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
				Endpoint:     cfg.S3Endpoint,
				Bucket:       cfg.S3Bucket,
				AccessKey:    cfg.S3AccessKey,
				SecretKey:    cfg.S3SecretKey,
				Region:       cfg.S3Region,
				UseSSL:       cfg.S3UseSSL,
				BucketLookup: cfg.S3BucketLookup,
			}), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.SMTPHost != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithMailer(pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
				Host:     cfg.SMTPHost,
				Port:     cfg.SMTPPort,
				Username: cfg.SMTPUsername,
				Password: cfg.SMTPPassword,
			}), pkgcore.MultiReplicaSafe|pkgcore.Stateless))
	}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, configModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: bootstrap kernel: %w", err)
	}

	// Attach runs strictly after Bootstrap, exactly once -- the contract
	// pkgcore.Kernel.Bootstrap's "Post-Bootstrap module steps" section
	// states: what it freezes is the schema snapshot of every config item
	// and feature flag the modules declared during Register (config's own
	// Attach doc comment).
	configService, err = configModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: attach the config module: %w", err)
	}

	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	if err := mountModuleRoutes(mux, reg); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("__APP_NAME__: mount module routes: %w", err)
	}
	// Obs route-label seeding: the shared kernel registers this host's
	// real route table (its two liveness paths plus every module route
	// just mounted) before obs.Middleware is constructed; see
	// speedapp.RegisterMountedRoutes' doc comment.
	speedapp.RegisterMountedRoutes(reg)

	// The bare mux IS the handler: no authn module means no verifier and no
	// Principal, so there is no middleware chain to wrap it in -- see
	// buildServer's doc comment above for the full reasoning.
	return mux, cleanup, nil
}

// mountModuleRoutes copies every route reg's modules mounted onto mux.
//
// Each route mounts through pkgcore.MountRoutes, whose own doc comment
// carries the exact-plus-subtree registration rule and the reasoning
// behind it.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry) error {
	for _, route := range reg.Routes.Routes() {
		pkgcore.MountRoutes(mux, route)
	}
	return nil
}
