package app

// bootstrap.go owns this app's bootstrap surface: the process-start input a
// boot resolves before anything else is wired, and the transform that turns the
// resolved values into the ServerConfig the assembly consumes.
//
// The surface is resolved by go/pkgcore/config's loader, never by direct
// environment reads. hostConfig is the loader target: its config tags pin each
// variable's exact name (the app's names are flat and single-underscored, a
// spelling the loader's nesting derivation never produces), its field defaults
// are the loader's lowest-priority source, and loadHostConfig drives
// config.New(config.WithEnvPrefix("APP_")) over the five-source chain -- flags,
// the environment, an optional config file (none is wired here), the six key
// materials' derivation from APP_ROOT_KEY, then those defaults. The six
// key-material fields are typed []byte and tagged derive, so the loader owns
// their whole resolution: an explicit 64-hex-character value wins, the
// configured root key's derivation stands next, and the documented development
// default (a Dev* constant) is what remains -- this app carries no key
// precedence or hex-decoding logic of its own. serverConfigFrom then turns the
// loaded surface into ServerConfig: parsing the deployment mode, splitting the
// proxy list and the cross-variable refusals are the shapes the loader's
// text decoding deliberately does not carry. No os.Getenv call survives in this
// app's executable code.
//
// The struct doubles as the surface's documentation: each field states what its
// variable configures, its unset fallback, and the refusals a boot enforces.
// verifyBootstrapBinding then proves at every boot that the target really binds
// the bootstrap keys this app's composition declares on the registry's
// bootstrap seat, and the keys this app owns besides them.

import (
	"fmt"
	"strings"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/config"
)

// envPrefix is the prefix every one of this app's own bootstrap variables
// carries (its own naming convention; the loader's default is SPEED_). PORT is
// the one exception, pinned by name because it is the unprefixed variable
// hosting platforms inject. APP_ROOT_KEY is the other: it is read by the
// loader's WithRootKeyEnv exactly as spelled, not through this prefix.
const envPrefix = "APP_"

