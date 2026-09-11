// This file is the reference app's assembly core: ServerConfig, the mapping
// of the resolved configuration onto the application engine's option set,
// and the host's teardown. It sits in internal/app so the composed server is
// importable: cmd/server's main.go boots it as the thin process shell, and
// the assembly-flow suites, which live beside the command in cmd/server,
// exercise the package through the command's tests. See doc.go for the
// package's design.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the engine's dbkit.Open has a driver to build from -- the reference
	// app runs its own database in standalone deployment mode's SQLite
	// dialect regardless of which deployment mode its other infrastructure
	// seams compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	obs "github.com/vislake/speed/go/observability"

	// Blank-imported for its init() side effect: obs.Init's local
	// exporters wire a real /metrics scrape endpoint only when a local
	// metrics reader has been registered (go/observability's own doc
	// comment on Init and RegisterLocalMetricsReader) -- this is what
	// obs.MountLiveness's metrics route actually serves once Run has called
	// obs.Init. Without this import, obs.Init still runs (traces and
	// metrics both go to stdout), but the metrics route answers 404.
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"

	// Blank-imported for its init() side effect: registers the OTLP/gRPC
	// exporter factory obs.Init consults exactly when a caller supplies a
	// non-empty WithOTLPEndpoint (go/observability's ErrOTLPExporterNotRegistered
	// names this import as the fix). Without it, an APP_OTLP_ENDPOINT set
	// in ConfigFromEnv would fail Run's Init with that error instead of
	// wiring the collector push the variable promises. Registration is
	// inert while the endpoint stays unset: Init stays on the local
	// exporters, byte-identical to this import never having existed.
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"

	// Blank-imported for its init() side effect: registers "objectstore.s3"
	// on pkgcore's shared ObjectStoreRegistry, the name the preset entry the
	// APP_S3_* composition overrides points the "objectstore" seam at.
	// Without this import the entry would fail Bootstrap with
	// ErrUnknownImplementation -- the database/sql driver trade every
	// registered built-in makes.
	_ "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	"github.com/vislake/speed/examples/reference-app/internal/cases"
	demomodule "github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

const (
	// DefaultPort is used when the PORT environment variable is unset.
	DefaultPort = "8080"

	// DefaultSQLitePath is used when APP_DB_PATH is unset. It is a
	// relative path so `go run ./cmd/server` works with zero setup -- the
	// example's own entry point must stay runnable with nothing
	// configured.
	DefaultSQLitePath = "reference-app.db"
)

