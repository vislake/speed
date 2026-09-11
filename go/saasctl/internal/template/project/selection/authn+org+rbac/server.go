//go:build ignore

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"

	speedapp "github.com/vislake/speed/go/app"
	speedchain "github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the database the engine opens has a driver to build from -- this
	// skeleton's own database always speaks SQLite, regardless of which
	// deployment mode its other infrastructure seams compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// serverBuild carries the assembly state this selection's callbacks hand each
// other: the resolved configuration and the loader target the engine fills,
// the modules the WithModules stage constructs, the services the post-bootstrap
// attach derives from them, and the Redis resources this host built and
// therefore owns. The declared key materials are not carried here: the
// assembly resolves them off the module components' declarations and hands
// them to the callbacks as the published bootstrap material.
type serverBuild struct {
	cfg        serverConfig
	hostConfig hostConfig

	redisBus    *eventbusredis.EventBus
	redisClient *redis.Client

	configModule *config.Module
	orgModule    *org.Module
	pkiModule    *pki.Module
	authnModule  *authn.Module
	rbacModule   *rbac.Module

	configService *config.Service
	rbacService   *rbac.Service

	reg *pkgcore.Registry
}

// runServer assembles this project through the application engine and serves
// it until the process is signalled. The engine's Run owns the whole
// lifecycle -- signal handling, observability init, the HTTP serve and the
// ordered drain -- and this file's callbacks are the host's seats inside the
// one fixed assembly order every speed application shares.
//
// This composition wires the authn, org, config and rbac modules (the
// generator's default --with set) plus pki, which is not part of the --with
// set at all -- it follows authn silently, supplying the KeySource authn's
// signing keys live behind. The middleware chain is chain.Standard's
// derivation: authn first, so each token is verified exactly once and the
// tenant comes from the verified Principal's claims, never a Host header,
// then tenancy under the chain's pre-auth allowlist -- authn.Middleware is
// optional auth (a bad token is a 401, an absent one stays anonymous), so
// tenancy's fail-closed default is what makes every route NOT on the
// allowlist require a valid Principal with no per-route wrapping. The
// allowlist covers only the non-authn routes that must work before a
// Principal exists: healthz, metrics and config's two pre-auth display
// endpoints. authn's own pre-auth operations need NO allowlist entries at
// all -- Standard splits the whole subtree under authn's API path onto its
// own branch ahead of tenancy, which is also what lets enterprise SSO's
// dynamic per-tenant provider names ("oidc:<tenant>") work: no allowlist
// could enumerate them, and authn's handler decides per operation which of
// its routes require a Principal (go/authn/handler.go).
//
// Host seams deliberately left unwired, each failing closed per the owning
// module's contract and each the owner's first task: authn's
// MembershipReader (session refresh re-verifies the session's current
// tenant against it, and an absent reader refuses rather than defaulting
// -- a freshly signed-in user has no tenant until the owner builds one);
// org's SubjectResolver (org's caller-scoped endpoints answer 401 until a
// real one derives the caller from verified token claims); org's
// invitation emailing (disabled via WithInvitationEmailDisabled below --
// sending needs a mail transport, a mail-from address and a frontend
// acceptance page a skeleton must not demand at Register); and the config
// resolver's host map, which deliberately matches NOTHING so unmatched
// hosts read platform defaults, never an error (the login-page rule; a
// static unauthenticated Host map would violate tenancy's own Resolver
// contract, go/tenancy/resolver.go).
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
// resolve under, the database, the
// encrypted-column registrations, the module set, the kernel's seam
// composition, the observability spec, the HTTP face, and the host's attach
// hook. Everything host-specific the engine cannot know is named here;
// nothing is defaulted on the host's behalf.
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
		speedapp.WithPreDB(b.registerEncryptedColumns),
		speedapp.WithModules(b.constructModules),
		speedapp.WithKernelOptions(b.kernelOptions()...),
		speedapp.WithObservability(speedapp.ObservabilitySpec{
			ServiceName:  "__APP_NAME__",
			OTLPEndpoint: b.cfg.OTLPEndpoint,
		}),
		speedapp.WithHTTP(b.httpSpec()),
		speedapp.WithHooks(speedapp.Hooks{PostBootstrap: b.postBootstrap}),
	}
}