// hostConfig is the loader target carrying this app's whole bootstrap surface,
// one field per variable. Every field pins its variable's exact name with the
// env tag option, so the names are read as spelled here whatever the loader's
// prefix derivation would make of the field's key.
//
// Field types are the loader's own decoding types: a string field holds its
// variable's text verbatim (an emptied variable arrives as "", which the
// transform reads as "unset" exactly as a direct read of an empty variable
// would), an int or bool field holds the parsed value, and an emptied variable
// behind either is a load-time refusal rather than a silent zero (the SMTPPort,
// ReadFlyClientIP, S3UseSSL and FailSelfServiceProvision fields state that rule
// for their own variables). The two existence switches
// (DisableDemoUserHeader, DisableQueueWorker) are deliberately strings: their
// contract is "any non-empty value turns this off", not bool parsing, and an
// emptied variable must keep meaning "off" for them. The six key-material
// fields inside the *Key groups are typed []byte and tagged derive, and hold
// the 32-byte materials the loader resolved (an explicit hex value, the
// APP_ROOT_KEY derivation, or the Dev* struct default); APP_ROOT_KEY itself is
// not a field at all -- it is consumed inside the same Load, by the
// WithRootKeyEnv option loadHostConfig wires.
type hostConfig struct {
	// DeploymentMode names APP_DEPLOYMENT_MODE: the deployment topology the
	// process runs as -- "standalone" (the default) or "distributed". It is a
	// topology declaration, never an implementation choice: the mode only
	// constrains which seam implementations may compose (a distributed boot
	// requires MultiReplicaSafe implementations of the stateful seams, which
	// the optional seam variables below compose). An unset or emptied
	// variable reads as "standalone"; anything else that is not a known mode
	// refuses the boot, naming the parse failure.
	DeploymentMode string `config:"env=APP_DEPLOYMENT_MODE"`

	// Port names PORT -- unprefixed on purpose, the variable hosting platforms
	// (Fly.io among them) inject: a pinned field reads exactly this name, which
	// no prefix-derived spelling of the key "port" could reach. It is the HTTP
	// listen port the server binds, and the fallback host of the derived public
	// origin below. Unset or emptied reads as DefaultPort.
	Port string `config:"env=PORT"`

	// DBPath names APP_DB_PATH: the SQLite database file the standalone
	// deployment opens, relative to the working directory unless absolute.
	// Unset or emptied reads as DefaultSQLitePath, so `go run ./cmd/server`
	// starts on a fresh local file with zero setup.
	DBPath string `config:"env=APP_DB_PATH"`

	// RedisAddr names APP_REDIS_ADDR: the Redis server address ("host:port")
	// the injected EventBus AND KVStore connect to -- one Redis instance backs
	// both seams, sharing one *redis.Client. Empty -- the default -- leaves
	// both seams on the in-process implementations the Preset resolves, so
	// zero-setup standalone development keeps working with nothing else
	// running; set it to compose real Redis-backed implementations into the
	// SAME standalone deployment mode (a deployment mode constrains which
	// implementations may compose, never selects one), or into a distributed
	// deployment mode, where MultiReplicaSafe is required of both seams.
	RedisAddr string `config:"env=APP_REDIS_ADDR"`

	// OTLPEndpoint names APP_OTLP_ENDPOINT: the OTLP/gRPC endpoint
	// ("host:port", the syntax go/observability's own Config.OTLPEndpoint doc
	// comment describes) traces and metrics are pushed to. Empty -- the
	// default -- leaves obs.Init on the local exporters (stdout traces/metrics
	// plus the /metrics scrape endpoint), so zero-setup standalone development
	// keeps working with nothing running; set it to push both signals at a
	// collector over OTLP. It is an implementation-composition question, never
	// a deployment-mode one: obs.Init takes no mode, and the exporter set the
	// endpoint selects works identically in the standalone and distributed
	// topologies this app boots under.
	OTLPEndpoint string `config:"env=APP_OTLP_ENDPOINT"`

	// PublicOrigin names APP_PUBLIC_ORIGIN: this deployment's own public origin
	// ("https://app.example.com" -- scheme, host and port, no path), the base
	// URL the outbound mail links this app renders point recipients at when the
	// recipient's tenant has no branded host of its own in cfg.HostTenants. The
	// branded hosts cfg.HostTenants names are demo-only (DemoHostTenants: the
	// two configured tenants), while every other tenant this app serves is a
	// self-registered clinic its own register route provisions at runtime
	// (self_service.go's ClinicTenantOf: "tenant-" + the registrant's user id,
	// by construction never a cfg.HostTenants value) -- and org's invitation
	// email is the one outbound message whose link needs a host (BuildServer's
	// org.WithInvitationLinkBuilder wiring). Unset or emptied derives
	// "http://localhost:" + the resolved PORT, the origin every zero-setup
	// local demo is actually reached at. That default is silently wrong for a
	// real deployment, not inert the way the unset SMTP and SMS variables are:
	// those leave their seams on console transports a local demo alone reads (a
	// distributed boot refuses them outright), so nothing wrongly-shaped leaves
	// the process, while an unset APP_PUBLIC_ORIGIN does not stop the mail --
	// it goes out over whatever transport the deployment did compose, with
	// every link pointing at http://localhost:PORT, a host no real recipient
	// can reach. A deployment whose mail must reach real recipients therefore
	// sets this variable to its own public origin; forgetting it ships mail
	// whose links are unreachable.
	PublicOrigin string `config:"env=APP_PUBLIC_ORIGIN"`

	// WebDist names APP_WEB_DIST: the directory holding this app's built
	// frontend (the dist/ examples/reference-app/web's `pnpm build` emits) when
	// this process should serve that frontend itself -- see frontend.go's own
	// package doc comment for this app's wiring and pkgcore/spa's for the
	// full serving contract. Empty -- the default,
	// and what every caller of BuildServer without a frontend gets -- leaves
	// the composed handler serving no static files at all.
	WebDist string `config:"env=APP_WEB_DIST"`

	// TrustedProxies names APP_TRUSTED_PROXIES: the comma-separated list of
	// reverse-proxy addresses this deployment receives requests through, the
	// value the transform hands authn.WithTrustedProxies (see
	// ServerConfig.TrustedProxies). It is the deployment declaration that lets
	// authn's session/login-history records carry the REAL client address
	// instead of the proxy's: on Fly.io, declaring the Fly proxy ranges in
	// fly.toml's [env] block is what recovers the client from the Fly-Client-IP
	// header Fly's proxy overwrites on every request. Empty -- the default --
	// keeps every request recording its direct connection address, which is the
	// correct fail-closed shape for a host not behind a proxy, since a host
	// that reads forwarding headers from an undeclared peer would let any
	// direct client mint its own recorded address. The transform splits the
	// list on commas, trims the entries and drops empties; an entry that is
	// neither an IP address nor a CIDR prefix refuses the boot in
	// authn.WithTrustedProxies' own validation, naming the entry.
	TrustedProxies string `config:"env=APP_TRUSTED_PROXIES"`

	// ReadFlyClientIP names APP_READ_FLY_CLIENT_IP: this deployment's
	// declaration that its proxy is FLY's -- the one platform whose proxy
	// genuinely overwrites the Fly-Client-IP header on every request it
	// forwards -- which is what authorizes authn to read that single-hop vendor
	// header at all (go/authn's WithVendorClientIPHeaders and
	// VendorClientIPHeaderFlyClientIP; see ServerConfig.ReadFlyClientIP).
	// APP_TRUSTED_PROXIES alone can never authorize it: a generic reverse proxy
	// (nginx, ALB, Envoy, Cloudflare) forwards a client-chosen Fly-Client-IP
	// verbatim, so reading the header for every declared proxy would let a
	// client mint its own recorded AND rate-limited address -- the smuggling
	// hole this declaration pair exists to close. It is a strict bool, parsed
	// by the loader (strconv.ParseBool's set); unset is false, and an emptied
	// variable refuses the load rather than reading as false. It only takes
	// effect alongside APP_TRUSTED_PROXIES: the transform refuses 'true' with
	// an empty proxy list, since that combination would silently keep recording
	// the proxy itself -- the very defect the declaration pair exists to fix.
	ReadFlyClientIP bool `config:"env=APP_READ_FLY_CLIENT_IP"`

	// S3Endpoint, S3Bucket, S3AccessKey and S3SecretKey name
	// APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY: the
	// four variables that together point the "objectstore" seam at the
	// registered "objectstore.s3" implementation -- BuildServer overrides
	// that one preset entry with these values, and the registration builds
	// the S3-compatible store from them. All four are
	// required together -- the transform fails loudly when only some of them
	// are set, rather than silently falling back to the local-directory Preset
	// default, since a partially named S3 target is far more likely a typo than
	// a deliberate choice. S3Region and S3UseSSL below refine the same
	// composition.
	S3Endpoint  string `config:"env=APP_S3_ENDPOINT"`
	S3Bucket    string `config:"env=APP_S3_BUCKET"`
	S3AccessKey string `config:"env=APP_S3_ACCESS_KEY"`
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone, the same false positive the other
	// name-bearing declarations in this file are excepted from.
	S3SecretKey string `config:"env=APP_S3_SECRET_KEY"`

	// S3Region names APP_S3_REGION: the region of the S3 composition above.
	// Region matters to AWS S3 and Aliyun OSS (MinIO ignores it, per
	// objectstore/s3.Config's own doc comment); it is optional and only
	// meaningful alongside a complete APP_S3_* group.
	S3Region string `config:"env=APP_S3_REGION"`

	// S3UseSSL names APP_S3_USE_SSL: whether the S3 endpoint speaks TLS,
	// parsed by the loader as a strict bool. Unset is false -- plain HTTP, the
	// common case for a local RustFS -- and an emptied variable refuses the
	// load rather than reading as false.
	S3UseSSL bool `config:"env=APP_S3_USE_SSL"`

	// ObjectStoreRoot names APP_OBJECT_STORE_ROOT: a fixed local directory for
	// the "objectstore" seam instead of the Preset's throwaway temp directory
	// (pkgcore.NewLocalObjectStore, injected with the SurvivesRestart
	// capability alone -- a directory this process wrote survives this
	// process's restart, but nothing about a single-process local store is safe
	// across replicas, so MultiReplicaSafe is never claimed for it). It is the
	// local twin of the APP_S3_* composition above and an alternative to it:
	// setting both names two different stores for one seam, so the transform
	// refuses the combination for the same fail-loud-on-ambiguous-config reason
	// the S3 completeness rule gives. The field exists because a host that
	// needs its objects to outlive one process must name the directory itself
	// -- the Preset default is a throwaway MkdirTemp -- which is what the
	// two-boot expiry-sweep flow test (flowtests/periodic_scheduler_flow_test.go)
	// needs: boot 2's sweep must find the bytes boot 1 wrote.
	ObjectStoreRoot string `config:"env=APP_OBJECT_STORE_ROOT"`

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword name
	// APP_SMTP_HOST/APP_SMTP_PORT/APP_SMTP_USERNAME/APP_SMTP_PASSWORD: the
	// variables that together point the "mailer" seam at the registered
	// "mailer.smtp" implementation -- BuildServer overrides that one preset
	// entry with these values, and the registration builds the SMTP Mailer
	// from them. Host and port are required together, for the same
	// fail-loud-on-partial-config reason the S3 group gives; username and
	// password are optional -- SMTP AUTH activates only when a username is set
	// (pkgcore.SMTPConfig.Username's own doc comment). All unset -- the default
	// -- leaves the "mailer" seam on the Preset's console default, exactly like
	// every other seam here. The port is an int: a port of 0 is no usable SMTP
	// port and reads as unset, and an emptied APP_SMTP_PORT refuses the load
	// rather than arriving as 0.
	SMTPHost string `config:"env=APP_SMTP_HOST"`
	SMTPPort int    `config:"env=APP_SMTP_PORT"`
	// SMTPUsername and SMTPPassword are optional; SMTP AUTH activates only
	// when a username is set.
	SMTPUsername string `config:"env=APP_SMTP_USERNAME"`
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value; see
	// S3SecretKey's identical exception above.
	SMTPPassword string `config:"env=APP_SMTP_PASSWORD"`

	// SMSGatewayURL names APP_SMS_GATEWAY_URL: the endpoint the real HTTP SMS
	// transport (pkgcore.NewHTTPSMSSender) posts delivery requests to. Empty
	// under the standalone deployment mode leaves authn's "SMS sender" seam on
	// its console default; empty under the distributed deployment mode leaves
	// that seam deliberately UNWIRED, so authn's own wiring-time validation
	// fails closed with authn.ErrMissingDistributedSMSSender rather than this
	// app silently keeping a console sender nobody in a distributed replica
	// pool is reading -- see BuildServer's authn wiring comment for the
	// three-way branch this drives.
	//
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value; see
	// S3SecretKey's identical exception above.
	SMSGatewayURL string `config:"env=APP_SMS_GATEWAY_URL"`

	// Config carries the key material the config module declares on the
	// registry's bootstrap seat. The group exists so the loader target's key
	// path (config.cipher_key) is exactly the declared key's spelling:
	// config.Verify compares them literally, which is how a boot proves this
	// target binds what the module's declaration promises.
	Config hostConfigKeyConfig

	// Org carries the key material org declares on the registry's bootstrap
	// seat, under the same literal-key-path rule as Config above.
	Org hostConfigKeyOrg

	// Notification carries the key material notification declares on the
	// registry's bootstrap seat, under the same literal-key-path rule.
	Notification hostConfigKeyNotification

	// Pki carries the key material pki declares on the registry's bootstrap
	// seat, under the same literal-key-path rule.
	Pki hostConfigKeyPki

	// Authn carries the two key materials authn declares on the registry's
	// bootstrap seat, under the same literal-key-path rule.
	Authn hostConfigKeyAuthn

	// DemoUsersPassword names APP_DEMO_USERS_PASSWORD: the passphrase that,
	// when non-empty, makes BuildServer seed the three demo accounts of
	// demo_users.go at the end of its composition -- register each through the
	// composed handler, then grant the membership and role its actor model
	// declares -- so a browser visitor can sign in as demo-owner@example.com
	// and friends with a real account, real membership and real rbac grants,
	// and no demo header. Empty -- the default (the zero-external-dependency
	// `go run ./cmd/server` experience) -- skips the seed entirely. The
	// passphrase is not secret in the same sense as the config keys are, but it
	// is also not a hardcoded default: it gates a DEMO affordance behind an
	// operator's deliberate choice, and the variable shape (APP_*_PASSWORD)
	// mirrors the key-material variables for the same reason -- an environment
	// variable is visible, auditable and per-deployment in a way a compiled-in
	// default is not.
	//
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value; see
	// S3SecretKey's identical exception above.
	DemoUsersPassword string `config:"env=APP_DEMO_USERS_PASSWORD"`

	// DemoPlatformStaffPassword names APP_DEMO_PLATFORM_STAFF_PASSWORD: the
	// passphrase that, when non-empty, makes BuildServer seed the demo
	// platform-staff account of demo_admin.go (seedDemoPlatformStaff)
	// INDEPENDENTLY of DemoUsersPassword: a boot seeds each demo account set
	// from its own variable, never one from the other's. The platform
	// administrator (BuiltinRoleOwner under rbac.SystemDomain, every admin:*
	// permission included) must never be seeded from the ordinary demo users'
	// password -- seeding it from APP_DEMO_USERS_PASSWORD would let one value
	// unlock the platform administrator and every demo user at once -- so the
	// two credential sources stay apart by construction. Setting both
	// variables to the same value is an operator's own choice; this app's code
	// never makes one seed read the other's variable. Empty -- the default --
	// skips the platform-staff seed.
	//
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value; see
	// S3SecretKey's identical exception above.
	DemoPlatformStaffPassword string `config:"env=APP_DEMO_PLATFORM_STAFF_PASSWORD"`

	// AIGatewayImageBaseURL and AIGatewayImageAPIKey name
	// APP_AI_GATEWAY_IMAGE_BASE_URL and APP_AI_GATEWAY_IMAGE_API_KEY: when the
	// API key variable is set, the transform fills
	// cfg.AIGatewayImageBaseURL/APIKey, and BuildServer writes the platform-wide
	// image-generation credential at boot (see those ServerConfig fields' own
	// doc comment). Both unset -- the default -- keeps the zero-setup posture
	// exactly: the image credential row is never written and a simulate request
	// dead-letters with the gateway's coded credential refusal until an
	// operator names a provider. The pair exists because the image journey must
	// be runnable against a real booted server without editing Go code: the
	// browser end-to-end suite boots `go run ./cmd/server` and points the pair
	// at its own throwaway provider (playwright.config.ts), the same
	// demo-password shape APP_DEMO_USERS_PASSWORD already establishes for the
	// seeded demo accounts -- a non-secret value only a disposable server ever
	// uses. The API key is NOT a secret by construction here: it is the value a
	// local or CI-only fake provider accepts; a real deployment's key travels
	// the same variables and is this app's own documented operator wiring,
	// never a committed value.
	AIGatewayImageBaseURL string `config:"env=APP_AI_GATEWAY_IMAGE_BASE_URL"`
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value; see
	// S3SecretKey's identical exception above.
	AIGatewayImageAPIKey string `config:"env=APP_AI_GATEWAY_IMAGE_API_KEY"`

	// DisableQueueWorker names APP_DISABLE_QUEUE_WORKER: when set to any
	// non-empty value, BuildServer skips standaloneQueue.Start entirely -- the
	// queue still accepts Enqueue calls (a plain row insert, see
	// jobs.StandaloneQueue.Enqueue's own doc comment -- it needs no dispatcher
	// or worker goroutine), but this replica's own dispatcher and worker
	// goroutines never launch, so it can never claim or execute a Job itself,
	// no matter how long it runs. This exists purely for
	// integration_test/distributed_mode_test.go's positive proof: with one
	// replica's worker structurally disabled this way, the OTHER replica is the
	// only process that can ever run a delivery Job, which turns "did the
	// notification.inbox.created announcement reach a replica that never
	// executed anything itself" into a genuine, deterministic cross-process
	// EventBus proof rather than a coincidence a shared SQLite jobs table could
	// also explain (see that file's own doc comment). The field is a string,
	// not a bool, on purpose: the contract is "any non-empty value disables
	// it", so an emptied variable keeps meaning "not disabled". Empty -- the
	// default -- starts the queue worker normally.
	DisableQueueWorker string `config:"env=APP_DISABLE_QUEUE_WORKER"`

	// DisableDemoUserHeader names APP_DISABLE_DEMO_USER_HEADER: when set to any
	// non-empty value, BuildServer stops reading EVERY demo identity header
	// this app ships -- DemoUserHeader (demo_subject.go's "X-Demo-User") AND
	// DemoOrgUserHeader ("X-Demo-User-Id", the attribution header
	// DemoOrgSubjectResolverFor and DemoNotesSubjectResolver read) -- uniformly:
	//
	//   - every permission-gated route resolves its acting Subject from the
	//     verified authn Principal alone, through DemoSubjectResolverFor(true)
	//     (see that function's own doc comment);
	//   - every attribution seam DemoOrgSubjectResolverFor serves (org's
	//     caller-scoped invitation endpoints, the notification module's whole
	//     surface, integration's creator reads) and DemoNotesSubjectResolver
	//     serves (notes' create handler, the cases surface) resolves its
	//     acting user from the verified authn Principal alone.
	//
	// This is the kill switch DemoUserHeader's own doc comment describes for
	// the rbac header's privilege-escalation hole -- an unauthenticated header
	// that still outranks a proven identity when both are present, so a caller
	// holding nothing more than a low-privilege session could set the header to
	// a higher-privileged demo actor's id and have rbac decide against that
	// actor's grants instead of the caller's own -- extended to the
	// attribution header too: X-Demo-User-Id would let the same class of caller
	// act as (or read the data of) any user id on the
	// org/notification/cases/notes surfaces while DemoUserHeader alone stays
	// disabled. Like DisableQueueWorker it is a string on purpose ("any
	// non-empty value disables them"), so an emptied variable keeps the headers
	// enabled. Empty -- the default -- keeps every demo journey and every test
	// driving demo actors through the headers, which is deliberate: flipping
	// the default would break them at once (cmd/server/demo_subject_test.go,
	// the notesRequestAs-family helpers in flowtests/server_test.go and
	// cmd/server/test_support_test.go, and the flowtests suite that drives a
	// demo actor through a header). An operator deploying this reference app
	// somewhere a real, non-demo user might reach it is the one case this
	// variable exists for: setting it closes the hole with no code change. See
	// DEPLOY.md's own section on these headers for the operator-facing version
	// of this same warning.
	DisableDemoUserHeader string `config:"env=APP_DISABLE_DEMO_USER_HEADER"`

	// FailSelfServiceProvision names APP_FAIL_SELF_SERVICE_PROVISION: the
	// failure-injection count for the self-service clinic provisioning chain
	// (self_service.go). It is a TEST-AND-E2E-ONLY switch -- a real deployment
	// must never set it -- with a strictly disabled default: absent, or "0",
	// leaves cfg.FailSelfServiceProvision nil (newProvisionFailureInjector(0)
	// answers nil). Set to a positive integer N, the server fails the first N
	// provisioning attempts of EACH self-registered account -- the synchronous
	// attempt inside the register request and the retry job's own attempts
	// alike, since every attempt consults the same hook at the top of provision
	// -- and succeeds on every later attempt of that account. The count is per
	// account, never process-global, so one account's exhaustion cannot silence
	// the injection for the next. The e2e rig drives the "provisioning fails ->
	// the retry converges -> the sign-in lands" recovery path with N=1: a fresh
	// self-service register fails its one synchronous attempt, the first retry
	// (short backoff) converges the clinic, and the gate signs in once after
	// convergence -- inside go/authn's per-account login budget, with no "retry
	// until it passes" loop. The loader parses the text as an integer; a
	// negative one refuses the boot naming this variable, and an emptied one
	// refuses the load rather than arriving as 0.
	FailSelfServiceProvision int `config:"env=APP_FAIL_SELF_SERVICE_PROVISION"`
}