// ServerConfig is main.go's own bootstrap wiring configuration -- the
// values a process must know before anything else can start (deployment
// mode, port, database path, the config master key, the optional Redis
// address, the demo host map). It is a plain struct resolved from the
// process environment by ConfigFromEnv's loader-driven bootstrap
// (bootstrap.go), NOT the dynamic configuration the config
// module serves: dynamic configuration lives in the configs table and can
// never hold the very key that encrypts it, so this bootstrap struct is
// the deliberate exception to "a plain struct, not pkgcore/config's
// dynamic configuration" -- it is main.go's own wiring, which never goes
// through Module.Register either.
type ServerConfig struct {
	DeploymentMode       pkgcore.DeploymentMode
	Port                 string
	SQLitePath           string
	ConfigKey            []byte
	OrgIndexKey          []byte
	NotificationIndexKey []byte

	// PKILocalKeyCipherKey, AuthnBlindIndexKey and AuthnPIICipherKey are
	// the three key materials whose environment overrides arrive through
	// APP_PKI_LOCAL_KEY_CIPHER_KEY, APP_AUTHN_BLIND_INDEX_KEY and
	// APP_AUTHN_PII_CIPHER_KEY -- see the Authn and Pki fields' own doc
	// comments (bootstrap.go) for what each
	// protects and why each is a separate secret.
	// ConfigFromEnv resolves all six key fields on this struct (these
	// three plus ConfigKey/OrgIndexKey/NotificationIndexKey above) through
	// the same three-tier precedence: an explicitly-set individual
	// environment variable wins over a APP_ROOT_KEY derivation, which
	// wins over the hardcoded development default.
	PKILocalKeyCipherKey []byte
	AuthnBlindIndexKey   []byte
	AuthnPIICipherKey    []byte

	RedisAddr   string
	HostTenants map[string]pkgcore.TenantID

	// OTLPEndpoint is the "host:port" target this deployment pushes its
	// traces and metrics to over OTLP/gRPC when non-empty (see the
	// OTLPEndpoint field's own doc comment (bootstrap.go) for what an empty
	// value means). ConfigFromEnv fills it from APP_OTLP_ENDPOINT; main.go's run
	// hands it to obs.Init through obs.WithOTLPEndpoint, exactly the shape
	// cfg.RedisAddr demonstrates for the Redis-backed seams.
	OTLPEndpoint string

	// PublicOrigin is this deployment's own public origin (scheme://host,
	// optional :port, no path), the fallback base URL org's invitation
	// accept links point at for a tenant that has no branded host in
	// HostTenants -- every self-registered clinic, whose tenant id
	// self_service.go derives from its registrant and which no configured
	// host can name (see the PublicOrigin field's own doc comment
	// (bootstrap.go) for the
	// full population split). ConfigFromEnv fills it from APP_PUBLIC_ORIGIN,
	// defaulting to "http://localhost:" + the resolved PORT so a
	// zero-setup local demo renders working links with no configuration;
	// the field's zero value keeps the fail-loud behavior for a
	// host that sets neither a branded host nor an origin, and the error
	// such a link build produces names this variable.
	PublicOrigin string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region, S3UseSSL
	// and S3BucketLookup name the registered "objectstore.s3"
	// implementation for the "objectstore" seam through the Preset's
	// config channel when S3Endpoint is non-empty: BuildServer overrides
	// that one entry of the standalone Preset with these values, and the
	// registration builds the S3-compatible ObjectStore from them -- the
	// host pre-builds nothing. The S3 fields' own doc comment
	// (bootstrap.go) has the completeness rule; an empty S3BucketLookup
	// reads as the store's endpoint-derived auto default.
	// Empty S3Endpoint (the default) leaves "objectstore" on the Preset's
	// local-directory default.
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3Region       string
	S3UseSSL       bool
	S3BucketLookup string

	// ObjectStoreRoot, when non-empty, replaces the "objectstore" seam's
	// Preset default with pkgcore.NewLocalObjectStore over this fixed
	// directory -- the local twin of the S3 fields above, and an
	// alternative to them (ConfigFromEnv refuses both a complete S3
	// composition and APP_OBJECT_STORE_ROOT, per the ObjectStoreRoot field's
	// own doc comment (bootstrap.go)). BuildServer injects it declaring the
	// SurvivesRestart
	// capability alone, never MultiReplicaSafe: the directory survives a
	// process restart, but nothing about a single-process local store is
	// replica-safe. The field exists because a host that needs its objects
	// to outlive one process must name the directory itself -- the Preset
	// default is a throwaway MkdirTemp -- which is exactly what the
	// two-boot expiry-sweep flow test (flowtests/periodic_scheduler_flow_test.go)
	// needs: boot 2's sweep must find the bytes boot 1 wrote.
	ObjectStoreRoot string

	// SMTPHost, SMTPPort, SMTPUsername, SMTPPassword and SMTPReplyTo name the
	// registered "mailer.smtp" implementation for the "mailer" seam through
	// the Preset's
	// config channel when SMTPHost is non-empty: BuildServer overrides that
	// one entry of the standalone Preset with these values, and the
	// registration builds the SMTP Mailer from them -- the host pre-builds
	// nothing. The SMTP fields' own doc comment (bootstrap.go) has the
	// completeness rule.
	// Empty SMTPHost (the default) leaves "mailer" on the Preset's
	// console default, exactly like an unset Mailer field below.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	// SMTPReplyTo is the transport-level Reply-To default the "mailer.smtp"
	// registration carries (pkgcore.SMTPConfig.ReplyTo); the modules' own
	// Reply-To (server.go's org/notification assembly) wins over it.
	SMTPReplyTo string

	// SMSGatewayURL composes the real HTTP SMS transport
	// (pkgcore.NewHTTPSMSSender) for authn's "SMS sender" seam when
	// non-empty. See the SMSGatewayURL field's own doc comment (bootstrap.go)
	// for what an
	// empty value means under each deployment mode.
	SMSGatewayURL string

	// DisableQueueWorker, when true, makes BuildServer skip
	// standaloneQueue.Start -- see the DisableQueueWorker field's own doc
	// comment (bootstrap.go) for why this exists and what it changes.
	// ConfigFromEnv sets it
	// from APP_DISABLE_QUEUE_WORKER; false (the default) starts the queue
	// worker normally.
	DisableQueueWorker bool

	// FailSelfServiceProvision is the failure-injection hook for the
	// self-service provisioning chain (self_service.go): when non-nil,
	// every provisioning attempt the server makes consults it at the top
	// of provision and fails when it returns an error, so a failure can
	// be placed on the synchronous delivery of a registration's event and
	// the retry job watched converging the same clinic. It has two
	// writers: ConfigFromEnv arms it from APP_FAIL_SELF_SERVICE_PROVISION
	// (absent or "0" leaves it nil -- the production default -- and N
	// arms a hook failing the first N attempts of each account; the
	// FailSelfServiceProvision field's own doc comment (bootstrap.go) has the
	// contract),
	// and the reference-app suites arm it on their own ServerConfig
	// before BuildServer captures it into the provisioner it builds
	// (wireSelfService). The hook answers per user id: a test's closure
	// counts or keys on that argument however its scenario needs.
	FailSelfServiceProvision func(userID string) error

	// DisableDemoUserHeader, when true, makes BuildServer wire every demo
	// identity source away from the demo headers: every permission-gated
	// route's SubjectResolver through DemoSubjectResolverFor(true), and
	// every attribution seam through headerDisabled
	// DemoOrgSubjectResolverFor/DemoNotesSubjectResolver instances -- see
	// the DisableDemoUserHeader field's own doc comment (bootstrap.go) for
	// why this exists
	// and exactly what it changes. ConfigFromEnv sets it from
	// APP_DISABLE_DEMO_USER_HEADER; false (the default) keeps every demo
	// identity source on its header-enabled wiring, matching
	// DisableQueueWorker's own contract just above.
	DisableDemoUserHeader bool

	// TrustedProxies is the authn.WithTrustedProxies declaration: the IP
	// addresses and CIDR prefixes of the reverse proxies this deployment
	// receives requests through, so authn's session/login-history records
	// carry the real client address (recovered from the X-Forwarded-For
	// chain those proxies append) instead of the proxy's address.
	// ConfigFromEnv fills it from APP_TRUSTED_PROXIES, a comma-separated
	// list (see the TrustedProxies field's own doc comment (bootstrap.go));
	// the empty default -- the
	// zero-external-dependency `go run ./cmd/server` experience, and every
	// test's config -- keeps authn's fail-closed behavior of recording
	// every request's direct connection address. A value whose entries are
	// not IP addresses or CIDR prefixes refuses boot:
	// authn.WithTrustedProxies validates its input at module construction
	// (go/authn's newOptions).
	// A deployment behind a proxy declares the proxy here or its records
	// stay proxy-addressed; it must never declare an untrusted range, and
	// the declared proxy must overwrite or strip the X-Forwarded-For it
	// receives from its own clients, exactly as Fly.io's proxy does.
	TrustedProxies []string

	// ReadFlyClientIP is this deployment's declaration that its proxy is
	// Fly's, the per-header opt-in (APP_READ_FLY_CLIENT_IP)
	// that authorizes authn to read the single-hop Fly-Client-IP vendor
	// header for a request whose peer is within TrustedProxies (go/authn's
	// WithVendorClientIPHeaders / VendorClientIPHeaderFlyClientIP). The
	// header is the real client address on every request Fly's proxy
	// forwards, so this app's session/login-history records keep carrying
	// the real client behind the Fly proxy. False (the default) reads no
	// vendor header at all -- authn falls back to X-Forwarded-For and the
	// connection address -- which is the correct fail-closed shape for a
	// non-Fly deployment: reading Fly-Client-IP for ANY declared proxy
	// would let a client smuggle its own value through a generic reverse
	// proxy that forwards unknown headers verbatim (nginx, ALB, Envoy,
	// Cloudflare), minting its own recorded AND rate-limited address.
	// ConfigFromEnv refuses true with an empty TrustedProxies: that
	// combination could never read the header and would silently keep
	// recording the proxy itself.
	ReadFlyClientIP bool

	// WebDistDir names the directory holding this app's built frontend
	// (the dist/ examples/reference-app/web's `pnpm build` emits) when
	// this process should serve that frontend itself -- see frontend.go's
	// own package doc comment for this app's wiring and pkgcore/spa's for
	// the full serving contract. ConfigFromEnv
	// sets it from APP_WEB_DIST; the empty default (the zero-external-
	// dependency `go run ./cmd/server` experience, and every test's
	// config) leaves the composed handler serving no static files at all.
	WebDistDir string

	// PeriodicTaskInterval is the tick cadence of this host's
	// periodic-task scheduler (the jobs.Scheduler BuildServer installs
	// over the registry's declared schedules; see periodic_scheduler.go).
	// ConfigFromEnv never sets it, so zero (the default) means the
	// scheduler's own default, jobs.DefaultScheduleInterval, and the flow
	// tests inject a sub-second interval to drive real ticks within test
	// time -- the same test-override shape Mailer and Memberships below
	// use.
	PeriodicTaskInterval time.Duration

	// PKIPropagationWindow, PKIRenewalLeadTime and PKIExpiryScanWindow
	// override pki's rotation timing and its expiry-scan idempotency window
	// when non-zero: BuildServer applies pki.WithPropagationWindow,
	// pki.WithRenewalLeadTime and pki.WithExpiryScanWindow only for values
	// above zero, so the zero default (what ConfigFromEnv always leaves
	// them at) keeps the module's own DefaultPropagationWindow /
	// DefaultRenewalLeadTime / DefaultExpiryScanWindow in force. The pki
	// flow test
	// injects a renewal lead time past a signing key's validity so the very
	// next expiry scan stages its replacement within test time, and a scan
	// window below the scheduler's own tick cadence so every test tick
	// lands in a fresh window and each tick really runs a scan (the
	// production defaults would stall the test's stage-then-promote proof
	// across window boundaries no test time can wait out).
	PKIPropagationWindow time.Duration
	PKIRenewalLeadTime   time.Duration
	PKIExpiryScanWindow  time.Duration

	// Mailer overrides the "mailer" seam with the host's own value when set
	// -- nil in production, where the seam is composed through the Preset's
	// config channel instead (the SMTPHost group above), so this field
	// exists for the org invitation-accept flows
	// (flowtests/org_flow_test.go, flowtests/org_clinic_invitation_test.go,
	// flowtests/org_invitation_signin_test.go), which need the rendered mail back
	// in-process to extract the invitation token rather than parsing it out
	// of console output. BuildServer injects it declaring pkgcore.Stateless
	// -- the honest capability for a throwaway in-process test double, and
	// exactly the bits the "mailer.smtp" registration declares for the real
	// transport.
	Mailer pkgcore.Mailer

	// Memberships is the seam authn asks tenant-membership questions
	// through (authn.WithMembershipReader below): customer-tenant answers
	// read org's own memberships table, and this store carries the
	// rbac.SystemDomain grants plus any test shortcut. Nil defaults to a
	// fresh, empty signInMemberships in BuildServer; a test that needs to
	// seed membership after registering a demo user keeps its own reference
	// by setting this field before calling BuildServer, rather than
	// reaching into BuildServer's internals. See sign_in_memberships.go's
	// own doc comment for the full shape.
	Memberships *signInMemberships

	// DemoUsersPassword, when non-empty, makes BuildServer seed the three
	// demo accounts of demo/demo_users.go at the end of its composition --
	// register each through the composed handler, then grant the
	// membership and role its actor model declares -- so a browser visitor
	// can sign in as demo-owner@example.com and friends with a real
	// account, real membership and real rbac grants, and no demo header.
	// ConfigFromEnv fills it from APP_DEMO_USERS_PASSWORD; the empty
	// default (the zero-external-dependency `go run ./cmd/server`
	// experience) skips the seed entirely.
	DemoUsersPassword string

	// DemoPlatformStaffPassword, when non-empty, makes BuildServer seed
	// the demo platform-staff account of demo/demo_admin.go (SeedDemoPlatformStaff)
	// at the end of its composition, INDEPENDENTLY of DemoUsersPassword: a
	// boot seeds each demo account set from its own variable, never one
	// from the other's. The platform administrator (BuiltinRoleOwner under
	// rbac.SystemDomain, every admin:* permission included) must never be
	// seeded from the ordinary demo users' password, so the two credential
	// sources stay apart by construction -- see the DemoPlatformStaffPassword
	// field's own doc comment (bootstrap.go) for why. ConfigFromEnv fills it from
	// APP_DEMO_PLATFORM_STAFF_PASSWORD; the empty default skips the seed.
	DemoPlatformStaffPassword string

	// SMSOutput is where the console SMS sender (the standalone deployment
	// mode's transport, pkgcore's sms.go) writes delivered messages. Nil
	// defaults to os.Stdout.
	SMSOutput io.Writer

	// SocialProviders, RedirectAllowlist and TrustedProviders wire authn's
	// social sign-in channels. All three default to empty/zero, the safe
	// nothing-enabled state this app ships with: no real OAuth app
	// credentials are configured for this example, and the per-provider
	// credential items authn registers are never read through dynamic
	// configuration (that read-through is unimplemented), so credentials
	// reach authn only through this field. flowtests/authn_e2e_test.go
	// supplies a channel pointed at a local httptest server here, to prove
	// the social sign-in flow end to end without a live provider.
	SocialProviders   []authn.SocialProvider
	RedirectAllowlist authn.RedirectAllowlist
	TrustedProviders  []string

	// AIGatewayBaseURL and AIGatewayAPIKey, when AIGatewayAPIKey is
	// non-empty, make BuildServer write a platform-wide ai-gateway
	// credential (aigateway.CredentialService.SetPlatformCredential) for
	// aigateway.ProviderOpenAICompatible at boot, so the consult module's
	// one route (consult.go) can actually reach a provider.
	// ConfigFromEnv never sets either -- there is no real OpenAI-compatible
	// key committed to this repository, the same posture SocialProviders'
	// own doc comment above describes -- so the zero-setup `go run
	// ./cmd/server` experience leaves the consult route permanently
	// answering aigateway.ErrCredentialNotFound until an operator wires a
	// real key. flowtests/consult_flow_test.go is what sets both: AIGatewayBaseURL to
	// an httptest.Server standing in for the OpenAI-compatible endpoint,
	// and AIGatewayAPIKey to a fixed test value, exactly the way cfg.Mailer
	// is a test-only override of a seam production leaves on its real
	// default.
	AIGatewayBaseURL string
	AIGatewayAPIKey  string

	// AIGatewayImageBaseURL and AIGatewayImageAPIKey are the image-side
	// mirror of AIGatewayBaseURL/AIGatewayAPIKey, above: when
	// AIGatewayImageAPIKey is non-empty, BuildServer writes a second
	// platform-wide ai-gateway credential for
	// aigateway.ProviderOpenAICompatibleImage at boot, so the smilesim
	// module's routes (smilesim.go) can actually reach an image
	// provider. The two credentials are deliberately independent rows of
	// the SAME ai_gateway_credentials table (keyed by provider name), so
	// chat and image credentials coexist with no schema change.
	// ConfigFromEnv fills
	// both from APP_AI_GATEWAY_IMAGE_BASE_URL/APP_AI_GATEWAY_IMAGE_API_KEY when the
	// API key variable is set (their doc comment carries the reasoning
	// and the non-secret shape of the e2e value); when both are unset, no
	// credential row is written, exactly like the chat pair above, and
	// flowtests/smilesim_flow_test.go is the other caller that sets both.
	AIGatewayImageBaseURL string
	AIGatewayImageAPIKey  string

	// WebhookURLValidator and WebhookHTTPClient override go/integration's
	// two SSRF enforcement points together (integration.
	// WithWebhookURLValidator's own doc comment explains why the two must
	// always move together) for ONE Module instance this BuildServer call
	// composes -- never a production weakening, since ConfigFromEnv never
	// sets either and BuildServer leaves both options unset (the module's
	// own strict default) whenever WebhookURLValidator is nil. This exists
	// for flowtests/webhook_flow_test.go alone: it is this app's only way to prove a
	// genuine signed HTTP delivery against a receiver it controls, since
	// neither an httptest.Server (loopback) nor a sibling Docker container
	// (RFC 1918 private space, exactly like every other Docker-backed
	// integration tier's own sibling containers in this repository) can
	// ever produce an address go/integration's production SSRF check is
	// willing to accept -- see integration.WithWebhookURLValidator's own
	// doc comment for the full argument. Mirrors AIGatewayBaseURL's and
	// cfg.Mailer's identical "test-only override of a seam production
	// leaves on its real default" shape above.
	WebhookURLValidator func(ctx context.Context, url string) error
	WebhookHTTPClient   *http.Client

	// OnRBACReady, when non-nil, receives the live *rbac.Service BuildServer
	// attaches, immediately after SeedDemoGrants seeds every configured
	// tenant's built-in roles and demo grants. It exists purely for a test
	// that needs to grant a role scoped to an organization node CREATED
	// AFTER the server starts serving HTTP -- the subtree-scoped grant
	// test (flowtests/org_route_guards_test.go), which cannot
	// know a node's id at boot time, since org builds its tree through real
	// HTTP calls the test itself drives once the server is up. Nil in every
	// production boot and every other test is a complete no-op, mirroring
	// WebhookURLValidator's identical "test-only override of what BuildServer
	// already wires, never a production weakening" shape above.
	OnRBACReady func(*rbac.Service)

	// OnConfigReady, when non-nil, receives the live *config.Service
	// BuildServer attaches, immediately after openConfiguredAuthnChannels
	// opens the assembled social channels' system-tier flag rows. It exists
	// purely for a test that must write a configuration row through the
	// module's real Set path (flowtests/authn_e2e_test.go's
	// TestAuthnE2E_PasswordChannelDisabled...), which
	// disables authn.password_login through the same system-tier write an
	// operator's admin-console write would land (under
	// config.SystemPurposeSystemWrite) and proves the composed stack then
	// refuses the password endpoint while SMS and social stay open. Nil in
	// every production boot and every other test is a complete no-op,
	// mirroring OnRBACReady's identical "test-only override of what
	// BuildServer already wires, never a production weakening" shape above.
	OnConfigReady func(*config.Service)
}

