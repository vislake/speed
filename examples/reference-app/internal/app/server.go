// This file is the reference app's assembly core: ServerConfig, the
// component assembly the composed server is driven through, and the host's
// two entry points. It sits in internal/app so the composed server is
// importable: cmd/server's main.go boots it as the thin process shell, and
// the assembly-flow suites, which live beside the command in cmd/server,
// exercise the package through the command's tests. See doc.go for the
// package's design.

package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

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
	obs "github.com/vislake/speed/go/observability"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the host's dbkit.Open call has a driver to build from -- the
	// reference app runs its own database in standalone deployment mode's
	// SQLite dialect regardless of which deployment mode its other
	// infrastructure seams compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"

	// Blank-imported for its init() side effect: obs.Init's local
	// exporters wire a real /metrics scrape endpoint only when a local
	// metrics reader has been registered (go/observability's own doc
	// comment on Init and RegisterLocalMetricsReader) -- this is what
	// obs.MountLiveness's metrics route actually serves once the
	// observability component's Prepare has run obs.Init. Without this
	// import, obs.Init still runs (traces and metrics both go to stdout),
	// but the metrics route answers 404.
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"

	// Blank-imported for its init() side effect: registers the OTLP/gRPC
	// exporter factory obs.Init consults exactly when a non-empty OTLP
	// endpoint is configured (go/observability's ErrOTLPExporterNotRegistered
	// names this import as the fix). Without it, an APP_OTLP_ENDPOINT set
	// in ConfigFromEnv would fail the observability component's Prepare with
	// that error instead of wiring the collector push the variable promises.
	// Registration is inert while the endpoint stays unset: Init stays on
	// the local exporters, byte-identical to this import never having
	// existed.
	_ "github.com/vislake/speed/go/observability/exporter/otlp"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"

	// Blank-imported for their init() side effects: each registers its
	// distributed component ("eventbus.redis", "kv.redis") with pkgcore's
	// global component registration, and the APP_REDIS_ADDR composition
	// selects both. Without them a Redis-configured boot would fail the
	// assembly with ErrUnknownComponent -- the database/sql driver trade
	// every registered built-in makes.
	_ "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	_ "github.com/vislake/speed/go/pkgcore/kv/redis"

	// Blank-imported for its init() side effect: registers the
	// "objectstore.s3" component, the one the APP_S3_* composition
	// selects for the "objectstore" seam. Without this import a boot with
	// APP_S3_ENDPOINT set would fail the assembly with
	// ErrUnknownComponent -- the database/sql driver trade every
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
// mode, port, database path, the optional Redis address, the demo host
// map). It is a plain struct resolved from the process environment by
// ConfigFromEnv's loader-driven bootstrap (bootstrap.go), NOT the dynamic
// configuration the config module serves: dynamic configuration lives in
// the configs table and can never hold the very key that encrypts it, so
// this bootstrap struct is the deliberate exception to "a plain struct, not
// pkgcore/config's dynamic configuration" -- it is main.go's own wiring,
// which never goes through Module.Register either. The six platform key
// materials are not here: each declaring module's component carries its
// key's declaration, and the assembly's loader resolves them into the
// bootstrap material the wiring reads.
type ServerConfig struct {
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string

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
	// implementation for the "objectstore" seam through the builtin composition's
	// config channel when S3Endpoint is non-empty: BuildServer overrides
	// that one entry of the builtin standalone composition with these values, and the
	// registration builds the S3-compatible ObjectStore from them -- the
	// host pre-builds nothing. The S3 fields' own doc comment
	// (bootstrap.go) has the completeness rule; an empty S3BucketLookup
	// reads as the store's endpoint-derived auto default.
	// Empty S3Endpoint (the default) leaves "objectstore" on the builtin composition's
	// local-directory default.
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3Region       string
	S3UseSSL       bool
	S3BucketLookup string

	// ObjectStoreRoot, when non-empty, replaces the "objectstore" seam's
	// builtin default with pkgcore.NewLocalObjectStore over this fixed
	// directory -- the local twin of the S3 fields above, and an
	// alternative to them (ConfigFromEnv refuses both a complete S3
	// composition and APP_OBJECT_STORE_ROOT, per the ObjectStoreRoot field's
	// own doc comment (bootstrap.go)). BuildServer injects it declaring the
	// SurvivesRestart
	// capability alone, never MultiReplicaSafe: the directory survives a
	// process restart, but nothing about a single-process local store is
	// replica-safe. The field exists because a host that needs its objects
	// to outlive one process must name the directory itself -- the builtin
	// default is a throwaway MkdirTemp -- which is exactly what the
	// two-boot expiry-sweep flow test (flowtests/periodic_scheduler_flow_test.go)
	// needs: boot 2's sweep must find the bytes boot 1 wrote.
	ObjectStoreRoot string

	// SMTPHost, SMTPPort, SMTPUsername, SMTPPassword and SMTPReplyTo name the
	// registered "mailer.smtp" implementation for the "mailer" seam through
	// the builtin composition's
	// config channel when SMTPHost is non-empty: BuildServer overrides that
	// one entry of the builtin standalone composition with these values, and the
	// registration builds the SMTP Mailer from them -- the host pre-builds
	// nothing. The SMTP fields' own doc comment (bootstrap.go) has the
	// completeness rule.
	// Empty SMTPHost (the default) leaves "mailer" on the builtin composition's
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
	// -- nil in production, where the seam is composed through the builtin composition's
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

// serverBuild carries the assembly state the host's components hand each
// other: the resolved configuration, the platform cipher and blind indexers
// the crypto component prepares, the tenant indexes the host's own steps
// read, and the runtime values the step bodies read back from the
// registry's by-type context (bindRegistry).
type serverBuild struct {
	cfg ServerConfig

	// hostConfig is the engine's configuration target: the loader target
	// whose shape the module components' declared bootstrap keys bind
	// against, with the platform key materials pre-filled from the resolved
	// ServerConfig (newServerBuild) so the loader's own pass keeps them
	// standing when no key environment supplies material (the loader only
	// writes what a source actually supplied).
	hostConfig hostConfig

	// platformCipher is the cipher the crypto component's Prepare builds
	// from the config.cipher_key material and its New provides.
	platformCipher *dbkit.Cipher

	db *gorm.DB

	configService          *config.Service
	rbacService            *rbac.Service
	standaloneQueue        jobs.Queue
	meteringModule         *metering.Module
	smileSimReconcilerStop func()

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

	// catalogOnce guards catalog and catalogErr, the boot's one merged
	// message catalog: the first view derives it from the plan's locale
	// assets and every later view shares the same instance.
	catalogOnce sync.Once
	catalog     *i18n.Catalog
	catalogErr  error
}

// newServerBuild returns the build state one BuildServer or Run call
// assembles from: the resolved configuration and a zero host target, which
// the assembly's loader fills with the host's own keys.
func newServerBuild(cfg ServerConfig) *serverBuild {
	return &serverBuild{cfg: cfg}
}

// BuildServer assembles the reference app through the component assembly and
// returns the composed handler, a close function that tears the process
// down, and the wired *compliance.Module. It is the one place this app's
// wiring lives -- main.go's Run and the end-to-end suites (flowtests) all
// call it, so the two can never drift into testing a different wiring than
// the one that actually runs.
//
// The assembly is the component registry's: the loader resolves the
// configuration targets and the composition configuration, and the registry
// walks its seven stages -- Prepare (the bootstrap material, the ciphers and
// the column registrations), Construct (every component's product), Verify
// (the assembled migration sets), Init (every declaration, plus the host's
// assembly steps and the composed HTTP face), and Start. BuildServer's drive
// starts no listener: the returned handler is served by its caller, which is
// what lets an httptest.Server front the exact composed chain.
//
// The deployment mode is validated by the assembly itself, against the
// capabilities every selected component declares: a composition that cannot
// run in the declared mode fails the Prepare stage with pkgcore's own
// capability error (ErrCapabilityUnsatisfied). Every stateful seam this app
// knows about -- eventbus, kv, mailer, objectstore, plus the "sms" seam --
// is selected from the environment variables ConfigFromEnv resolves
// (APP_REDIS_ADDR, the APP_S3_* group, APP_OBJECT_STORE_ROOT, the APP_SMTP_*
// group, APP_SMS_GATEWAY_URL); every one of them defaults to the in-process
// implementation when unset, so a plain `go run ./cmd/server` needs nothing
// else running.
//
// The compliance module is returned because a caller cannot reach it any
// other way: it exposes no accessor on the composed handler, and the
// flowtests' retention/erasure/export suites drive its services directly.
// The caller must call the returned close function once done with the
// handler; it is idempotent (the assembly's Close runs its steps once).
func BuildServer(ctx context.Context, cfg ServerConfig) (http.Handler, func() error, *compliance.Module, error) {
	b := newServerBuild(cfg)
	reg, err := b.assemble(ctx, false)
	if err != nil {
		// The assembly's own rollback already tore the process down --
		// every constructed component closed in reverse order, exactly
		// once -- so no host-side teardown is needed here.
		return nil, nil, nil, err
	}
	face, err := pkgcore.Get[*hostFace](reg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: read the composed HTTP face: %w", err)
	}
	complianceModule, err := pkgcore.Get[*compliance.Module](reg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: read the compliance module: %w", err)
	}
	return face.Handler(), func() error { return speedapp.Shutdown(context.Background(), reg) }, complianceModule, nil
}

// Run assembles the reference app with BuildServer's composition and serves
// it until the process is signalled: the app component's Start binds the
// listener, the signal-derived context is what the serve waits on, and the
// two-phase shutdown (the Stop notification, then the drain and release)
// runs once the context is done.
func Run(ctx context.Context, cfg ServerConfig) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b := newServerBuild(cfg)
	reg, err := b.assemble(ctx, true)
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
// the whole assembly: the host's own components (the step components, the
// override components and the provider components) register first, the
// loader resolves the configuration and the composition, and the registry
// walks Prepare through Start. live tells the drive whether this assembly
// owns the listener -- true for Run, whose app component binds it, false for
// BuildServer, whose caller serves the returned handler.
func (b *serverBuild) assemble(ctx context.Context, live bool) (*pkgcore.ComponentRegistry, error) {
	// The write-capture scope and the composed face both read runtime state
	// the host keeps across components: the tenant reverse index and the
	// sign-in membership store are resolved here, before any component
	// callback runs.
	b.hostByTenant = make(map[pkgcore.TenantID]string, len(b.cfg.HostTenants))
	for host, tenant := range b.cfg.HostTenants {
		b.hostByTenant[tenant] = host
	}
	b.memberships = b.cfg.Memberships
	if b.memberships == nil {
		b.memberships = NewSignInMemberships()
	}

	reg := pkgcore.NewComponentRegistry()
	components, err := b.hostComponents(ctx, reg, live)
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
		Options:   declaredKeyOptions(),
		Overrides: &speedapp.CompositionOverrides{Config: b.composition(live)},
	}
	if err := speedapp.Assemble(ctx, reg, spec); err != nil {
		return nil, err
	}
	return reg, nil
}