// hostConfigKeyConfig carries the key material the config module declares: the
// parent and child names spell the declared key path config.cipher_key
// literally, which config.Verify compares against the declaration.
type hostConfigKeyConfig struct {
	// Cipher_Key names APP_CONFIG_KEY: the 32-byte AES cipher key the config
	// module seals Sensitive values with (config.WithCipher over
	// dbkit.NewCipher). It is the bootstrap configuration this app's own configs
	// table must never hold -- the key that encrypts the table cannot live in
	// the table -- so it resolves through the loader like every other bootstrap
	// value. The derive tag makes the loader the owner of its resolution: an
	// explicit APP_CONFIG_KEY must be 64 hex characters, the configured root
	// key (APP_ROOT_KEY, via loadHostConfig's WithRootKeyEnv) derives the key's
	// material from the declared path when no explicit value is set, and the
	// documented development default (DevConfigKey) stands when neither is
	// present. An emptied variable reads as unset, as it always has.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key config.cipher_key, which Verify compares literally.
	Cipher_Key []byte `config:"env=APP_CONFIG_KEY,derive"`
}

// hostConfigKeyOrg carries the key material org declares: the parent and child
// names spell the declared key path org.invitation_email_index_key literally.
type hostConfigKeyOrg struct {
	// Invitation_Email_Index_Key names APP_ORG_INDEX_KEY: the 32-byte HMAC key
	// org.WithEmailIndexer's blind indexer is built from
	// (org.NewEmailIndexer), resolved through the loader's derive path
	// exactly like the config key above (explicit 64 hex characters, else the
	// APP_ROOT_KEY derivation, else DevOrgIndexKey). It is a SEPARATE bootstrap
	// secret from the config cipher key on purpose: this app reuses the config
	// cipher (built from APP_CONFIG_KEY) to also encrypt org's Invitation.Email
	// column (registered through org.RegisterEmailSerializer), and dbkit's own
	// rule is that an AES key must never double as an HMAC key -- see
	// go/org/invitation.go's EmailSerializerName doc comment. Introducing this
	// one additional key, distinct from the cipher key, is what keeps that rule
	// real rather than aspirational in this app's own wiring. An invitation
	// whose address cannot be indexed can never be found again, so the key must
	// not change between restarts.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key org.invitation_email_index_key, which Verify compares literally.
	Invitation_Email_Index_Key []byte `config:"env=APP_ORG_INDEX_KEY,derive"`
}