// serverBuild carries the assembly state the host's hooks hand each other:
// the resources built before the engine runs (the audit bus and its Redis
// client), the modules the WithModules callback constructs, the services and
// stores the attach hooks derive from them, and the background components
// the worker starts and drains.
type serverBuild struct {
	cfg ServerConfig

	// hostConfig and platformConfig are the engine's configuration targets:
	// the loader target whose shape the bootstrap-binding verification
	// checks, and the platform key material the engine builds its platform
	// cipher from (mapped from the resolved ServerConfig in newServerBuild).
	hostConfig     hostConfig
	platformConfig speedapp.PlatformConfig

	bus             pkgcore.EventBus
	busCapabilities pkgcore.Capability
	redisBus        *eventbusredis.EventBus
	redisClient     *redis.Client
	db              *gorm.DB

	configService             *config.Service
	rbacService               *rbac.Service
	standaloneQueue           *jobs.StandaloneQueue
	meteringModule            *metering.Module
	smileSimReconcilerStop    func()
	periodicTaskSchedulerStop func()

	orgIndexer          *dbkit.BlindIndexer
	contactEmailIndexer *dbkit.BlindIndexer
	contactPhoneIndexer *dbkit.BlindIndexer

	hostByTenant map[pkgcore.TenantID]string

	configModule        *config.Module
	orgModule           *org.Module
	pkiModule           *pki.Module
	authnModule         *authn.Module
	notesModule         *notes.Module
	auditModule         *audit.Module
	rbacModule          *rbac.Module
	storageModule       *storage.Module
	sharingModule       *sharing.Module
	integrationModule   *integration.Module
	demoModule          *demomodule.Module
	notificationModule  *notification.Module
	aiGatewayModule     *aigateway.Module
	billingModule       *billing.Module
	complianceModule    *compliance.Module
	adminModule         *admin.Module
	attestationService  *attestation.Service
	smileSimService     *smilesim.Service
	caseRepository      *cases.Repository
	authnUserLocales    demo.AuthnUserLocales
	gatewayEntitlements aigateway.EntitlementsFunc
	memberships         *signInMemberships

	reg *pkgcore.Registry
}