// declaredKeyOptions returns the loader options the assembly's resolution
// runs with: the APP_ prefix the declared key paths' variables are derived
// under, the root key APP_ROOT_KEY feeds the derivation with, the platform
// derivation composition, and this app's documented development defaults as
// the lowest-priority table. The declared keys themselves come from the
// composed modules' components -- this app declares none on its own.
func declaredKeyOptions() []speedapp.ConfigOption {
	return []speedapp.ConfigOption{
		speedapp.ConfigEnvPrefix(envPrefix),
		speedapp.ConfigRootKeyEnv(rootKeyEnv),
		speedapp.ConfigKeyDerivation(dbkit.DeriveBootstrapKey),
		speedapp.ConfigDevDefaults(BootstrapDevDefaults()),
	}
}

// composition returns this host's code-override layer: the composition
// configuration's highest source, carrying the components this app selects
// -- every module, the infrastructure implementations the resolved
// ServerConfig names, and the host's own components -- and deselecting the
// built-in implementations the host's overrides stand in for.
func (b *serverBuild) composition(live bool) pkgcore.ComponentConfig {
	// The infrastructure implementations lead the block: the mode's
	// capability validation walks the selection in this order, so the
	// shortfall a composition that cannot run in the declared mode reports
	// is the seam implementation that first fails to satisfy it.
	components := pkgcore.ComponentConfig{}.
		With("eventbus.memory", nil).
		With("kv.memory", nil).
		With("mailer.console", nil).
		With("objectstore.local", nil).
		With("sms.console", nil).
		With("db.sqlite", false).
		With(hostComponentPrefix+"db", nil).
		With("queue.standalone", pkgcore.ComponentConfig{}.
			With("worker", !b.cfg.DisableQueueWorker).
			With("schedule_interval", b.cfg.PeriodicTaskInterval)).
		With("pki", nil).
		With("signer.local", false).
		With(hostComponentPrefix+"signer.local", nil).
		With("authn", false).
		With(hostComponentPrefix+"authn", nil).
		With("org", false).
		With(hostComponentPrefix+"org", nil).
		With("config", nil).
		With("storage", nil).
		With("sharing", nil).
		With("integration", false).
		With(hostComponentPrefix+"integration", nil).
		With("demo", nil).
		With("notification", false).
		With(hostComponentPrefix+"notification", nil).
		With("ai-gateway", false).
		With(hostComponentPrefix+"ai-gateway", nil).
		With("billing", nil).
		With("metering", nil).
		With("compliance", nil).
		With("audit", nil).
		With("notes", nil).
		With("cases", nil).
		With("smilesim", nil).
		// rbac is the one host override left among these: its config
		// attach must run in the post-bootstrap step, because this host's
		// Init-stage consumers (the demo seeds, the self-service chain)
		// need the complete permission catalog while they run. config,
		// admin and observability are selected by their own descriptors --
		// config's Start completes its schema snapshot, admin's Start
		// binds the rbac Service, and both declare their capabilities
		// themselves.
		With("rbac", false).
		With(hostComponentPrefix+"rbac", nil).
		With("admin", nil).
		With(hostComponentPrefix+"crypto", nil).
		With(hostComponentPrefix+"tenancy-resolver", nil).
		With(hostComponentPrefix+"subject-resolvers", nil).
		With(hostComponentPrefix+"rbac-subtree", nil).
		With(hostComponentPrefix+"notification-addresses", nil).
		With(hostComponentPrefix+"notification-locales", nil).
		With(hostComponentPrefix+"sharing-resources", nil).
		With(hostComponentPrefix+"sharing-expiry", nil).
		With(hostComponentPrefix+"integration-permissions", nil).
		With(hostComponentPrefix+"integration-membership", nil).
		With(hostComponentPrefix+"gateway-entitlements", nil).
		With(hostComponentPrefix+"gateway-usage", nil).
		With(hostComponentPrefix+"compliance-sharing", nil).
		With(hostComponentPrefix+"attestation", nil).
		With(hostComponentPrefix+"tenant-lister", nil).
		// The host's own assembly steps come last: they run after every
		// module's declaration turn (the module components are listed
		// above), and their plan order is this block's order.
		With(hostComponentPrefix+"post_bootstrap", nil).
		With(hostComponentPrefix+"post_attach", nil).
		With(hostComponentPrefix+"app", nil).
		With(hostComponentPrefix+"pre_serve", nil).
		With(hostComponentPrefix+"worker", nil)

	// The infrastructure seams follow the resolved ServerConfig, one
	// selected implementation per seam: an unset variable leaves that seam
	// on its in-process default, so a plain `go run ./cmd/server` needs
	// nothing else running.
	if b.cfg.RedisAddr != "" {
		components = components.
			With("eventbus.memory", false).
			With("eventbus.redis", pkgcore.ComponentConfig{}.With("addr", b.cfg.RedisAddr)).
			With("kv.memory", false).
			With("kv.redis", pkgcore.ComponentConfig{}.With("addr", b.cfg.RedisAddr))
	} else {
		components = components.
			With("eventbus.memory", nil).
			With("kv.memory", nil)
	}
	switch {
	case b.cfg.Mailer != nil:
		// The host's own in-process capture double stands in for a
		// registered mailer implementation.
		components = components.With("mailer.console", false).With(hostComponentPrefix+"mailer", nil)
	case b.cfg.SMTPHost != "":
		components = components.With("mailer.console", false).With("mailer.smtp", pkgcore.ComponentConfig{}.
			With("host", b.cfg.SMTPHost).
			With("port", b.cfg.SMTPPort).
			With("username", b.cfg.SMTPUsername).
			With("password", b.cfg.SMTPPassword).
			With("reply_to", b.cfg.SMTPReplyTo))
	default:
		components = components.With("mailer.console", nil)
	}
	switch {
	case b.cfg.S3Endpoint != "":
		components = components.With("objectstore.local", false).With("objectstore.s3", s3ObjectStoreConfig(b.cfg))
	case b.cfg.ObjectStoreRoot != "":
		components = components.With("objectstore.local", pkgcore.ComponentConfig{}.With("directory", b.cfg.ObjectStoreRoot))
	default:
		components = components.With("objectstore.local", nil)
	}
	// The pki timing knobs are the composition's configuration, applied
	// only when a test injects them: a zero value (what ConfigFromEnv
	// always leaves them at) keeps pki's own defaults in force.
	pkiConfig := pkgcore.ComponentConfig{}
	if b.cfg.PKIPropagationWindow > 0 {
		pkiConfig = pkiConfig.With("propagation_window", b.cfg.PKIPropagationWindow)
	}
	if b.cfg.PKIRenewalLeadTime > 0 {
		pkiConfig = pkiConfig.With("renewal_lead_time", b.cfg.PKIRenewalLeadTime)
	}
	if b.cfg.PKIExpiryScanWindow > 0 {
		pkiConfig = pkiConfig.With("expiry_scan_window", b.cfg.PKIExpiryScanWindow)
	}
	components = components.With("pki", pkiConfig)

	switch {
	case b.cfg.SMSGatewayURL != "":
		components = components.With("sms.console", false).With("sms.http", pkgcore.ComponentConfig{}.With("endpoint", b.cfg.SMSGatewayURL))
	case b.cfg.SMSOutput != nil:
		components = components.With("sms.console", false).With(hostComponentPrefix+"sms", nil)
	default:
		// The console sender's standard output. A distributed deployment
		// with neither a gateway URL nor a host sender selects it too, and
		// the authn component refuses the boot on its own construction --
		// authn's WithDeploymentMode validation is what rejects a
		// distributed deployment whose SMS transport nobody in a replica
		// pool reads (ErrMissingDistributedSMSSender), so the refusal names
		// the module that owns the rule.
		components = components.With("sms.console", nil)
	}
	// The engine's observability component, selected with this app's
	// telemetry configuration (the component itself is the engine's).
	if live {
		observability := pkgcore.ComponentConfig{}.With("service_name", "reference-app")
		if b.cfg.OTLPEndpoint != "" {
			observability = observability.With("otlp_endpoint", b.cfg.OTLPEndpoint)
		}
		components = components.With("observability", observability)
	} else {
		// BuildServer serves the returned handler in-process; the telemetry
		// lifecycle belongs to the process that owns the listener.
		components = components.With("observability", false)
	}

	return pkgcore.ComponentConfig{}.
		With("deployment", string(b.cfg.DeploymentMode)).
		With("strict", true).
		With("components", components)
}

