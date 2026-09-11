//go:build ignore

package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"

	"gorm.io/gorm"

	speedapp "github.com/vislake/speed/go/app"
	speedchain "github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite,
	// the dialect the database component this composition selects opens --
	// this skeleton's own database always speaks SQLite, regardless of which
	// deployment mode its other infrastructure seams compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	// Blank-imported for their init() side effects: each registers the
	// distributed component the composition selects when the matching
	// variable is set ("eventbus.redis" and "kv.redis" for APP_REDIS_ADDR).
	// Without the import, a configured boot would fail the assembly with
	// ErrUnknownComponent -- the database/sql driver trade every registered
	// built-in makes.
	_ "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	_ "github.com/vislake/speed/go/pkgcore/kv/redis"
	// Imported (and thereby registered) for its init() side effect and read
	// by the composition below: the "objectstore.s3" component the APP_S3_*
	// group selects, and the addressing-style enum the resolved serverConfig
	// carries.
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"

	authnlocales "github.com/vislake/speed/go/authn/locales"
	"github.com/vislake/speed/go/pkgcore/i18n"
	pkilocales "github.com/vislake/speed/go/pki/locales"
	"github.com/vislake/speed/go/tenancy"
)

// hostComponentPrefix names every component this file contributes, keeping
// the host's own copies, providers and steps apart from the components the
// module packages register for themselves.
const hostComponentPrefix = "__APP_NAME__."

// The declared bootstrap key paths this file's wiring reads material for.
// Each is the declaring module's own key path, spelled as its declaration
// carries it: the assembly's loader resolves the declaration, and this file
// reads the result by the same path.
const (
	// configCipherKeyPath is the path the platform cipher's material is
	// declared under (go/config's component).
	configCipherKeyPath = "config.cipher_key"
	// authnBlindIndexKeyPath is the path authn's blind-index HMAC key is
	// declared under (go/authn's component).
	authnBlindIndexKeyPath = "authn.blind_index_key"
	// pkiLocalKeyCipherKeyPath is the path the cipher sealing pki's
	// local-signer private-key column is declared under (go/pki's component).
	pkiLocalKeyCipherKeyPath = "pki.local_key_cipher_key"
)

// overriddenModuleLocales lists the locale resources of the modules whose
// descriptors this host overrides. A locale file's message ids are prefixed
// with the MODULE name, and the assembly's locale validation requires the
// component name to be that prefix -- an override component carries a host
// name, so it must not carry the module's locale resources. They merge here
// instead, under the module's own name, through hostCatalog below.
var overriddenModuleLocales = []struct {
	name string
	fs   embed.FS
}{
	{"authn", authnlocales.FS},
	{"pki", pkilocales.FS},
}

// serverBuild carries the assembly state this selection's components hand
// each other: the resolved configuration and the loader target the engine
// fills, the modules the host's steps and its composed face read products
// from, and the services the post-bootstrap attach derives from them.
type serverBuild struct {
	cfg        serverConfig
	hostConfig hostConfig

	configModule *config.Module
	pkiModule    *pki.Module
	authnModule  *authn.Module

	configService *config.Service
}