// newServerBuild returns the build state one BuildServer or Run call
// assembles from: the resolved configuration, the platform key material the
// engine's cipher is built from (mapped one-to-one onto the platform
// declaration, so the six key paths are never restated), and the app's own
// loader target -- the shape the engine's binding verification checks
// against the keys the composed modules declared.
func newServerBuild(cfg ServerConfig) *serverBuild {
	return &serverBuild{
		cfg:            cfg,
		platformConfig: platformKeyMaterial(cfg),
	}
}

// platformKeyMaterial maps the resolved ServerConfig's six key materials
// onto the engine's platform declaration. The host resolves the materials
// itself (ConfigFromEnv, or a test's own ServerConfig), so the declaration
// the engine consumes carries the resolved values; the engine's own
// configuration pass re-resolves the same environment over the same target
// shape, which in a real boot derives the identical material and in a test
// with no key environment leaves the mapped values standing.
func platformKeyMaterial(cfg ServerConfig) speedapp.PlatformConfig {
	return speedapp.PlatformConfig{
		Authn: speedapp.PlatformAuthnKeyMaterial{
			Blind_Index_Key: cfg.AuthnBlindIndexKey,
			PII_Cipher_Key:  cfg.AuthnPIICipherKey,
		},
		Config:       speedapp.PlatformConfigKeyMaterial{Cipher_Key: cfg.ConfigKey},
		Notification: speedapp.PlatformNotificationKeyMaterial{Contact_Index_Key: cfg.NotificationIndexKey},
		Org:          speedapp.PlatformOrgKeyMaterial{Invitation_Email_Index_Key: cfg.OrgIndexKey},
		PKI:          speedapp.PlatformPKIKeyMaterial{Local_Key_Cipher_Key: cfg.PKILocalKeyCipherKey},
	}
}