// bindRegistry reads the assembled products and published runtime services
// this host's step bodies work from into the build state, so one step body
// serves the component drive with the values the registry resolved. Every
// read happens before the step body runs, and a missing value fails the step
// by name rather than surfacing as a nil dereference inside it.
func (b *serverBuild) bindRegistry(reg *pkgcore.ComponentRegistry) error {
	var err error
	if b.db, err = pkgcore.Get[*gorm.DB](reg); err != nil {
		return fmt.Errorf("reference-app: read the assembled database: %w", err)
	}
	if b.standaloneQueue, err = pkgcore.Get[jobs.Queue](reg); err != nil {
		return fmt.Errorf("reference-app: read the assembled job queue: %w", err)
	}
	if b.configModule, err = pkgcore.Get[*config.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the config module: %w", err)
	}
	if b.orgModule, err = pkgcore.Get[*org.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the org module: %w", err)
	}
	if b.pkiModule, err = pkgcore.Get[*pki.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the pki module: %w", err)
	}
	if b.authnModule, err = pkgcore.Get[*authn.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the authn module: %w", err)
	}
	if b.notesModule, err = pkgcore.Get[*notes.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the notes module: %w", err)
	}
	if b.auditModule, err = pkgcore.Get[*audit.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the audit module: %w", err)
	}
	if b.rbacModule, err = pkgcore.Get[*rbac.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the rbac module: %w", err)
	}
	if b.storageModule, err = pkgcore.Get[*storage.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the storage module: %w", err)
	}
	if b.sharingModule, err = pkgcore.Get[*sharing.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the sharing module: %w", err)
	}
	if b.integrationModule, err = pkgcore.Get[*integration.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the integration module: %w", err)
	}
	if b.demoModule, err = pkgcore.Get[*demomodule.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the demo module: %w", err)
	}
	if b.notificationModule, err = pkgcore.Get[*notification.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the notification module: %w", err)
	}
	if b.aiGatewayModule, err = pkgcore.Get[*aigateway.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the ai-gateway module: %w", err)
	}
	if b.billingModule, err = pkgcore.Get[*billing.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the billing module: %w", err)
	}
	if b.meteringModule, err = pkgcore.Get[*metering.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the metering module: %w", err)
	}
	if b.complianceModule, err = pkgcore.Get[*compliance.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the compliance module: %w", err)
	}
	if b.adminModule, err = pkgcore.Get[*admin.Module](reg); err != nil {
		return fmt.Errorf("reference-app: read the admin module: %w", err)
	}
	if b.attestationService, err = pkgcore.Get[*attestation.Service](reg); err != nil {
		return fmt.Errorf("reference-app: read the attestation service: %w", err)
	}
	return nil
}

// bindRuntimeServices reads the two runtime services the post-bootstrap
// step publishes (config's schema-bearing service and rbac's
// catalog-bearing one) into the build state. It runs for every step after
// the attach step; the attach step itself binds them as it publishes them.
func (b *serverBuild) bindRuntimeServices(reg *pkgcore.ComponentRegistry) error {
	var err error
	if b.configService, err = pkgcore.Get[*config.Service](reg); err != nil {
		return fmt.Errorf("reference-app: read the config service: %w", err)
	}
	if b.rbacService, err = pkgcore.Get[*rbac.Service](reg); err != nil {
		return fmt.Errorf("reference-app: read the rbac service: %w", err)
	}
	return nil
}