// hostConfigKeyNotification carries the key material notification declares: the
// parent and child names spell the declared key path
// notification.contact_index_key literally.
type hostConfigKeyNotification struct {
	// Contact_Index_Key names APP_NOTIFICATION_INDEX_KEY: the 32-byte HMAC key
	// the blind indexers over the notification module's encrypted contact
	// addresses are built from
	// (notification.NewContactEmailIndexer/NewContactPhoneIndexer), resolved
	// through the loader's derive path exactly like the two keys above
	// (explicit 64 hex characters, else the APP_ROOT_KEY derivation, else
	// DevNotificationIndexKey). It is a SEPARATE bootstrap secret from the
	// config cipher key for the same reason the org key above gives: this app
	// reuses the config cipher to also encrypt notification's Contact.Address
	// column (registered through
	// notification.RegisterContactAddressSerializer), and
	// dbkit's own rule is that an AES key must never double as an HMAC key. One
	// HMAC key serves both the email and the phone indexers, exactly as authn's
	// single blind-index key serves both of its indexers -- the two normalizers
	// keep the two index columns' inputs in disjoint canonical forms, so a
	// shared key leaks nothing between them. It must not change between
	// restarts.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key notification.contact_index_key, which Verify compares literally.
	Contact_Index_Key []byte `config:"env=APP_NOTIFICATION_INDEX_KEY,derive"`
}