// BuildServer assembles the reference app through the application engine and
// returns the composed handler, a close function that tears the process
// down, and the wired *compliance.Module. It is the one place this app's
// wiring lives -- main.go's Run and the end-to-end suites (flowtests) all
// call it, so the two can never drift into testing a different wiring than
// the one that actually runs.
//
// The ServerConfig is mapped onto the engine's option set (options above):
// the engine loads the configuration targets, builds the platform cipher,
// runs the encrypted-column registrations, opens and migrates the database,
// constructs and bootstraps the module set, runs the host's typed attaches
// and wiring hooks, composes the HTTP face (this app's own protected face
// through chain.Standard) and starts the background worker -- in the one
// fixed order every speed application shares.
//
// The deployment mode itself refuses nothing here: the Kernel the engine
// bootstraps is what validates the assembled composition against
// cfg.DeploymentMode, failing startup with pkgcore's own capability error
// (ErrCapabilityUnsatisfied) when the resolved composition cannot run in
// the declared mode. Every stateful seam this app knows about -- eventbus,
// kv, mailer, objectstore, plus authn's own "SMS sender" seam -- can be
// pointed at a real, MultiReplicaSafe-capable implementation through the
// environment variables ConfigFromEnv resolves (APP_REDIS_ADDR, the APP_S3_*
// group, APP_OBJECT_STORE_ROOT, the APP_SMTP_* group, APP_SMS_GATEWAY_URL);
// every one of them defaults to the standalone Preset's in-process
// implementation when unset, so a plain `go run ./cmd/server` needs nothing
// else running.
//
// The compliance module is returned because a caller cannot reach it any
// other way: it exposes no accessor on the assembled application, and the
// flowtests' retention/erasure/export suites drive its services directly.
// The caller must call the returned close function once done with the
// handler; it is idempotent (the engine's Close runs its steps once).
func BuildServer(ctx context.Context, cfg ServerConfig) (http.Handler, func() error, *compliance.Module, error) {
	b := newServerBuild(cfg)
	a, err := speedapp.New(ctx, b.options()...)
	if err != nil {
		// The engine's rollback tears down everything the engine built;
		// the audit bus and, when Redis composes it, the client it runs on
		// were built before New and are this host's, so they close here.
		if closeErr := b.closeExternal(); closeErr != nil {
			obs.FromContext(ctx).Error("reference-app: closing the pre-assembly resources after a failed assembly failed", "error", closeErr)
		}
		return nil, nil, nil, err
	}
	return a.Handler(), func() error { return a.Close(context.Background()) }, b.complianceModule, nil
}