// registerEncryptedColumns is the engine's pre-database stage: authn's and
// pki's encrypted columns need their serializers registered BEFORE
// dbkit.Open, because GORM resolves a model's serializer while it parses the
// schema and the registration is process-global (each registrar's own doc
// comment states the contract). deps carries the engine's platform cipher,
// built from the config.cipher_key material, and the assembly's resolved
// bootstrap material, which is where authn's PII key and pki's local-key
// cipher key come from -- separate secrets, because an AES key must never
// double as another construction's key (dbkit's key-separation rule).
func (b *serverBuild) registerEncryptedColumns(_ context.Context, deps speedapp.PreDBDeps) error {
	piiKey, err := declaredKeyMaterial(deps.Material, "authn.pii_cipher_key")
	if err != nil {
		return err
	}
	piiCipher, err := dbkit.NewCipher(piiKey)
	if err != nil {
		return fmt.Errorf("__APP_NAME__: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return fmt.Errorf("__APP_NAME__: register authn's PII serializer: %w", regErr)
	}

	pkiLocalKeyKey, err := declaredKeyMaterial(deps.Material, "pki.local_key_cipher_key")
	if err != nil {
		return err
	}
	pkiLocalKeyCipher, err := dbkit.NewCipher(pkiLocalKeyKey)
	if err != nil {
		return fmt.Errorf("__APP_NAME__: build pki's local-key cipher: %w", err)
	}
	if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
		return fmt.Errorf("__APP_NAME__: register pki's local-key serializer: %w", regErr)
	}

	// org's Invitation.Email column is encrypted at rest under this same
	// platform cipher and made queryable by a SEPARATE HMAC key,
	// org.invitation_email_index_key's material, which constructModules hands
	// org.NewEmailIndexer: reusing the cipher key for both would be exactly
	// the AES-key-doubling-as-an-HMAC-key weakness dbkit warns about. The
	// module's own registrar and constructor own the serializer name and the
	// index column, so neither crosses this wiring as a hand-typed string.
	if regErr := org.RegisterEmailSerializer(deps.Cipher); regErr != nil {
		return fmt.Errorf("__APP_NAME__: register org's email serializer: %w", regErr)
	}
	return nil
}

// constructModules is the engine's module-construction stage: the modules
// this selection wires, built explicitly here -- the engine never knows a
// module type -- in the order the kernel registers them, which is also the
// order their migrations apply. Every Register-time declaration (authn's
// config items, permissions and events first, then org's, then config's own
// Register, and rbac's Attach-time snapshot last) lands before the step
// that freezes it.
func (b *serverBuild) constructModules(_ context.Context, deps speedapp.ModuleDeps) ([]pkgcore.Module, error) {
	db := deps.DB

	// pki owns authn's signing-key lifecycle: LocalSigner (its own
	// zero-external-dependency default, exactly what this standalone
	// composition needs) generates and stores the key in the database, so it
	// persists across restarts with no dev-seed derivation required -- see
	// config.go's devPKILocalKeyCipherKey doc comment for what seals that
	// stored key.
	b.pkiModule = pki.NewModule(db)

	// authn's "SMS sender" seam follows a conditional-injection shape: a
	// configured gateway URL always wins, under either deployment mode, and
	// composes the real HTTP SMS transport (pkgcore.NewHTTPSMSSender); absent
	// that, the standalone deployment mode falls back to the console
	// transport, while the distributed deployment mode is left deliberately
	// UNWIRED -- authn.NewModule's own options then fail closed with
	// authn.ErrMissingDistributedSMSSender rather than this composition
	// silently keeping a console sender nobody in a distributed replica pool
	// is reading.
	// authn's blind-index key is one of the declared key materials the
	// assembly resolved before this callback ran; the material source it
	// published is where the declared path resolves.
	blindIndexKey, err := declaredKeyMaterial(deps.Material, "authn.blind_index_key")
	if err != nil {
		return nil, err
	}
	authnOpts := []authn.Option{
		authn.WithKeySource(b.pkiModule.Service()),
		authn.WithBlindIndexKey(blindIndexKey),
		authn.WithDeploymentMode(b.cfg.DeploymentMode),
	}
	switch {
	case b.cfg.SMSGatewayURL != "":
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewHTTPSMSSender(b.cfg.SMSGatewayURL)))
	case b.cfg.DeploymentMode != pkgcore.DeploymentModeDistributed:
		authnOpts = append(authnOpts, authn.WithSMSSender(pkgcore.NewConsoleSMSSender(os.Stdout)))
	}
	authnModule, err := authn.NewModule(db, authnOpts...)
	if err != nil {
		return nil, fmt.Errorf("__APP_NAME__: build the authn module: %w", err)
	}
	b.authnModule = authnModule

	// The org module is this project's organization-tree piece. Its blind
	// indexer is built from the org.invitation_email_index_key material --
	// the serializer above encrypted the column; this key makes it
	// queryable.
	orgIndexKey, err := declaredKeyMaterial(deps.Material, "org.invitation_email_index_key")
	if err != nil {
		return nil, err
	}
	orgIndexer, err := org.NewEmailIndexer(orgIndexKey)
	if err != nil {
		return nil, fmt.Errorf("__APP_NAME__: build the org email indexer: %w", err)
	}
	b.orgModule = org.NewModule(db,
		org.WithEmailIndexer(orgIndexer),
		org.WithInvitationEmailDisabled(),
	)

	// The config module is required in every generated composition (its two
	// pre-auth display endpoints render the login page a sign-in flow
	// presupposes). Its cipher is the engine's platform cipher -- the one
	// built from config.cipher_key -- and its resolver's lookup deliberately
	// matches NOTHING, so the display endpoints serve platform defaults to
	// every caller until the owner wires a real host-to-tenant source; an
	// empty default tenant maps unmatched hosts onto the "platform defaults"
	// tier rather than an error (see runServer's doc comment).
	b.configModule = config.NewModule(db,
		config.WithCipher(deps.Cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				return "", false
			},
			"",
		)),
	)

	// rbac needs nothing from this host but a database: it declares its own
	// permissions during Register and reads EVERY module's declarations once,
	// in Attach, after Bootstrap. The Attach-time Service is what composeFace
	// hands to the route table below it.
	b.rbacModule = rbac.NewModule(db)

	return []pkgcore.Module{b.pkiModule, b.authnModule, b.orgModule, b.configModule, b.rbacModule}, nil
}