// hostConfigKeyPki carries the key material pki declares: the parent and child
// names spell the declared key path pki.local_key_cipher_key literally.
type hostConfigKeyPki struct {
	// Local_Key_Cipher_Key names APP_PKI_LOCAL_KEY_CIPHER_KEY: the 32-byte AES
	// key that seals go/pki's LocalSigner private-key column (pki_local_keys,
	// via pki.RegisterLocalKeySerializer), resolved through the loader's derive
	// path (explicit 64 hex characters, else the APP_ROOT_KEY derivation, else
	// DevPKILocalKeyCipherKey). Without it, a deployment that sets none of this
	// file's other keys would silently run its signing-key storage on a key
	// committed to this repository's own source (the DevPKILocalKeyCipherKey
	// development default). It is a SEPARATE bootstrap secret from every other
	// key in this file: dbkit's key-separation rule applies across modules, not
	// only within one. The signing key itself is generated once by
	// pki.Service.EnsurePurpose and PERSISTS in cfg.SQLitePath across restarts,
	// so this key must stay stable across restarts too.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key pki.local_key_cipher_key, which Verify compares literally.
	Local_Key_Cipher_Key []byte `config:"env=APP_PKI_LOCAL_KEY_CIPHER_KEY,derive"`
}

// hostConfigKeyAuthn carries the two key materials authn declares: the parent
// and child names spell the declared key paths authn.blind_index_key and
// authn.pii_cipher_key literally.
type hostConfigKeyAuthn struct {
	// Blind_Index_Key names APP_AUTHN_BLIND_INDEX_KEY: the 32-byte HMAC key
	// authn.WithBlindIndexKey indexes its users.email_index/phone_index columns
	// with (dbkit.NewBlindIndexer), resolved through the loader's derive path
	// (explicit 64 hex characters, else the APP_ROOT_KEY derivation, else
	// DevBlindIndexKey). This key must stay IDENTICAL across restarts or every
	// already-stored email/phone blind index becomes unfindable, so setting
	// this variable (or APP_ROOT_KEY, which derives it) and then changing it
	// has the same operational consequences a real key rotation always has.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key authn.blind_index_key, which Verify compares literally.
	Blind_Index_Key []byte `config:"env=APP_AUTHN_BLIND_INDEX_KEY,derive"`

	// PII_Cipher_Key names APP_AUTHN_PII_CIPHER_KEY: the 32-byte AES key that
	// seals authn's encrypted PII columns (email, phone, TOTP secrets) via
	// authn.RegisterPIISerializer, resolved through the loader's derive path
	// (explicit 64 hex characters, else the APP_ROOT_KEY derivation, else
	// DevPIICipherKey) -- deliberately a SEPARATE secret from every other key
	// in this file, including the pki key.
	//
	//nolint:staticcheck // the field name must lowercase to the declared bootstrap key authn.pii_cipher_key, which Verify compares literally.
	PII_Cipher_Key []byte `config:"env=APP_AUTHN_PII_CIPHER_KEY,derive"`
}