// Run assembles the reference app with BuildServer's option set and serves
// it until the process is signalled. Signal handling, the HTTP serve and the
// ordered drain are the engine's Run, and observability is initialized from
// the resolved ServerConfig before assembly (the OTLP endpoint when
// APP_OTLP_ENDPOINT is set) and shut down last during the drain.
func Run(ctx context.Context, cfg ServerConfig) error {
	b := newServerBuild(cfg)
	return speedapp.Run(ctx, append(b.options(), b.observabilityOption())...)
}

// httpSpec declares this app's HTTP face for the engine's stage 7: the
// protected-face composition (composeFace, which mounts the app's own
// routes and derives the middleware chain from the registry), the listen
// address Run binds, and the frontend directory when this boot serves one
// (cfg.WebDistDir, set by ConfigFromEnv from APP_WEB_DIST -- the Dockerfile
// ships the dist and sets the variable itself).
func (b *serverBuild) httpSpec() speedapp.HTTPSpec {
	spec := speedapp.HTTPSpec{
		Addr:    ":" + b.cfg.Port,
		Compose: b.composeFace,
	}
	if b.cfg.WebDistDir != "" {
		spec.SPA = webSPASpec(b.cfg.WebDistDir)
	}
	return spec
}

// observabilitySpec returns the observability declaration the engine's Run
// initializes before assembly: the service name every span and metric is
// tagged with, and the OTLP/gRPC endpoint ConfigFromEnv resolved
// (APP_OTLP_ENDPOINT). An empty endpoint is carried as-is -- the engine
// omits an empty option, which leaves go/observability's own local
// exporters in force, byte-identical to the endpoint never having been
// configured.
func observabilitySpec(cfg ServerConfig) speedapp.ObservabilitySpec {
	return speedapp.ObservabilitySpec{
		ServiceName:  "reference-app",
		OTLPEndpoint: cfg.OTLPEndpoint,
	}
}