// runServer assembles this project through the component assembly and serves
// it until the process is signalled: the signal-derived context is what the
// serve waits on, and the two-phase shutdown -- the Stop notification, then
// the drain and release -- runs once it is done.
//
// This composition wires the authn and config modules (the
// generator's --with set for this project). Each module is selected through
// the component assembly -- the modules register their own descriptors in
// their init() functions, and composition() names the ones this project
// selects, with the host's own override for the modules whose construction
// or declaration timing needs this file's wiring. The middleware chain is
// chain.Standard's derivation: authn first, so each token is verified
// exactly once and the tenant comes from the verified Principal's claims,
// never a Host header, then tenancy under the chain's pre-auth allowlist --
// authn.Middleware is optional auth (a bad token is a 401, an absent one
// stays anonymous), so tenancy's fail-closed default is what makes every
// route NOT on the allowlist require a valid Principal with no per-route
// wrapping. The allowlist covers only the non-authn routes that must work
// before a Principal exists: healthz, metrics and config's two pre-auth
// display endpoints. authn's own pre-auth operations need NO allowlist
// entries at all -- Standard splits the whole subtree under authn's API path
// onto its own branch ahead of tenancy, which is also what lets enterprise
// SSO's dynamic per-tenant provider names ("oidc:<tenant>") work: no
// allowlist could enumerate them, and authn's handler decides per operation
// which of its routes require a Principal (go/authn/handler.go). Every
//
// Host seams deliberately left unwired, each failing closed per the owning
// module's contract and each the owner's first task: authn's
// MembershipReader (session refresh re-verifies the session's current
// tenant against it, and an absent reader refuses rather than defaulting
// -- a freshly signed-in user has no tenant until the owner builds one);
// and the config
// resolver's host map, which deliberately matches NOTHING so unmatched
// hosts read platform defaults, never an error (the login-page rule; a
// static unauthenticated Host map would violate tenancy's own Resolver
// contract, go/tenancy/resolver.go).
func runServer(baseCtx context.Context, cfg serverConfig, hc hostConfig) error {
	// The signal-derived context is the assembly's own lifecycle, and
	// baseCtx stays the request base context the composed face hands its
	// listener: a shutdown signal must never cancel in-flight requests
	// ahead of the graceful drain (net/http's Server.BaseContext contract).
	ctx, stop := signal.NotifyContext(baseCtx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b := &serverBuild{cfg: cfg, hostConfig: hc}
	reg, err := b.assemble(ctx, baseCtx)
	if err != nil {
		return err
	}
	<-ctx.Done()
	if err := speedapp.Shutdown(context.WithoutCancel(ctx), reg); err != nil {
		return err
	}
	obs.FromContext(ctx).Info("server stopped cleanly")
	return nil
}

// assemble builds the host's component set over a fresh registry and drives
// the whole assembly: the host's own components (the providers, the
// overrides and the assembly steps) register first, the loader resolves the
// configuration and the composition, and the registry walks Prepare through
// Start. ctx is the assembly's lifecycle context; baseCtx is the context
// served requests inherit. A failure needs no host-side teardown: the
// assembly's own rollback closes every constructed component in reverse
// order, exactly once, before the error returns.
func (b *serverBuild) assemble(ctx context.Context, baseCtx context.Context) (*pkgcore.ComponentRegistry, error) {
	reg := pkgcore.NewComponentRegistry()
	components, err := b.hostComponents(reg, baseCtx)
	if err != nil {
		return nil, err
	}
	for _, c := range components {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	spec := speedapp.LoadSpec{
		Host:      &b.hostConfig,
		Options:   b.loaderOptions(),
		Overrides: &speedapp.CompositionOverrides{Config: b.composition()},
	}
	if err := speedapp.Assemble(ctx, reg, spec); err != nil {
		return nil, err
	}
	return reg, nil
}

// loaderOptions returns the loader options the assembly's configuration
// resolution runs with: the environment prefix everything of this project's
// bootstrap surface is spelled under and this project's documented
// development defaults as the lowest-priority table. The declared key
// materials themselves come from the selected modules' components -- this
// project declares none of its own, and installs no root key, so the
// derivation tier stays unconfigured.
func (b *serverBuild) loaderOptions() []speedapp.ConfigOption {
	return []speedapp.ConfigOption{
		speedapp.ConfigEnvPrefix(envPrefix),
		speedapp.ConfigDevDefaults(bootstrapDevDefaults()),
	}
}

// composition returns this project's code-override layer: the composition
// configuration's highest source, carrying the components this project
// selects -- every module, the infrastructure implementations the resolved
// serverConfig names, and this file's own providers, overrides and steps --
// and deselecting the registered implementations the host's own copies stand
// in for. The block's key order is the plan order for tied components, so
// the infrastructure leads (the mode's capability validation walks the
// selection in this order, and the module rows keep the order their
// migrations apply in), and the host's assembly steps come last.
func (b *serverBuild) composition() pkgcore.ComponentConfig {
	components := pkgcore.ComponentConfig{}.
		With("observability", false).
		With(hostComponentPrefix+"observability", pkgcore.ComponentConfig{}.With("service_name", "__APP_NAME__")).
		With("db.sqlite", false).
		With(hostComponentPrefix+"db", pkgcore.ComponentConfig{}.With("dsn", b.cfg.SQLitePath)).
		With("eventbus.memory", nil).
		With("kv.memory", nil).
		With("mailer.console", nil).
		With("objectstore.local", nil).
		With("sms.console", nil).
		With(hostComponentPrefix+"crypto", nil).
		With(hostComponentPrefix+"tenancy-resolver", nil).
		// The modules, in the order their migrations apply: the host
		// overrides first where a module needs one, each paired with the
		// deselection of the module package's own descriptor.
		With("pki", false).
		With(hostComponentPrefix+"pki", nil).
		With("signer.local", false).
		With(hostComponentPrefix+"signer.local", nil).
		With("authn", false).
		With(hostComponentPrefix+"authn", nil).
		With("config", false).
		With(hostComponentPrefix+"config", nil).
		// The host's own assembly steps come last: the post-bootstrap step
		// runs after every module's declaration turn (the module components
		// are listed above), and the application component after it, since
		// the face it composes reads the services that step publishes.
		With(hostComponentPrefix+"post_bootstrap", nil).
		With(hostComponentPrefix+"app", nil)

	// The infrastructure seams follow the resolved serverConfig, one
	// selected implementation per seam: an unset variable leaves that seam
	// on its in-process default, so a plain `go run ./cmd/server` needs
	// nothing else running. The Redis client composes both "eventbus" and
	// "kv" -- wiring only one of the two can never let a distributed
	// composition pass the mode's capability validation.
	if b.cfg.RedisAddr != "" {
		components = components.
			With("eventbus.memory", false).
			With("eventbus.redis", pkgcore.ComponentConfig{}.With("addr", b.cfg.RedisAddr)).
			With("kv.memory", false).
			With("kv.redis", pkgcore.ComponentConfig{}.With("addr", b.cfg.RedisAddr))
	}
	if b.cfg.S3Endpoint != "" {
		components = components.
			With("objectstore.local", false).
			With("objectstore.s3", pkgcore.ComponentConfig{}.
				With("endpoint", b.cfg.S3Endpoint).
				With("bucket", b.cfg.S3Bucket).
				With("access_key", b.cfg.S3AccessKey).
				With("secret_key", b.cfg.S3SecretKey).
				With("region", b.cfg.S3Region).
				With("use_ssl", b.cfg.S3UseSSL).
				With("bucket_lookup", s3BucketLookupText(b.cfg.S3BucketLookup)))
	}
	if b.cfg.SMTPHost != "" {
		components = components.
			With("mailer.console", false).
			With("mailer.smtp", pkgcore.ComponentConfig{}.
				With("host", b.cfg.SMTPHost).
				With("port", b.cfg.SMTPPort).
				With("username", b.cfg.SMTPUsername).
				With("password", b.cfg.SMTPPassword))
	}
	// The "SMS sender" seam: a configured gateway URL always wins under
	// either deployment mode; absent that the console transport stays
	// selected, and a distributed boot with neither is refused by authn's
	// own construction (ErrMissingDistributedSMSSender) rather than keeping
	// a transport nobody in a replica pool reads.
	if b.cfg.SMSGatewayURL != "" {
		components = components.
			With("sms.console", false).
			With("sms.http", pkgcore.ComponentConfig{}.With("endpoint", b.cfg.SMSGatewayURL))
	}
	// The observability block carries this project's telemetry
	// configuration (go/app's observability component schema): the service
	// name, and the OTLP collector target when APP_OTLP_ENDPOINT is set.
	if b.cfg.OTLPEndpoint != "" {
		components = components.
			With(hostComponentPrefix+"observability", pkgcore.ComponentConfig{}.
				With("service_name", "__APP_NAME__").
				With("otlp_endpoint", b.cfg.OTLPEndpoint))
	}

	return pkgcore.ComponentConfig{}.
		With("deployment", string(b.cfg.DeploymentMode)).
		With("strict", true).
		With("components", components)
}

// s3BucketLookupText maps the addressing style serverConfig parsed
// APP_S3_BUCKET_LOOKUP into back to the component schema's text spelling:
// the value crosses into the composition as text because the component
// parses and refuses it itself, and the auto default passes as the empty
// string -- the store's own endpoint-derived default.
func s3BucketLookupText(lookup objectstores3.BucketLookupType) string {
	switch lookup {
	case objectstores3.BucketLookupPath:
		return "path"
	case objectstores3.BucketLookupVirtualHost:
		return "virtual_host"
	default:
		return ""
	}
}

// hostComponents returns the components this host contributes, in an order
// whose independent components prepare and plan in the order this list
// states: the provider components (their products are what the module
// descriptors read while constructing), the override components (the copies
// of the selected modules' descriptors carrying this project's own
// construction), and the assembly steps -- the post-bootstrap step, then the
// application component that owns the HTTP face. reg is the assembly
// instance the override components copy their modules' descriptors from.
func (b *serverBuild) hostComponents(reg *pkgcore.ComponentRegistry, baseCtx context.Context) ([]pkgcore.Component, error) {
	components := []pkgcore.Component{b.cryptoComponent()}

	for _, build := range []func(*pkgcore.ComponentRegistry) (pkgcore.Component, error){
		b.dbComponent,
		b.observabilityComponent,
	} {
		c, err := build(reg)
		if err != nil {
			return nil, err
		}
		components = append(components, c)
	}

	// The modules whose descriptors declare no capability: this host selects
	// its own copies, carrying the replica-safe declaration and the package's
	// own construction untouched.
	for _, name := range []string{"pki", "signer.local"} {
		c, err := capabilityComponent(reg, name)
		if err != nil {
			return nil, err
		}
		components = append(components, c)
	}

	for _, build := range []func(*pkgcore.ComponentRegistry) (pkgcore.Component, error){
		b.authnComponent,
		b.configComponent,
	} {
		c, err := build(reg)
		if err != nil {
			return nil, err
		}
		components = append(components, c)
	}

	components = append(components,
		b.tenancyResolverComponent(),
		b.postBootstrapComponent(),
		appComponent(b, baseCtx),
	)

	// Every component this file contributes is replica-safe state: each
	// holds nothing a second replica would silently split (the providers
	// adapt shared-database services, the copies carry the package's own
	// construction, and what the steps publish runs per replica over the
	// shared database), so the declaration is made once here and a
	// distributed composition assembles.
	for i := range components {
		components[i] = replicaSafeModule(components[i])
	}
	return components, nil
}

// replicaSafeModule declares the MultiReplicaSafe bit on a host component:
// the module's state is its rows in the deployment's shared database and the
// shared event bus, so several replicas may run it at once.
func replicaSafeModule(c pkgcore.Component) pkgcore.Component {
	c.Capabilities |= pkgcore.MultiReplicaSafe
	return c
}

// registeredComponent returns the component registered under name.
func registeredComponent(reg *pkgcore.ComponentRegistry, name string) (pkgcore.Component, bool) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == name {
			return c, true
		}
	}
	return pkgcore.Component{}, false
}