// hostConfigDefaults returns the loader target with the defaults the loader
// falls back to when no source supplies a key (its lowest-priority source).
// An unset deployment mode is standalone, an unset port is DefaultPort, an
// unset database path is DefaultSQLitePath, and each of the six key materials
// starts at its documented, recognizable, NON-SECRET development default (the
// Dev* constants) -- the value that stands when neither the key's own variable
// nor APP_ROOT_KEY is set. Every other field's zero value is its documented
// unset behavior.
func hostConfigDefaults() hostConfig {
	return hostConfig{
		DeploymentMode: string(pkgcore.DeploymentModeStandalone),
		Port:           DefaultPort,
		DBPath:         DefaultSQLitePath,
		Config:         hostConfigKeyConfig{Cipher_Key: DevConfigKey},
		Org:            hostConfigKeyOrg{Invitation_Email_Index_Key: DevOrgIndexKey},
		Notification:   hostConfigKeyNotification{Contact_Index_Key: DevNotificationIndexKey},
		Pki:            hostConfigKeyPki{Local_Key_Cipher_Key: DevPKILocalKeyCipherKey},
		Authn: hostConfigKeyAuthn{
			Blind_Index_Key: DevBlindIndexKey,
			PII_Cipher_Key:  DevPIICipherKey,
		},
	}
}

// loadHostConfig resolves the app's bootstrap surface from the process
// environment through the loader: flags first, then the environment under the
// APP_ prefix (PORT pinned by name), no config file, the key-material
// derivation over APP_ROOT_KEY, then hostConfigDefaults.
//
// The two derivation options are this app's whole root-key wiring.
// WithRootKeyEnv("APP_ROOT_KEY") has the loader read the host's root secret --
// one hex-encoded 32-byte high-entropy value -- in the same Load that consumes
// it: no os.Getenv in this app's own code, and no chicken-and-egg between
// reading the secret and resolving the fields it feeds. A variable that is
// unset or empty means no root key, and every key material keeps its
// development default; a set-but-wrong value fails the load, naming the
// variable. WithKeyDerivation(dbkit.DeriveBootstrapKey) installs the platform
// composition each derive-tagged field's material comes from: the field's
// dotted key path gets its purpose string at the bootstrap seat
// (pkgcore.BootstrapKeyPurpose -- which is why the six key-group field names
// spell the declared key paths literally, the same literals config.Verify
// compares), and dbkit.DeriveKey turns the root key plus that purpose into the
// field's 32 bytes.
//
// The precedence the loader applies per key is the documented three tiers: an
// explicitly-set individual variable (e.g. APP_ORG_INDEX_KEY) always wins,
// over what APP_ROOT_KEY would have derived for that same key, which in turn
// always wins over the hardcoded development default -- so setting
// APP_ROOT_KEY alone is the recommended deployment shape (DEPLOY.md documents
// it), while a deployment that wants fine-grained, independent rotation for
// one specific key keeps setting that key's own variable instead, and the two
// compose freely. The trade-off APP_ROOT_KEY inherits from the platform
// derivation is real: a leaked root key compromises every derived key at once,
// and rotating it rotates all six materials together; a deployment that can
// accept neither keeps setting the individual variables.
func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	err := config.New(
		config.WithEnvPrefix(envPrefix),
		config.WithRootKeyEnv("APP_ROOT_KEY"),
		config.WithKeyDerivation(dbkit.DeriveBootstrapKey),
	).Load(&hc)
	return hc, err
}

// ConfigFromEnv resolves ServerConfig from the process environment through the
// bootstrap loader, defaulting to the standalone deployment mode on SQLite so
// `go run ./cmd/server` genuinely starts a working server with zero external
// dependencies. It is loadHostConfig plus the transform below, in one call,
// for every caller that wants the assembled config rather than the raw
// surface.
func ConfigFromEnv() (ServerConfig, error) {
	hc, err := loadHostConfig()
	if err != nil {
		return ServerConfig{}, err
	}
	return serverConfigFrom(hc)
}