// observabilityOption is the spec as the engine's option.
func (b *serverBuild) observabilityOption() speedapp.Option {
	return speedapp.WithObservability(observabilitySpec(b.cfg))
}

// options maps the resolved ServerConfig onto the engine's option set: the
// configuration targets and loader options, the database and its
// write-capture scope, the encrypted-column registrations, the module set,
// the kernel's seam composition, the HTTP face, the assembly hooks and the
// background worker. Everything host-specific the engine cannot know is
// named here; nothing is defaulted on the host's behalf.
func (b *serverBuild) options() []speedapp.Option {
	// The audit-capture bus exists before the engine opens the database:
	// dbkit's write-capture plugin publishes on this very bus, and the
	// identical instance is injected into the kernel below -- reg.EventBus()
	// must be the bus the capture plugin publishes on, or org's captured
	// writes would vanish into a bus audit's subscriptions never see.
	b.openBus()

	opts := []speedapp.Option{
		speedapp.WithConfig(
			speedapp.ConfigSpec{Host: &b.hostConfig, Platform: &b.platformConfig},
			speedapp.ConfigEnvPrefix(envPrefix),
			speedapp.ConfigRootKeyEnv(rootKeyEnv),
			speedapp.ConfigKeyDerivation(dbkit.DeriveBootstrapKey),
		),
		speedapp.WithDatabase(speedapp.DatabaseSpec{
			Dialect: dbkit.DialectSQLite,
			DSN:     b.cfg.SQLitePath,
			// Org's automatic write capture publishes on this bus; the
			// capture scope is org's own exported declaration and nothing
			// else (notes.Note stays out: its module records its own trail
			// through audit.Emit).
			AuditBus:    b.bus,
			AuditModels: org.AuditableModels(),
		}),
		speedapp.WithPreDB(b.registerEncryptedColumns),
		speedapp.WithModules(b.constructModules),
		speedapp.WithKernelOptions(b.kernelOptions()...),
		speedapp.WithHTTP(b.httpSpec()),
		speedapp.WithHooks(speedapp.Hooks{
			PostBootstrap: b.postBootstrap,
			PostAttach:    b.postAttach,
			PreServe:      b.preServe,
		}),
		speedapp.WithWorker(&backgroundWorker{b: b}),
	}
	if b.cfg.DisableQueueWorker {
		// The library-level form of this app's DisableQueueWorker switch:
		// the worker is registered (and still closed by the ordered
		// shutdown) but the queue's dispatcher and this replica's worker
		// goroutines never launch, so it can never claim or execute a Job.
		opts = append(opts, speedapp.WithoutBackgroundWorkers())
	}
	return opts
}