// overrideComponent returns the registered descriptor named base carrying
// this host's own construction: everything but the name and the New callback
// is the package's own declaration -- the same Requires, Provides, assets,
// capabilities and lifecycle callbacks the component ships. moduleName is
// the module the descriptor implements and the suffix of the override's own
// name; the extra requirements are this host's own construction
// dependencies, appended to the descriptor's.
func overrideComponent(reg *pkgcore.ComponentRegistry, base, moduleName string, construct func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error), extra ...pkgcore.Requirement) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, base)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("__APP_NAME__: component %q has no registered descriptor to override", base)
	}
	c := descriptor
	c.Name = hostComponentPrefix + moduleName
	c.Locales = embed.FS{}
	c.New = construct
	c.Requires = append(append([]pkgcore.Requirement(nil), descriptor.Requires...), extra...)
	return c, nil
}

// capabilityComponent returns the registered descriptor named base carrying
// the replica-safe declaration and nothing else: the same Requires, Provides,
// assets and lifecycle callbacks, under the host's own name -- what a
// distributed composition needs of a module whose descriptor declares no
// capability.
func capabilityComponent(reg *pkgcore.ComponentRegistry, name string) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, name)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("__APP_NAME__: component %q has no registered descriptor", name)
	}
	c := replicaSafeModule(descriptor)
	c.Name = hostComponentPrefix + name
	c.Locales = embed.FS{}
	return c, nil
}

