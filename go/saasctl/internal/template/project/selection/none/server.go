//go:build ignore

package main

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the database the engine opens has a driver to build from -- this
	// skeleton's own database always speaks SQLite, regardless of which
	// deployment mode its other infrastructure seams compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/tenancy"
)

// serverBuild carries the assembly state this selection's callbacks hand each
// other: the resolved configuration and the loader target the engine fills,
// the module the WithModules stage constructs, the service the post-bootstrap
// attach derives from it, and the Redis resources this host built and
// therefore owns. The declared key materials are not carried here: the
// assembly resolves them off the module components' declarations and hands
// them to the callbacks as the published bootstrap material.
type serverBuild struct {
	cfg        serverConfig
	hostConfig hostConfig

	redisBus    *eventbusredis.EventBus
	redisClient *redis.Client

	configModule *config.Module

	configService *config.Service

	reg *pkgcore.Registry
}

// runServer assembles this project through the application engine and serves
// it until the process is signalled. The engine's Run owns the whole
// lifecycle -- signal handling, observability init, the HTTP serve and the
// ordered drain -- and this file's callbacks are the host's seats inside the
// one fixed assembly order every speed application shares.
//
// This composition wires ONLY the config module (the generator's --with set
// for this project) -- the module every composition requires, whose two
// pre-auth display endpoints render the login page a sign-in flow
// presupposes. It is the empty selection: no authn, no org, no rbac.
//
// There is deliberately NO middleware chain here, and so no HTTPSpec.Compose:
// a middleware chain exists to turn a verified caller into tenant context,
// and this composition has no authn module and therefore no verification
// step and no Principal -- authn.Middleware's verifier has nothing to
// verify, and tenancy.Middleware's resolver has no claim to resolve a tenant
// from. Wrapping the mux in tenancy.Middleware anyway, with a resolver that
// can never succeed, would fail closed every route below it for no gain --
// fail-closed protection is worth exactly what it protects, and none of the
// routes this composition serves (healthz, metrics, config's two pre-auth
// display endpoints) needs a tenant to answer correctly: each is pre-auth by
// its owning module's design, and config's endpoints read the platform
// default tier when the host map matches nothing (their own contract,
// below). Every other route in this process is whatever the owner mounts
// later -- and the owner's first step toward anything tenant-shaped is
// regenerating with --with authn (or hand-wiring the chain from an
// authn-wiring selection's server.go), not hand-rolling a weaker chain here.
//
// Host seams deliberately left unwired, each failing closed per the owning
// module's contract and each the owner's first task: the config resolver's
// host map, which deliberately matches NOTHING so the display endpoints
// serve platform defaults to every caller, never an error (the login-page
// rule; a static unauthenticated Host map would violate tenancy's own
// Resolver contract, go/tenancy/resolver.go).
func runServer(baseCtx context.Context, cfg serverConfig, hc hostConfig) error {
	b := &serverBuild{cfg: cfg, hostConfig: hc}
	// The Redis client is this host's own resource: the engine's ordered
	// shutdown stops every seam it resolved but never closes a value the
	// host built, so the host closes it here, after Run has drained
	// everything that could still be using it (a failed assembly included:
	// Run rolls back before returning).
	defer b.closeRedis()
	return speedapp.Run(baseCtx, b.options()...)
}

// closeRedis stops the Redis-backed event bus and releases the client it was
// built over, the reverse of their construction. Both may be nil -- the
// default standalone composition runs no Redis at all.
func (b *serverBuild) closeRedis() {
	if b.redisBus != nil {
		b.redisBus.Close()
	}
	if b.redisClient != nil {
		_ = b.redisClient.Close()
	}
}

// options maps the resolved configuration onto the engine's option set: the
// host's configuration target and the loader options the declared keys
// resolve under, the database, the module set,
// the kernel's seam composition, the observability spec, the HTTP face (with
// no protected-face composition -- see runServer's doc comment), and the
// host's attach hook. Everything host-specific the engine cannot know is
// named here; nothing is defaulted on the host's behalf.
func (b *serverBuild) options() []speedapp.Option {
	return []speedapp.Option{
		// The host target carries this project's own keys. The declared key
		// materials come off the module components' declarations and resolve
		// on this same loader chain: the environment prefix derives each
		// key's variable from its declared path, and bootstrapDevDefaults is
		// the table that stands when no variable does. This project installs
		// no root key, so the derivation tier stays unconfigured.
		speedapp.WithConfig(
			speedapp.ConfigSpec{Host: &b.hostConfig},
			speedapp.ConfigEnvPrefix(envPrefix),
			speedapp.ConfigDevDefaults(bootstrapDevDefaults()),
		),
		speedapp.WithDatabase(speedapp.DatabaseSpec{
			Dialect: dbkit.DialectSQLite,
			DSN:     b.cfg.SQLitePath,
		}),
		speedapp.WithModules(b.constructModules),
		speedapp.WithKernelOptions(b.kernelOptions()...),
		speedapp.WithObservability(speedapp.ObservabilitySpec{
			ServiceName:  "__APP_NAME__",
			OTLPEndpoint: b.cfg.OTLPEndpoint,
		}),
		speedapp.WithHTTP(speedapp.HTTPSpec{Addr: ":" + b.cfg.Port}),
		speedapp.WithHooks(speedapp.Hooks{PostBootstrap: b.postBootstrap}),
	}
}