// serverConfigFrom transforms a loaded bootstrap surface into the ServerConfig
// the assembly consumes: it parses the deployment mode and enforces the
// cross-variable rules (the S3 and SMTP completeness pairs, the object-store
// ambiguity, the Fly-client-IP declaration pair) that a single-variable loader
// cannot state. Every refusal names the variable an operator must change. The
// six key materials arrive fully resolved -- the loader decoded an explicit
// value, derived one from APP_ROOT_KEY, or left the development default
// standing -- so this transform only carries them over.
//
// Strings arrive from the loader with "" for both an unset and an explicitly
// emptied variable -- the same value a direct environment read reports -- so
// every default this transform applies treats "" as unset.
func serverConfigFrom(hc hostConfig) (ServerConfig, error) {
	deploymentModeStr := hc.DeploymentMode
	if deploymentModeStr == "" {
		deploymentModeStr = string(pkgcore.DeploymentModeStandalone)
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(deploymentModeStr)
	if err != nil {
		return ServerConfig{}, err
	}

	port := hc.Port
	if port == "" {
		port = DefaultPort
	}

	dbPath := hc.DBPath
	if dbPath == "" {
		dbPath = DefaultSQLitePath
	}

	// failProvisionCount is the self-service provisioning failure injection
	// APP_FAIL_SELF_SERVICE_PROVISION arms -- 0 (absent or "0", the production
	// default) disables it entirely; a positive N arms
	// newProvisionFailureInjector below with a per-account budget of N failed
	// attempts (see the field's own doc comment for the full contract and the
	// e2e shape). A negative count refuses the boot here, naming the variable,
	// exactly like every other strict parse this transform runs; a value that
	// is not an integer at all was refused by the loader already.
	failProvisionCount := hc.FailSelfServiceProvision
	if failProvisionCount < 0 {
		return ServerConfig{}, fmt.Errorf("reference-app: APP_FAIL_SELF_SERVICE_PROVISION must not be negative (absent or 0 disables the injection), got %d", failProvisionCount)
	}

	// The six key materials arrive from the loader already resolved: an
	// explicit individual variable's hex text was decoded, APP_ROOT_KEY's
	// derivation filled whatever no explicit value supplied, and the Dev*
	// struct defaults (hostConfigDefaults) stand for the rest -- see
	// loadHostConfig's own doc comment for the wiring and the precedence.
	configKey := hc.Config.Cipher_Key
	orgIndexKey := hc.Org.Invitation_Email_Index_Key
	notificationIndexKey := hc.Notification.Contact_Index_Key
	pkiLocalKeyCipherKey := hc.Pki.Local_Key_Cipher_Key
	authnBlindIndexKey := hc.Authn.Blind_Index_Key
	authnPIICipherKey := hc.Authn.PII_Cipher_Key

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the Preset's local-directory default;
	// when any one of them is set, all four are required -- the S3 fields' own
	// doc comment explains why a partial S3 target is refused rather than
	// silently ignored.
	s3Endpoint := hc.S3Endpoint
	s3Bucket := hc.S3Bucket
	s3AccessKey := hc.S3AccessKey
	s3SecretKey := hc.S3SecretKey
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		var missing []string
		if s3Endpoint == "" {
			missing = append(missing, "APP_S3_ENDPOINT")
		}
		if s3Bucket == "" {
			missing = append(missing, "APP_S3_BUCKET")
		}
		if s3AccessKey == "" {
			missing = append(missing, "APP_S3_ACCESS_KEY")
		}
		if s3SecretKey == "" {
			missing = append(missing, "APP_S3_SECRET_KEY")
		}
		if len(missing) > 0 {
			return ServerConfig{}, fmt.Errorf(
				"reference-app: an S3 ObjectStore composition needs %s set too (got some but not all of APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY)",
				strings.Join(missing, ", "))
		}
	}
	s3UseSSL := hc.S3UseSSL

	// readFlyClientIP is APP_READ_FLY_CLIENT_IP's strict bool: 'true' is this
	// deployment's declaration that its proxy is Fly's, the per-header opt-in
	// that authorizes authn to read Fly-Client-IP (see the field's own doc
	// comment). The declaration is only meaningful alongside
	// APP_TRUSTED_PROXIES -- authn reads a vendor header only from a request
	// whose peer is a declared proxy, so the header with no declared proxy
	// would never be read and the records would silently stay proxy-addressed,
	// the defect the pair exists to fix -- which is why that combination is
	// refused here rather than accepted as a no-op.
	readFlyClientIP := hc.ReadFlyClientIP
	if readFlyClientIP && len(splitTrustedProxies(hc.TrustedProxies)) == 0 {
		return ServerConfig{}, fmt.Errorf(
			"reference-app: APP_READ_FLY_CLIENT_IP is true but APP_TRUSTED_PROXIES is empty: reading Fly-Client-IP is authorized only for a deployment whose proxy is declared there, and this pair would silently keep recording the proxy itself")
	}

	// objectStoreRoot is the local-directory twin of the S3 composition above:
	// unset leaves "objectstore" on the Preset's throwaway temp-directory
	// default, set names a fixed directory whose contents survive this
	// process's restart (the field's own doc comment has the capability
	// reasoning). Both compositions at once would name two different stores for
	// the one seam, so a non-empty root alongside a complete S3 target is
	// refused rather than silently preferring one.
	objectStoreRoot := hc.ObjectStoreRoot
	if objectStoreRoot != "" && s3Endpoint != "" {
		return ServerConfig{}, fmt.Errorf(
			"reference-app: APP_OBJECT_STORE_ROOT and an APP_S3_* composition name two different ObjectStores for one seam; set only one of them")
	}

	// smtpHost/smtpPort mirror the S3 group above: both unset leaves the
	// "mailer" seam on the Preset's console default, and a partial APP_SMTP_*
	// set is refused rather than silently ignored. The port's zero value is its
	// unset value -- 0 is no usable SMTP port -- and an emptied APP_SMTP_PORT
	// never reaches here at all: the loader refuses an empty value for an int
	// field.
	smtpHost := hc.SMTPHost
	smtpPort := hc.SMTPPort
	switch {
	case smtpHost == "" && smtpPort == 0:
		// Both unset: the "mailer" seam stays on its Preset default.
	case smtpHost == "" || smtpPort == 0:
		return ServerConfig{}, fmt.Errorf(
			"reference-app: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set")
	}

	// publicOrigin is where outbound mail links point for a tenant that has no
	// branded host in cfg.HostTenants (every self-registered clinic).
	// APP_PUBLIC_ORIGIN when set, else the origin every zero-setup local demo
	// is actually reached at -- "http://localhost:" + the same resolved PORT
	// above; a real deployment whose mail must reach real recipients sets the
	// variable (the field's own doc comment has the full split).
	publicOrigin := hc.PublicOrigin
	if publicOrigin == "" {
		publicOrigin = "http://localhost:" + port
	}

	cfg := ServerConfig{
		DeploymentMode:        deploymentMode,
		Port:                  port,
		SQLitePath:            dbPath,
		ConfigKey:             configKey,
		OrgIndexKey:           orgIndexKey,
		NotificationIndexKey:  notificationIndexKey,
		PKILocalKeyCipherKey:  pkiLocalKeyCipherKey,
		AuthnBlindIndexKey:    authnBlindIndexKey,
		AuthnPIICipherKey:     authnPIICipherKey,
		RedisAddr:             hc.RedisAddr,
		OTLPEndpoint:          hc.OTLPEndpoint,
		S3Endpoint:            s3Endpoint,
		S3Bucket:              s3Bucket,
		S3AccessKey:           s3AccessKey,
		S3SecretKey:           s3SecretKey,
		S3Region:              hc.S3Region,
		S3UseSSL:              s3UseSSL,
		ObjectStoreRoot:       objectStoreRoot,
		SMTPHost:              smtpHost,
		SMTPPort:              smtpPort,
		SMTPUsername:          hc.SMTPUsername,
		SMTPPassword:          hc.SMTPPassword,
		SMSGatewayURL:         hc.SMSGatewayURL,
		DisableQueueWorker:    hc.DisableQueueWorker != "",
		DisableDemoUserHeader: hc.DisableDemoUserHeader != "",
		TrustedProxies:        splitTrustedProxies(hc.TrustedProxies),
		ReadFlyClientIP:       readFlyClientIP,
		WebDistDir:            hc.WebDist,
		HostTenants:           DemoHostTenants,
		PublicOrigin:          publicOrigin,
		// Empty when unset: the demo-user seed is opt-in (its own doc comment
		// in demo_users.go says why the default skips it). The platform-staff
		// seed is read from its OWN variable, never this one -- see the
		// field's own doc comment for why the platform administrator must not
		// share the ordinary demo users' credential source.
		DemoUsersPassword:         hc.DemoUsersPassword,
		DemoPlatformStaffPassword: hc.DemoPlatformStaffPassword,
		// Filled from the environment when the API key variable is set, left
		// empty otherwise (the zero-setup default that skips the
		// image-credential write at boot entirely) -- see the fields' own doc
		// comment for the full reasoning and the demo-only shape of the value.
		AIGatewayImageBaseURL: hc.AIGatewayImageBaseURL,
		AIGatewayImageAPIKey:  hc.AIGatewayImageAPIKey,
		// newProvisionFailureInjector(0) answers nil, so the default -- absent
		// or "0" -- keeps the field nil; a positive count arms the injection
		// the e2e rig drives (the field's own doc comment).
		FailSelfServiceProvision: newProvisionFailureInjector(failProvisionCount),
	}
	// The SMTP target itself needs no further resolution here: BuildServer
	// names "mailer.smtp" in the preset entry the resolved SMTPHost group
	// overrides and hands the four values over as that entry's Config -- the
	// registration builds the mailer, the host pre-builds nothing (the
	// ServerConfig.SMTPHost doc comment and BuildServer's kernel-options
	// comment carry the full split).
	return cfg, nil
}