// declaredMaterial reads the []byte material a declared bootstrap key
// resolved to from the assembly's published material source, naming the
// declared path when the assembly carries no value for it.
func declaredMaterial(reg *pkgcore.ComponentRegistry, keyPath string) ([]byte, error) {
	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		return nil, err
	}
	value, ok := material.Material(keyPath)
	if !ok {
		return nil, fmt.Errorf("__APP_NAME__: the assembly resolved no material for the declared bootstrap key %q", keyPath)
	}
	return value, nil
}

// cryptoComponent returns the component that builds and registers the
// serializers this project's modules read their encrypted columns through,
// before the connection that parses their models exists -- which is why its
// work runs in the Prepare stage, and why the cipher it builds is provided
// as a product: the config module's Sensitive items are sealed with it.
// authn's own PII column needs no registration here: its descriptor performs
// it in its own Prepare callback.
func (b *serverBuild) cryptoComponent() pkgcore.Component {
	var platformCipher *dbkit.Cipher
	return pkgcore.Component{
		Name:     hostComponentPrefix + "crypto",
		Provides: []any{(*dbkit.Cipher)(nil)},
		Prepare: func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
			cipherKey, err := declaredMaterial(reg, configCipherKeyPath)
			if err != nil {
				return err
			}
			cipher, err := dbkit.NewCipher(cipherKey)
			if err != nil {
				return fmt.Errorf("__APP_NAME__: build the platform cipher: %w", err)
			}

			// go/pki's LocalSigner private-key column, over the
			// pki.local_key_cipher_key material -- a separate secret from
			// the platform cipher above, because an AES key must never double
			// as another construction's key (dbkit's key-separation rule).
			pkiLocalKeyCipherKey, err := declaredMaterial(reg, pkiLocalKeyCipherKeyPath)
			if err != nil {
				return err
			}
			pkiLocalKeyCipher, err := dbkit.NewCipher(pkiLocalKeyCipherKey)
			if err != nil {
				return fmt.Errorf("__APP_NAME__: build pki's local-key cipher: %w", err)
			}
			if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
				return fmt.Errorf("__APP_NAME__: register pki's local-key serializer: %w", regErr)
			}
			platformCipher = cipher
			return nil
		},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			if platformCipher == nil {
				return nil, fmt.Errorf("__APP_NAME__: the platform cipher is missing; its Prepare callback builds it before construction")
			}
			return platformCipher, nil
		},
	}
}