// openBus constructs the event bus this app runs on. With APP_REDIS_ADDR
// configured it is the real Redis-backed implementation over a go-redis
// client this host constructs and owns (the same client backs the "kv" seam
// when kernelOptions composes it -- one Redis instance backing both seams is
// this app's minimal-footprint choice); unset, it is pkgcore's in-process
// memory bus, the identical implementation (and identical zero capability
// declaration) the standalone Preset would otherwise resolve on its own.
// go-redis is imported here -- and go.mod therefore requires it directly --
// because the app is the assembly host that eventbus/redis's EventBus
// contract names as the client's owner.
func (b *serverBuild) openBus() {
	if b.cfg.RedisAddr == "" {
		b.bus = pkgcore.NewMemoryEventBus()
		return
	}
	b.redisClient = redis.NewClient(&redis.Options{Addr: b.cfg.RedisAddr})
	b.redisBus = eventbusredis.NewEventBus(b.redisClient)
	b.bus = b.redisBus
	b.busCapabilities = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
}

// closeExternal closes the resources built before the engine runs: the
// audit bus and, when Redis composes it, the client it was built on. On a
// failed assembly the engine has already rolled back everything it built
// and never saw these; on a successful one the background worker's Close
// owns them instead, in the same order.
func (b *serverBuild) closeExternal() error {
	var errs []error
	if b.redisBus != nil {
		b.redisBus.Close()
	}
	if b.redisClient != nil {
		if err := b.redisClient.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// backgroundWorker is this host's seat in the engine's lifecycle: Start
// launches the job queue and the periodic-task scheduler together (a task
// this replica can never execute is pointless to enqueue, so the scheduler
// runs exactly when the queue's worker does), and Close drains the process
// in the order the shared resources require.
type backgroundWorker struct{ b *serverBuild }

// Start launches the job queue's dispatcher and worker pool, then the
// periodic-task scheduler over the registry's declared schedules. The
// tenant universe every per-tenant declaration expands through is the
// configured host tenants joined with go/admin's tenant ledger
// (periodic_scheduler.go's periodicTenantUniverse).
func (w *backgroundWorker) Start(ctx context.Context) error {
	b := w.b
	if err := b.standaloneQueue.Start(ctx); err != nil {
		return fmt.Errorf("reference-app: start the job queue: %w", err)
	}
	schedulerOpts := []jobs.SchedulerOption{
		jobs.WithSchedules(b.reg.Schedules),
		jobs.WithTenantLister(newPeriodicTenantUniverse(b.cfg.HostTenants, b.adminModule.Tenants())),
	}
	if b.cfg.PeriodicTaskInterval > 0 {
		schedulerOpts = append(schedulerOpts, jobs.WithInterval(b.cfg.PeriodicTaskInterval))
	}
	scheduler := jobs.NewScheduler(b.standaloneQueue, schedulerOpts...)
	// context.Background(), never ctx, per jobs.Scheduler.Start's own doc
	// comment: the enqueues must keep running until Close's own scheduler
	// stop, not be cut short by whatever cancels the assembly context.
	if err := scheduler.Start(context.Background()); err != nil {
		return fmt.Errorf("reference-app: start the periodic-task scheduler: %w", err)
	}
	b.periodicTaskSchedulerStop = scheduler.Stop
	return nil
}

// Close tears the process's shared resources down in the order their
// dependencies require: the credit-reservation reconciler and the
// scheduler first (both tick against the queue and the database), then the
// attached services whose background loops write the database, then the
// queue the engine registered as the worker (stopping its dispatcher and
// waiting for in-flight jobs), then metering's pipelines (draining what the
// recorder buffered into the aggregator), then the event bus (so no remote
// event can still be delivered to a handler writing a closing database) and
// the Redis client the host owns. The database itself closes last, in the
// engine's own order after this step. Every step is attempted even when an
// earlier one failed; the first error wins.
func (w *backgroundWorker) Close(ctx context.Context) error {
	b := w.b
	var firstErr error
	keepErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if b.smileSimReconcilerStop != nil {
		b.smileSimReconcilerStop()
	}
	if b.periodicTaskSchedulerStop != nil {
		b.periodicTaskSchedulerStop()
	}
	if b.configService != nil {
		keepErr(b.configService.Close())
	}
	if b.rbacService != nil {
		keepErr(b.rbacService.Close())
	}
	if b.standaloneQueue != nil {
		// Close is idempotent, so a failed assembly that runs before Start
		// ever ran is safe.
		keepErr(b.standaloneQueue.Close(ctx))
	}
	if b.meteringModule != nil {
		// Stops both of metering's background pipelines -- the analytics
		// recorder's flush loop and the dispatcher's outbox poll -- and
		// delivers whatever the recorder still had buffered into the
		// aggregator before returning. Safe to call before Start, or more
		// than once.
		b.meteringModule.Stop()
	}
	if b.redisBus != nil {
		b.redisBus.Close()
	}
	if b.redisClient != nil {
		keepErr(b.redisClient.Close())
	}
	return firstErr
}