// constructModules is the engine's module-construction stage: the module
// this selection wires, built explicitly here -- the engine never knows a
// module type. The config module's cipher is the engine's platform cipher,
// the one built from config.cipher_key.
func (b *serverBuild) constructModules(_ context.Context, deps speedapp.ModuleDeps) ([]pkgcore.Module, error) {
	// The config module is required in every generated composition (its two
	// pre-auth display endpoints render the login page a sign-in flow
	// presupposes). Its resolver's lookup deliberately matches NOTHING, so
	// the display endpoints serve platform defaults to every caller until
	// the owner wires a real host-to-tenant source; an empty default tenant
	// maps unmatched hosts onto the "platform defaults" tier rather than an
	// error -- exactly the login-page rule (see runServer's doc comment).
	b.configModule = config.NewModule(deps.DB,
		config.WithCipher(deps.Cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				return "", false
			},
			"",
		)),
	)

	return []pkgcore.Module{b.configModule}, nil
}

// postBootstrap is the engine's attach stage, after the kernel bootstrapped
// the module set and the bootstrap-key binding was verified: the typed
// Attach call config requires exactly once after Bootstrap.
// configModule.Attach freezes the schema snapshot of every config item and
// feature flag the modules declared during Register; taken any earlier it
// would be missing whatever registered after it.
func (b *serverBuild) postBootstrap(_ context.Context, a *speedapp.Application) error {
	b.reg = a.Registry()
	var err error
	if b.configService, err = b.configModule.Attach(b.reg); err != nil {
		return fmt.Errorf("__APP_NAME__: attach the config module: %w", err)
	}
	return nil
}

// kernelOptions assembles the kernel options the engine bootstraps with: the
// deployment mode and the conditional seam injections. WithDeploymentMode
// declares the topology the assembled composition is validated against; it
// never selects an implementation (the deployment mode and the
// implementation composition are orthogonal axes, and the validation refuses
// a composition the declared mode cannot run, naming the seam, the
// implementation and the missing capability).
//
// Kernel.Bootstrap always resolves and validates all four registered seams
// (eventbus, kv, mailer, objectstore) regardless of which modules this
// selection wires -- this composition uses none of them directly, but a
// distributed boot still fails closed on whichever seam the Preset would
// otherwise resolve to an in-process default, since every resolved seam must
// satisfy DeploymentModeDistributed's RequiredCapabilities (MultiReplicaSafe).
// The conditional injections below are the same shape at every seam: an
// unset env var leaves that seam on the Preset's in-process default, so
// `go run ./cmd/server` stays unaffected, and a configured one injects a
// real implementation declaring the capability bits that implementation
// genuinely carries. One Redis client backs both "eventbus" and "kv" -- see
// config.go's RedisAddr field doc comment for why wiring only one of the two
// can never let a distributed composition pass Bootstrap; the client is this
// host's own resource and runServer closes it (the S3 store and the SMTP
// mailer are injected values the kernel's shutdown does close, through its
// own registered closers).
func (b *serverBuild) kernelOptions() []pkgcore.KernelOption {
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(b.cfg.DeploymentMode)}
	if b.cfg.RedisAddr != "" {
		b.redisClient = redis.NewClient(&redis.Options{Addr: b.cfg.RedisAddr})
		b.redisBus = eventbusredis.NewEventBus(b.redisClient)
		kernelOptions = append(kernelOptions,
			pkgcore.WithEventBus(b.redisBus, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
		kernelOptions = append(kernelOptions,
			pkgcore.WithKVStore(kvredis.NewKVStore(b.redisClient), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if b.cfg.S3Endpoint != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(objectstores3.NewObjectStore(objectstores3.Config{
				Endpoint:     b.cfg.S3Endpoint,
				Bucket:       b.cfg.S3Bucket,
				AccessKey:    b.cfg.S3AccessKey,
				SecretKey:    b.cfg.S3SecretKey,
				Region:       b.cfg.S3Region,
				UseSSL:       b.cfg.S3UseSSL,
				BucketLookup: b.cfg.S3BucketLookup,
			}), pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if b.cfg.SMTPHost != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithMailer(pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
				Host:     b.cfg.SMTPHost,
				Port:     b.cfg.SMTPPort,
				Username: b.cfg.SMTPUsername,
				Password: b.cfg.SMTPPassword,
			}), pkgcore.MultiReplicaSafe|pkgcore.Stateless))
	}
	return kernelOptions
}