// dbComponent returns this project's database component: the registered
// "db.sqlite" descriptor carrying the capabilities this project's database
// placement has. The built-in declares none -- its own assumption is a
// per-replica file -- and every selected component must satisfy the declared
// deployment mode's requirement, so this project's placement is declared
// here instead: APP_DB_PATH names one database file every replica of a
// distributed deployment reads (point replicas at the same file), and the
// file outlives any one process. A deployment that wants a client/server
// database selects the "db.postgres" component (or its own) in composition()
// instead of this copy.
func (b *serverBuild) dbComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, "db.sqlite")
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("__APP_NAME__: the built-in db.sqlite component is not registered")
	}
	c := descriptor
	c.Name = hostComponentPrefix + "db"
	c.Capabilities |= pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	return c, nil
}

// observabilityComponent returns the engine's observability component under
// the host's own name and with the capability this project's telemetry
// genuinely has: each replica exports its own spans and metrics, sharing no
// state with any sibling, so several replicas running it split nothing. The
// engine's own descriptor declares no capability -- telemetry must never be
// the reason an assembly fails -- which under the distributed mode's
// requirement would make selecting it a startup error.
func (b *serverBuild) observabilityComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	descriptor, ok := registeredComponent(reg, "observability")
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("__APP_NAME__: the engine's observability component is not registered")
	}
	c := descriptor
	c.Name = hostComponentPrefix + "observability"
	c.Capabilities |= pkgcore.MultiReplicaSafe
	return c, nil
}

// authnComponent returns authn's descriptor with this project's own
// construction: the signing-key lifecycle taken from the pki module's own
// product, the blind-index key material the assembly resolved for the
// module's declared path, the declared deployment mode, the configuration
// module's lazy handle as authn's dynamic-configuration reader, and the SMS
// transport the "SMS sender" seam's conditional-injection rule selects.
func (b *serverBuild) authnComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return overrideComponent(reg, "authn", "authn", func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		// The signing-key lifecycle is the pki module's own construction
		// product: its *Service satisfies authn's KeySource contract
		// structurally, and the module hands it out the moment it is
		// constructed.
		pkiModule, err := pkgcore.Get[*pki.Module](reg)
		if err != nil {
			return nil, err
		}
		blindIndexKey, err := declaredMaterial(reg, authnBlindIndexKeyPath)
		if err != nil {
			return nil, err
		}
		// The config module's construction product: this construction wires
		// its lazy handle as authn's feature gate and dynamic-configuration
		// reader below, and the required edge declared beside pki's orders
		// this component after the config module's, so the product is in the
		// by-type context here.
		cfgModule, err := pkgcore.Get[*config.Module](reg)
		if err != nil {
			return nil, err
		}
		opts := []authn.Option{
			authn.WithKeySource(pkiModule.Service()),
			authn.WithBlindIndexKey(blindIndexKey),
			authn.WithDeploymentMode(b.cfg.DeploymentMode),
			// The feature gate, off the same lazy handle: authn's declared
			// feature flags are enforced at request time, so a sign-in channel
			// the configuration disables is refused at its endpoint, matching
			// the login page that reads the same flag values through the
			// config module's pre-authentication features endpoint.
			authn.WithFeatureGate(cfgModule.Handle()),
			// The dynamic-configuration reader: the handle satisfies
			// authn.SettingsReader structurally, so the module's declared
			// dynamic config items (password policy, token TTLs, the
			// trusted-provider list, per-channel credentials) take effect at
			// runtime.
			authn.WithSettingsReader(cfgModule.Handle()),
		}
		// The "SMS sender" seam follows a conditional-injection shape: the
		// seam the composition selected is wired when a gateway URL is
		// configured (under either deployment mode) or when this is a
		// standalone deployment; a distributed deployment with no gateway
		// URL stays deliberately UNWIRED, so authn's own construction fails
		// closed with ErrMissingDistributedSMSSender rather than this
		// composition silently keeping a console sender nobody in a
		// distributed replica pool is reading.
		wireSender := b.cfg.SMSGatewayURL != "" || b.cfg.DeploymentMode != pkgcore.DeploymentModeDistributed
		sender, hasSender, err := pkgcore.GetOptional[pkgcore.SMSSender](reg)
		if err != nil {
			return nil, err
		}
		if wireSender && hasSender {
			opts = append(opts, authn.WithSMSSender(sender))
		}
		authnModule, err := authn.NewModule(db, opts...)
		if err != nil {
			return nil, fmt.Errorf("__APP_NAME__: build the authn module: %w", err)
		}
		return authnModule, nil
	},
		pkgcore.Requirement{Token: (*pki.Module)(nil)},
		pkgcore.Requirement{Token: (*config.Module)(nil)},
	)
}