// postBootstrap is the engine's attach stage, after the kernel bootstrapped
// the module set and the bootstrap-key binding was verified: the typed
// Attach calls every module requires exactly once after Bootstrap. The order
// is this project's own choice, not a dependency between the two -- neither
// step reads the other's Service; what binds is only that a module is
// attached before the first host step that consumes its Service.
// configModule.Attach freezes the schema snapshot of every config item and
// feature flag the modules declared during Register; rbacModule.Attach
// freezes the snapshot of every permission every module declared -- taken
// any earlier it would be missing whatever registered after it, and a
// permission missing from the catalog cannot be granted at all.
func (b *serverBuild) postBootstrap(_ context.Context, a *speedapp.Application) error {
	b.reg = a.Registry()
	var err error
	if b.configService, err = b.configModule.Attach(b.reg); err != nil {
		return fmt.Errorf("__APP_NAME__: attach the config module: %w", err)
	}
	if b.rbacService, err = b.rbacModule.Attach(b.reg); err != nil {
		return fmt.Errorf("__APP_NAME__: attach the rbac module: %w", err)
	}
	return nil
}

// httpSpec declares this project's HTTP face for the engine's stage 7: the
// listen address and the protected-face composition below.
func (b *serverBuild) httpSpec() speedapp.HTTPSpec {
	return speedapp.HTTPSpec{
		Addr:    ":" + b.cfg.Port,
		Compose: b.composeFace,
	}
}