// splitTrustedProxies splits APP_TRUSTED_PROXIES' comma-separated value into
// the per-entry list ServerConfig.TrustedProxies carries: trimmed, empty
// entries dropped, empty input yielding nil. It never rejects an entry --
// validation of the entries themselves is authn.WithTrustedProxies' job
// (go/authn's newOptions refuses an entry that is neither an IP address nor a
// CIDR prefix), so a typo'd declaration fails boot there, naming the entry,
// rather than here.
func splitTrustedProxies(raw string) []string {
	if raw == "" {
		return nil
	}
	var proxies []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			proxies = append(proxies, entry)
		}
	}
	return proxies
}

// hostBootstrapKeys lists the bootstrap keys this app owns: the deployment
// shape, connection addresses, switches and demo rigs of its own assembly. The
// six key materials the platform modules declare are deliberately not listed
// here -- they are verified from the live registry instead, so a module adding
// a key fails this app's boot until its target binds it. APP_ROOT_KEY is
// absent for a structural reason rather than an omission: it is not a field of
// the loader target at all, it is the variable loadHostConfig's
// WithRootKeyEnv option reads inside the same Load (so there is no field for
// config.Verify to bind).
var hostBootstrapKeys = []string{
	"deploymentmode",
	"port",
	"dbpath",
	"redisaddr",
	"otlpendpoint",
	"publicorigin",
	"webdist",
	"trustedproxies",
	"readflyclientip",
	"s3endpoint",
	"s3bucket",
	"s3accesskey",
	"s3secretkey",
	"s3region",
	"s3usessl",
	"objectstoreroot",
	"smtphost",
	"smtpport",
	"smtpusername",
	"smtppassword",
	"smsgatewayurl",
	"demouserspassword",
	"demoplatformstaffpassword",
	"aigatewayimagebaseurl",
	"aigatewayimageapikey",
	"disabledemouserheader",
	"disablequeueworker",
	"failselfserviceprovision",
}

// verifyBootstrapBinding proves the loader target binds this app's whole
// bootstrap surface: every key the composed modules declared on the registry's
// bootstrap seat maps onto a field, and every key this app owns does too. A
// field nobody declares and no key reaches is what the target's own leaf count
// pins (bootstrap_test.go), so the two directions together are the strict
// correspondence between the target and its surface.
func verifyBootstrapBinding(reg *pkgcore.Registry) error {
	declared := reg.Bootstrap.Keys()
	declaredKeys := make([]string, 0, len(declared))
	for _, key := range declared {
		declaredKeys = append(declaredKeys, key.Key)
	}
	if err := config.Verify(&hostConfig{}, declaredKeys); err != nil {
		return fmt.Errorf("reference-app: the bootstrap target must bind the keys the composed modules declared: %w", err)
	}
	if err := config.Verify(&hostConfig{}, hostBootstrapKeys); err != nil {
		return fmt.Errorf("reference-app: the bootstrap target must bind the host's own bootstrap keys: %w", err)
	}
	return nil
}