// configComponent returns config's descriptor declaring only: its schema
// snapshot is taken in the post-bootstrap step (configModule.Attach), after
// every module has declared its configuration items and feature flags. The
// module's own descriptor Init would take the snapshot at its own turn,
// missing whatever declared after it.
func (b *serverBuild) configComponent(reg *pkgcore.ComponentRegistry) (pkgcore.Component, error) {
	return registerOnlyComponent(reg, "config", func(instance any, reg *pkgcore.ComponentRegistry) error {
		m, ok := instance.(*config.Module)
		if !ok {
			return fmt.Errorf("__APP_NAME__: the config component was handed a %T instance, want *config.Module", instance)
		}
		return m.Register(reg)
	})
}

// registerOnlyComponent returns the module's registered descriptor with its
// Init callback reduced to the module's one declaration entry point. The
// modules whose descriptor Init does more than declare -- config and rbac --
// freeze a snapshot there (the configuration schema, the permission
// catalog), and each of those snapshots must cover the declarations of EVERY
// module, not the ones made before the snapshotting component's own turn in
// plan order. This host therefore takes the declaration here and performs the
// attach sequence itself, in the post-bootstrap step, after every module's
// declaration turn has run.
func registerOnlyComponent(reg *pkgcore.ComponentRegistry, moduleName string, declare func(any, *pkgcore.ComponentRegistry) error) (pkgcore.Component, error) {
	base, ok := registeredComponent(reg, moduleName)
	if !ok {
		return pkgcore.Component{}, fmt.Errorf("__APP_NAME__: component %q has no registered descriptor", moduleName)
	}
	c := replicaSafeModule(base)
	c.Name = hostComponentPrefix + moduleName
	c.Locales = embed.FS{}
	c.Init = func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		return declare(instance, reg)
	}
	return c, nil
}

// tenancyResolverComponent returns the request-to-tenant resolver the config
// module's two public endpoints pick whose configuration to serve with: an
// empty resolver whose lookup deliberately matches NOTHING, so unmatched
// hosts read the platform-defaults tier rather than an error -- the
// documented display decision for the unauthenticated case (the login-page
// rule).
func (b *serverBuild) tenancyResolverComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:     hostComponentPrefix + "tenancy-resolver",
		Provides: []any{(*tenancy.Resolver)(nil)},
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			return tenancy.NewDomainResolver(
				func(host string) (pkgcore.TenantID, bool) {
					return "", false
				},
				"",
			), nil
		},
	}
}

// hostStep is a step component's product: the marker a step's New returns. A
// step owns no resource of its own -- what it works on is the assembly's
// registry and the values it reads from the by-type context.
type hostStep struct{}

// newHostStep produces the marker a step component's New returns.
func newHostStep(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
	return &hostStep{}, nil
}

// postBootstrapComponent returns the host's post-bootstrap step: the step
// that runs after every module's own Init turn has declared, takes the
// freeze point those declarations feed -- config's schema snapshot --
// through the module's own Attach call, publishes the service into the
// registry for the steps and the face that follow, and publishes the merged
// message catalog.
func (b *serverBuild) postBootstrapComponent() pkgcore.Component {
	return pkgcore.Component{
		Name: hostComponentPrefix + "post_bootstrap",
		New:  newHostStep,
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			if err := b.bindRegistry(reg); err != nil {
				return err
			}
			// The merged catalog is published into the by-type context here:
			// rendering a message (org's invitation mail, a notification
			// template, a verification code) reads it through the registry,
			// and this step runs before the first served request can render.
			catalog, err := hostCatalog(reg)
			if err != nil {
				return err
			}
			reg.Put(catalog)

			// config's schema snapshot: every module's configuration items
			// and feature flags are declared by now, so the schema the writes
			// below (and every later read) validate against is complete.
			var attachErr error
			if b.configService, attachErr = b.configModule.Attach(reg); attachErr != nil {
				return fmt.Errorf("__APP_NAME__: attach the config module: %w", attachErr)
			}
			reg.Put(b.configService)
			return nil
		},
	}
}