// composeFace is the engine's protected-face callback. It derives the whole
// middleware chain from the registry with chain.Standard -- the route
// partition (authn's subtree split out with authn.ExemptSubtree, everything
// else mounted on the mux the engine prepared) and the fixed middleware
// order live there, not here -- and supplies the host-specific half: the
// authorization domain and this project's route table (routeRules below),
// through which rbac.GuardRoutes admits every mounted route, wraps each
// gated one in rbac's fail-closed gate before it is mounted, and fails --
// so this assembly fails -- when a mounted path has no declared decision or
// the table names a path no module mounted.
func (b *serverBuild) composeFace(mux *http.ServeMux) (http.Handler, error) {
	handler, err := speedchain.Standard(
		b.reg,
		b.authnModule.Service().Verifier(),
		mux,
		speedchain.WithAuthorization(b.rbacService, routeRules()),
	)
	if err != nil {
		return nil, fmt.Errorf("__APP_NAME__: compose the middleware chain: %w", err)
	}
	return handler, nil
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
// selection wires -- a distributed boot still fails closed on whichever seam
// the Preset would otherwise resolve to an in-process default, since every
// resolved seam must satisfy DeploymentModeDistributed's RequiredCapabilities
// (MultiReplicaSafe). The conditional injections below are the same shape at
// every seam: an unset env var leaves that seam on the Preset's in-process
// default, so `go run ./cmd/server` stays unaffected, and a configured one
// injects a real implementation declaring the capability bits that
// implementation genuinely carries. One Redis client backs both "eventbus"
// and "kv" -- see config.go's RedisAddr field doc comment for why wiring
// only one of the two can never let a distributed composition pass
// Bootstrap; the client is this host's own resource and runServer closes it
// (the S3 store and the SMTP mailer are injected values the kernel's
// shutdown does close, through its own registered closers).
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

// routeRules declares the authorization decision for every route the
// selected modules mount -- the table rbac.GuardRoutes refuses to serve
// around: a mounted path it does not name, an entry no module mounted, and
// an entry declaring neither a public decision nor a permission all fail
// the assembly, so a module added to the --with set later cannot start
// up with an undecided route.
//
// The gated entries select the owning module's own permission constants
// and let rbac's gate evaluate them through the default subject resolver --
// the Subject an authenticating layer installs with rbac.WithSubject. This
// skeleton wires no such layer yet, so a gated route answers 403
// rbac.permission_denied until the owner bridges its identity source into
// a Subject (the seam rbac.WithSubjectResolver documents) and grants
// roles; that is deliberately the fail-closed direction. The public
// entries are the platform's own pre-auth surfaces, declared explicitly
// rather than left to omission.
func routeRules() []rbac.RouteRule {
	return []rbac.RouteRule{
		// authn's subtree is split out ahead of the gated mux by
		// chain.Standard (authn.ExemptSubtree), and authn.Handler decides
		// per operation which of its routes require a Principal; the entry
		// keeps the table exhaustive over what authn mounts.
		{Path: speedapp.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}},
		// config's two pre-auth display endpoints: a login page's brand and
		// feature flags must render before anyone has signed in.
		{Path: config.PathPublic, Access: pkgcore.RouteAccess{Public: true}},
		{Path: config.PathSystemFeatures, Access: pkgcore.RouteAccess{Public: true}},
		// org's handler performs no permission check of its own, so this
		// entry's selector is where org's four declared permissions are
		// enforced, per sub-resource exactly as go/org documents them. The
		// exemption is the accept-invitation operation: a person accepting
		// their FIRST invitation has no rbac grant yet in the tenant they
		// are about to join, and org.Handler's own caller resolution is
		// that operation's whole gate.
		{
			Path:   orgAPIPath,
			Access: pkgcore.RouteAccess{Permission: orgPermissionFor},
			Exempt: orgAcceptInvitationRequest,
		},
		// pki's handler performs no permission check of its own, so this
		// entry gates its route on the module's own read permission -- the
		// three read operations (both JWKS exports and the CRL fetch) are
		// GETs, and pkiPermissionFor demands nothing of every other
		// method, which DENIES. The two revoke operations are deliberately
		// not opened here: gating them correctly requires the
		// platform-domain evaluation the signing-key half's permission
		// contract mandates (a subject resolver pinning rbac.SystemDomain,
		// go/pki/AGENTS.md), and the owner adds that resolver and the
		// revoke permissions together -- never the permissions first,
		// because a tenant's own owner role carries every permission any
		// module declared, platform-scoped ones included.
		{Path: pkiAPIPath, Access: pkgcore.RouteAccess{Permission: pkiPermissionFor}},
	}
}

// orgAPIPath is where the org module mounts its HTTP surface (the module
// keeps its own path constant unexported; the table's exactness check
// turns a drift into a startup error naming the mounted path).
const orgAPIPath = "/api/v1/org"

// orgPermissionFor selects the org:* permission a request must hold, from
// its sub-resource and method alone -- the module's own per-operation
// contract, mirrored here: the tree and anything this selector does not
// recognize require the read permission on GET/HEAD and the manage
// permission on every write (the strict direction: an unrecognized write
// demands more, never less), the member sub-resource its own
// remove-member permission, and the invitation sub-resource its own
// invite-member permission. The accept operation never reaches this
// function: the org entry's exemption short-circuits it first.
func orgPermissionFor(r *http.Request) string {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	path := strings.TrimPrefix(r.URL.Path, orgAPIPath)
	switch {
	case strings.HasPrefix(path, "/members"):
		if read {
			return org.PermissionRead
		}
		return org.PermissionRemoveMember
	case strings.HasPrefix(path, "/invitations"):
		if read {
			return org.PermissionRead
		}
		return org.PermissionInviteMember
	default:
		if read {
			return org.PermissionRead
		}
		return org.PermissionManage
	}
}

// orgAcceptInvitationRequest names the one org operation the entry's
// Exempt predicate lets through with no permission check at all -- see the
// org entry's own comment for why.
func orgAcceptInvitationRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == orgAPIPath+"/invitations/accept"
}

// pkiAPIPath is where the pki module mounts its HTTP surface (the module
// keeps its own path constant unexported; the table's exactness check
// turns a drift into a startup error naming the mounted path).
const pkiAPIPath = "/api/v1/pki"

// pkiPermissionFor selects the pki:* permission a request must hold: the
// module's read permission on GET/HEAD, and nothing -- which denies -- on
// every other method. See the pki entry's own comment for what the owner
// adds here and in what order.
func pkiPermissionFor(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return pki.PermissionRead
	default:
		return ""
	}
}