// hostCatalog merges the selected components' locale resources into the
// message catalog, plus the resources of the modules whose descriptors this
// host overrides (overriddenModuleLocales above). The component name is the
// locale id prefix -- the builder rejects an asset whose message ids do not
// start with the name it is registered under, so an override component
// cannot carry its module's locales (it carries a host name) and those
// resources merge here under the module's own name.
func hostCatalog(reg *pkgcore.ComponentRegistry) (*i18n.Catalog, error) {
	var zeroLocales embed.FS
	builder := i18n.NewBuilder()
	for _, asset := range pkgcore.Assets(reg) {
		if asset.Locales == zeroLocales {
			continue
		}
		if err := builder.AddModule(asset.Name, asset.Locales); err != nil {
			return nil, fmt.Errorf("__APP_NAME__: component %q has invalid locale resources: %w", asset.Name, err)
		}
	}
	for _, overridden := range overriddenModuleLocales {
		if err := builder.AddModule(overridden.name, overridden.fs); err != nil {
			return nil, fmt.Errorf("__APP_NAME__: overridden module %q has invalid locale resources: %w", overridden.name, err)
		}
	}
	return builder.Build(), nil
}

// bindRegistry reads the assembled products this host's steps work from into
// the build state, so one step body serves the component drive with the
// values the registry resolved. A missing product fails the step by name
// rather than surfacing as a nil dereference inside it.
func (b *serverBuild) bindRegistry(reg *pkgcore.ComponentRegistry) error {
	var err error
	if b.configModule, err = pkgcore.Get[*config.Module](reg); err != nil {
		return fmt.Errorf("__APP_NAME__: read the config module: %w", err)
	}
	if b.pkiModule, err = pkgcore.Get[*pki.Module](reg); err != nil {
		return fmt.Errorf("__APP_NAME__: read the pki module: %w", err)
	}
	if b.authnModule, err = pkgcore.Get[*authn.Module](reg); err != nil {
		return fmt.Errorf("__APP_NAME__: read the authn module: %w", err)
	}
	return nil
}

// hostFace is the application component's product: the composed HTTP handler
// and the listener's drain state. Its reads and lifecycle methods are what
// this file's serve loop drives; the fields stay unexported because only the
// component's callbacks write them.
type hostFace struct {
	// handler is the composed face: the protected-face composition
	// (composeFace) over the mux carrying the platform liveness routes.
	handler http.Handler
	// server is the http.Server Start builds and serves. It carries the
	// observability middleware as its handler, the serve timeouts and the
	// request base context.
	server *http.Server
	// listener is the bound listener Start serves on.
	listener net.Listener
	// addr is the listen address: the resolved port before Start, the
	// listener's own address after it.
	addr string
	// baseCtx is the context every served request inherits. It is
	// deliberately the host's own assembly context, never the signal-derived
	// context a serve loop waits on, so a shutdown signal never cancels
	// in-flight requests ahead of the drain (net/http's Server.BaseContext
	// contract).
	baseCtx context.Context
	// drained closes when the asynchronous drain Stop began has finished;
	// drainErr carries its result. Both are written before the close, so a
	// reader that sees the closed channel sees the error.
	drained  chan struct{}
	drainErr error

	stopOnce sync.Once
}

// start builds the http.Server over the composed handler and serves it on
// the face's listen address, in a goroutine: the observability middleware is
// applied here, at serve time, exactly where the engine's serve loop applies
// it (the outermost layer a served request reaches), the server's request
// base context is the face's own, and Start records the listener's real
// address so a caller can reach a port the operating system picked.
func (f *hostFace) start(ctx context.Context) error {
	server := &http.Server{
		Addr:              f.addr,
		Handler:           obs.Middleware(f.handler),
		ReadHeaderTimeout: speedapp.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return f.baseCtx },
	}
	listener, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("__APP_NAME__: serve: listen on %s: %w", f.addr, err)
	}
	f.server = server
	f.listener = listener
	f.addr = listener.Addr().String()
	f.drained = make(chan struct{})
	obs.FromContext(ctx).Info("server listening", "addr", f.addr)
	go func() {
		if err := server.Serve(f.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			obs.FromContext(ctx).Error("the server stopped serving", "error", err)
		}
	}()
	return nil
}

// stop is the non-blocking half of shutdown: it detaches a goroutine that
// stops the listener from accepting and waits out the in-flight requests,
// bounded by the shutdown timeout, and returns immediately. A face that
// never started is a no-op, so Stop is safe before Start.
func (f *hostFace) stop(ctx context.Context) {
	if f.server == nil || f.drained == nil {
		return
	}
	f.stopOnce.Do(func() {
		go func() {
			drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), speedapp.ShutdownTimeout)
			defer cancel()
			f.drainErr = f.server.Shutdown(drainCtx)
			close(f.drained)
		}()
	})
}

// close is the blocking half: it reports the drain stop began, waiting it
// out bounded by the shutdown timeout on top of the caller's context. A face
// whose drain has not finished -- Close before Stop, or a drain still in
// flight -- stops accepting and waits out the in-flight requests here,
// synchronously and bounded the same way; a face that never started releases
// nothing.
func (f *hostFace) close(ctx context.Context) error {
	if f.server == nil {
		return nil
	}
	if f.drained != nil {
		select {
		case <-f.drained:
			if f.drainErr != nil {
				return fmt.Errorf("__APP_NAME__: shut the HTTP server down: %w", f.drainErr)
			}
			return nil
		default:
		}
	}

	drainCtx, cancel := context.WithTimeout(ctx, speedapp.ShutdownTimeout)
	defer cancel()
	if err := f.server.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("__APP_NAME__: shut the HTTP server down: %w", err)
	}
	return nil
}

// hostFaceOf returns the application component's own product from the
// instance a callback was handed. The registry passes back the very value
// New returned, so a mismatch means the descriptor and its callbacks
// disagree about the product -- a wiring error reported by name rather than
// a panic inside a lifecycle callback.
func hostFaceOf(instance any) (*hostFace, error) {
	face, ok := instance.(*hostFace)
	if !ok {
		return nil, fmt.Errorf("__APP_NAME__: the application component was handed a %T, want its own *hostFace product", instance)
	}
	return face, nil
}

// appComponent returns the host's application component: the one component
// that owns the HTTP face and the listener. Its Init composes the face -- a
// phase that must compose it: composing installs the subscriptions the
// middleware chain carries and the mounting rule resolves the selected
// modules' routes, and no route declaration may land after Start. Start only
// listens; Stop begins the non-blocking drain and Close waits it out.
//
// baseCtx is the context every served request inherits (see hostFace's own
// doc comment for why it is not the signal-derived one).
func appComponent(b *serverBuild, baseCtx context.Context) pkgcore.Component {
	return pkgcore.Component{
		Name:     hostComponentPrefix + "app",
		Provides: []any{(*hostFace)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &hostFace{baseCtx: baseCtx}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			if err := b.bindRegistry(reg); err != nil {
				return err
			}
			return b.composeHostFace(reg, face)
		},
		Start: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			return face.start(ctx)
		},
		Stop: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			face.stop(ctx)
			return nil
		},
		Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			face, err := hostFaceOf(instance)
			if err != nil {
				return err
			}
			return face.close(ctx)
		},
	}
}

// composeHostFace composes the face the application component serves, in the
// one order the pieces require: a mux carrying the platform liveness routes,
// the mounted-route seed for the observability middleware's route-label
// budget (healthz and metrics, which no module registers, plus every route
// the assembly mounted -- the last write before that middleware is
// constructed, which Start does), then the protected face composeFace builds
// over it.
func (b *serverBuild) composeHostFace(reg *pkgcore.ComponentRegistry, face *hostFace) error {
	mux := http.NewServeMux()
	obs.MountLiveness(mux)
	obs.RegisterMountedRoutes(append([]pkgcore.MountedRoute{
		{Path: obs.HealthzPath},
		{Path: obs.MetricsPath},
	}, reg.MountedRoutes()...))

	handler, err := b.composeFace(reg, mux)
	if err != nil {
		return err
	}
	face.handler = handler
	face.addr = ":" + b.cfg.Port
	return nil
}

// composeFace is the protected-face composition the application component
// runs in its Init. It derives the whole middleware chain from the registry
// with chain.Standard -- the route partition (authn's subtree exempted with
// authn.ExemptSubtree, everything else mounted on the mux), the fixed
// middleware order and the mounted-route admission live there, not here. No
// authorization option is passed: none of this composition's modules
// declares a route-level permission, so Standard mounts every non-authn
// route unguarded, and the tenancy chain's fail-closed default is what keeps
// each of them behind a verified Principal.
func (b *serverBuild) composeFace(reg *pkgcore.ComponentRegistry, mux *http.ServeMux) (http.Handler, error) {
	handler, err := speedchain.Standard(
		reg,
		b.authnModule.Service().Verifier(),
		mux,
	)
	if err != nil {
		return nil, fmt.Errorf("__APP_NAME__: compose the middleware chain: %w", err)
	}
	return handler, nil
}
