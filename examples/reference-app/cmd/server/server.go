// Package main is examples/reference-app's minimal starter skeleton --
// exactly the kind of "minimal starter skeleton...freely editable by
// consumers" root CLAUDE.md's "Shape" section describes, not a business
// module. It never goes through pkgcore.Module.Register itself; its whole
// job is wiring one together (see buildServer below) and running it.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so dbkit.Open below has a driver to build from -- the reference app
	// runs its own database in standalone deployment mode's SQLite dialect
	// regardless of which deployment mode its other infrastructure seams
	// compose under.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	obs "github.com/vislake/speed/go/observability"

	// Blank-imported for its init() side effect: obs.Init's local
	// exporters wire a real /metrics scrape endpoint only when a local
	// metrics reader has been registered (go/observability's own doc
	// comment on Init and RegisterLocalMetricsReader) -- this is what
	// metricsHandler below actually serves once main.go's run has called
	// obs.Init. Without this import, obs.Init still runs (traces and
	// metrics both go to stdout), but MetricsHandler answers 404.
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	kvredis "github.com/vislake/speed/go/pkgcore/kv/redis"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
)

const (
	// defaultPort is used when the PORT environment variable is unset.
	defaultPort = "8080"

	// defaultSQLitePath is used when APP_DB_PATH is unset. It is a
	// relative path so `go run ./cmd/server` works with zero setup, per
	// root CLAUDE.md's "task dev must work in standalone deployment
	// mode" rule applied to this example's own entry point.
	defaultSQLitePath = "reference-app.db"

	// shutdownTimeout bounds how long graceful shutdown waits for
	// in-flight requests to finish before giving up.
	shutdownTimeout = 10 * time.Second

	// readHeaderTimeout bounds how long the server waits to receive a
	// request's headers before aborting the connection -- protects
	// against slow-header (Slowloris-style) connections that trickle
	// bytes to hold a socket open indefinitely.
	readHeaderTimeout = 5 * time.Second

	// healthzPath is the one route exempted from tenant resolution -- see
	// buildServer's use of tenancy.WithAllowlist.
	healthzPath = "/healthz"

	// metricsPath is the standalone deployment mode's Prometheus scrape
	// endpoint, exempted from tenant resolution for exactly the same
	// reason healthzPath is: a scraper (or a human's browser, per
	// docs/internal/09-observability.md's own description of the standalone
	// deployment mode) has no demo Host to send and must not depend on one.
	metricsPath = "/metrics"

	// configKeyEnv names the environment variable holding the hex-encoded
	// 32-byte master key the config module seals Sensitive values with
	// (config.WithCipher over dbkit.NewCipher). It is the bootstrap
	// configuration this app's own configs table must never hold -- the
	// key that encrypts the table cannot live in the table -- so it comes
	// from the environment like every other bootstrap value, with the
	// documented development default below.
	configKeyEnv = "APP_CONFIG_KEY"

	// configKeyHexLength is the encoded length of the required 32-byte key
	// (2 hex characters per byte), checked so a short or malformed
	// APP_CONFIG_KEY fails configuration loading with a precise message
	// rather than surfacing later as an opaque NewCipher error.
	configKeyHexLength = 64

	// orgIndexKeyEnv names the environment variable holding the hex-encoded
	// 32-byte HMAC key org.WithEmailIndexer's blind indexer is built from
	// (dbkit.NewBlindIndexer). It is a SEPARATE bootstrap secret from
	// configKeyEnv on purpose: this app reuses the config cipher (built
	// from configKeyEnv) to also encrypt org's Invitation.Email column
	// (registered under org.EmailSerializerName below), and dbkit's own
	// rule is that an AES key must never double as an HMAC key -- see
	// go/org/invitation.go's EmailSerializerName doc comment. Introducing
	// this one additional key, distinct from the cipher key, is what keeps
	// that rule real rather than aspirational in this app's own wiring.
	orgIndexKeyEnv = "APP_ORG_INDEX_KEY"

	// notificationIndexKeyEnv names the environment variable holding the
	// hex-encoded 32-byte HMAC key the blind indexers over the notification
	// module's encrypted contact addresses are built from
	// (dbkit.NewBlindIndexer). It is a SEPARATE bootstrap secret from
	// configKeyEnv for the same reason orgIndexKeyEnv's doc comment above
	// gives: this app reuses the config cipher to also encrypt
	// notification's Contact.Address column (registered under
	// notification.ContactAddressSerializerName below), and dbkit's own rule
	// is that an AES key must never double as an HMAC key. One HMAC key
	// serves both the email and the phone indexers, exactly as authn's
	// single blind-index key serves both of its indexers (devBlindIndexKey's
	// comment says so) -- the two normalizers keep the two index columns'
	// inputs in disjoint canonical forms, so a shared key leaks nothing
	// between them.
	notificationIndexKeyEnv = "APP_NOTIFICATION_INDEX_KEY"

	// pkiLocalKeyCipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals go/pki's LocalSigner private-key
	// column (pki_local_keys, via pki.RegisterLocalKeySerializer). Before
	// this round it had NO override path at all -- only the hardcoded
	// devPKILocalKeyCipherKey development default existed -- so a real
	// deployment that set none of this file's other keys was silently
	// running its signing-key storage on a key committed to this
	// repository's own source. It is a SEPARATE bootstrap secret from every
	// other key in this file: dbkit's key-separation rule applies across
	// modules, not only within one (devPKILocalKeyCipherKey's own doc
	// comment).
	pkiLocalKeyCipherKeyEnv = "APP_PKI_LOCAL_KEY_CIPHER_KEY"

	// authnBlindIndexKeyEnv names the environment variable holding the
	// hex-encoded 32-byte HMAC key authn.WithBlindIndexKey indexes its
	// users.email_index/phone_index columns with (dbkit.NewBlindIndexer).
	// Like pkiLocalKeyCipherKeyEnv above, this had NO override path before
	// this round -- only the hardcoded devBlindIndexKey development default
	// existed. This key must stay IDENTICAL across restarts (devBlindIndexKey's
	// own doc comment) or every already-stored email/phone blind index
	// becomes unfindable, so setting this env var (or APP_ROOT_KEY, which
	// derives it) and then changing it has the same operational
	// consequences a real key rotation always has -- see "Key derivation"
	// in go/dbkit/AGENTS.md for the rotation-granularity trade-off.
	authnBlindIndexKeyEnv = "APP_AUTHN_BLIND_INDEX_KEY"

	// authnPIICipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals authn's encrypted PII columns
	// (email, phone, TOTP secrets) via authn.RegisterPIISerializer. Like
	// its two siblings above, this had NO override path before this round --
	// only the hardcoded devPIICipherKey development default existed --
	// deliberately a SEPARATE secret from every other key in this file,
	// including pkiLocalKeyCipherKeyEnv (devPIICipherKey's own doc comment).
	authnPIICipherKeyEnv = "APP_AUTHN_PII_CIPHER_KEY"

	// rootKeyEnv names the environment variable holding a single
	// hex-encoded 32-byte high-entropy root secret that, when set, derives
	// ALL SIX of the key materials this file otherwise requires
	// individually (configKeyEnv, orgIndexKeyEnv, notificationIndexKeyEnv,
	// pkiLocalKeyCipherKeyEnv, authnBlindIndexKeyEnv, authnPIICipherKeyEnv)
	// via dbkit.DeriveKey, one distinct, versioned purpose string per key
	// (see the rootKeyPurpose* constants below) -- so a deployer can set
	// ONE secret instead of six and still end up with six independent
	// derived keys, none of them reused across two differently-designed
	// constructions (go/dbkit/AGENTS.md's "Key derivation" section has the
	// full rationale and the honest trade-off: a leaked root key
	// compromises every derived key at once, and rotating the root
	// rotates all six simultaneously).
	//
	// Precedence, applied independently per key: an explicitly-set
	// individual environment variable (e.g. APP_ORG_INDEX_KEY) always
	// wins over what APP_ROOT_KEY would have derived for that same key,
	// which in turn always wins over the hardcoded development default --
	// so setting APP_ROOT_KEY alone is the recommended default for a
	// real deployment (examples/reference-app/DEPLOY.md documents this),
	// while a deployment that wants fine-grained, independent rotation for
	// one specific key keeps setting that key's own variable instead, and
	// the two compose freely. Leaving APP_ROOT_KEY unset changes nothing
	// about this file's behavior before this round existed -- every one of
	// the six keys still falls back to its own hardcoded development
	// default exactly as before.
	rootKeyEnv = "APP_ROOT_KEY"

	// redisAddrEnv names the environment variable holding the Redis server
	// address ("host:port") the injected EventBus AND KVStore connect to --
	// one Redis instance backs both seams, sharing one *redis.Client the
	// same way go/notification's own Redis integration leg shares one
	// client across two bus instances (see buildServer's kernel doc comment
	// below for what the resulting composition proves). Empty -- the
	// default -- leaves both seams on the in-process implementations the
	// Preset resolves, so zero-setup standalone development keeps working
	// with nothing else running; set it to compose real Redis-backed
	// implementations into the SAME standalone deployment mode, which is
	// the deployment-mode / implementation-composition orthogonality
	// docs/internal/03-deployment-modes.md draws, or into a distributed
	// deployment mode, where MultiReplicaSafe is required of both seams.
	redisAddrEnv = "APP_REDIS_ADDR"

	// s3EndpointEnv, s3BucketEnv, s3AccessKeyEnv and s3SecretKeyEnv name the
	// environment variables that together compose a real S3-compatible
	// ObjectStore (objectstore/s3.NewObjectStore) for the "objectstore"
	// seam. All four are required together -- configFromEnv fails loudly
	// when only some of them are set, rather than silently falling back to
	// the local-directory Preset default, since a partially named S3 target
	// is far more likely a typo than a deliberate choice. s3RegionEnv and
	// s3UseSSLEnv refine the same composition: Region matters to AWS S3 and
	// Aliyun OSS (MinIO ignores it, per objectstore/s3.Config's own doc
	// comment), and s3UseSSLEnv, parsed as a Go bool, defaults to false --
	// plain HTTP, the common case for a local RustFS -- when unset.
	s3EndpointEnv  = "APP_S3_ENDPOINT"
	s3BucketEnv    = "APP_S3_BUCKET"
	s3AccessKeyEnv = "APP_S3_ACCESS_KEY"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone, the same false positive
	// demoUsersPasswordEnv's own #nosec comment (demo_users.go) already
	// excepts elsewhere in this codebase.
	s3SecretKeyEnv = "APP_S3_SECRET_KEY"
	s3RegionEnv    = "APP_S3_REGION"
	s3UseSSLEnv    = "APP_S3_USE_SSL"

	// objectStoreRootEnv names the environment variable that points the
	// "objectstore" seam at a fixed local directory instead of the
	// Preset's throwaway temp directory: pkgcore.NewLocalObjectStore over
	// APP_OBJECT_STORE_ROOT, injected with the SurvivesRestart capability
	// alone -- a directory this process wrote survives this process's
	// restart, but nothing about a single-process local store is safe
	// across replicas, so MultiReplicaSafe is never claimed for it. This
	// is the local twin of the APP_S3_* composition above and an
	// alternative to it: setting both names two different stores for one
	// seam, so configFromEnv refuses the combination for the same
	// fail-loud-on-ambiguous-config reason the S3 completeness rule gives.
	objectStoreRootEnv = "APP_OBJECT_STORE_ROOT"

	// smtpHostEnv and smtpPortEnv name the environment variables that
	// together compose a real SMTP Mailer (pkgcore.NewSMTPMailer) for the
	// "mailer" seam; both are required together, for the same
	// fail-loud-on-partial-config reason s3EndpointEnv's doc comment gives.
	// smtpUsernameEnv and smtpPasswordEnv are optional -- SMTP AUTH
	// activates only when a username is set (pkgcore.SMTPConfig.Username's
	// own doc comment). Unset -- the default -- leaves the "mailer" seam on
	// the Preset's console default, exactly like every other seam here.
	smtpHostEnv     = "APP_SMTP_HOST"
	smtpPortEnv     = "APP_SMTP_PORT"
	smtpUsernameEnv = "APP_SMTP_USERNAME"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value, the identical false positive s3SecretKeyEnv's own #nosec
	// comment above excepts.
	smtpPasswordEnv = "APP_SMTP_PASSWORD"

	// smsGatewayURLEnv names the environment variable holding the endpoint
	// authn's real SMS transport (authn.NewHTTPSMSSender) posts delivery
	// requests to. Empty under the standalone deployment mode leaves
	// authn's "SMS sender" seam on its console default, unchanged from
	// before this round; empty under the distributed deployment mode
	// leaves that seam deliberately UNWIRED, so authn's own wiring-time
	// validation fails closed with authn.ErrMissingDistributedSMSSender
	// rather than this app silently keeping a console sender nobody in a
	// distributed replica pool is reading -- see buildServer's authn
	// wiring comment for the three-way branch this drives.
	smsGatewayURLEnv = "APP_SMS_GATEWAY_URL"

	// disableQueueWorkerEnv names the environment variable that, when set to
	// any non-empty value, makes buildServer skip standaloneQueue.Start
	// entirely: the queue still accepts Enqueue calls (a plain row insert,
	// see jobs.StandaloneQueue.Enqueue's own doc comment -- it needs no
	// dispatcher or worker goroutine), but this replica's own dispatcher and
	// worker goroutines never launch, so it can never claim or execute a
	// Job itself, no matter how long it runs. This exists purely for
	// integration_test/distributed_mode_test.go's positive proof: with one
	// replica's worker structurally disabled this way, the OTHER replica is
	// the only process that can ever run a delivery Job, which turns "did
	// the notification.inbox.created announcement reach a replica that
	// never executed anything itself" into a genuine, deterministic
	// cross-process EventBus proof rather than a coincidence a shared
	// SQLite jobs table could also explain (see that file's own doc
	// comment for the finding this env var exists to address). Left unset
	// (the default), buildServer's behavior is exactly what it was before
	// this variable existed.
	disableQueueWorkerEnv = "APP_DISABLE_QUEUE_WORKER"

	// disableDemoUserHeaderEnv names the environment variable that, when set
	// to any non-empty value, makes buildServer's rbac wiring stop reading
	// demoUserHeader (demo_subject.go's "X-Demo-User") at all: every gated
	// route resolves its acting Subject from the verified authn Principal
	// alone, through demoSubjectResolverFor(true) -- see that function's own
	// doc comment. This is the kill switch demoUserHeader's own doc comment
	// describes for the header's remaining privilege-escalation hole: an
	// unauthenticated header that still outranks a proven identity when both
	// are present, so a caller holding nothing more than a low-privilege
	// session could set the header to a higher-privileged demo actor's id
	// and have rbac decide against that actor's grants instead of the
	// caller's own. Left unset (the default), this variable changes nothing
	// -- every existing demo journey and every test built around the header
	// winning keeps behaving exactly as it did before this variable existed,
	// which is deliberate: flipping the default would break every one of
	// them at once (demo_subject_test.go, notesRequestAs and friends in
	// server_test.go, and the flow tests across this package that drive a
	// demo actor through the header). An operator deploying this reference
	// app somewhere a real, non-demo user might reach it is the one case
	// this variable exists for: setting it closes the hole with no code
	// change. See DEPLOY.md's own section on this header for the operator-
	// facing version of this same warning.
	disableDemoUserHeaderEnv = "APP_DISABLE_DEMO_USER_HEADER"

	// rootKeyPurposeConfigCipher, rootKeyPurposeOrgIndex,
	// rootKeyPurposeNotificationIndex, rootKeyPurposePKILocalKeyCipher,
	// rootKeyPurposeAuthnBlindIndex and rootKeyPurposeAuthnPIICipher are the
	// six dbkit.DeriveKey purpose strings APP_ROOT_KEY's derivation uses,
	// one per key material rootKeyEnv's doc comment above lists, in the
	// same order. Each is distinct (so no two ever derive the same bytes)
	// and versioned (a trailing ".v1", per DeriveKey's own doc comment on
	// why: a deliberate future re-derivation bumps the suffix rather than
	// editing a string already used in production, which would silently
	// re-derive a different key for whatever it named). Never rename or
	// reuse one of these strings once APP_ROOT_KEY ships to a real
	// deployment -- doing so is operationally identical to rotating that
	// one key without telling anyone.
	rootKeyPurposeConfigCipher      = "speed.reference-app.config.cipher.v1"
	rootKeyPurposeOrgIndex          = "speed.reference-app.org.blind_index.v1"
	rootKeyPurposeNotificationIndex = "speed.reference-app.notification.blind_index.v1"
	rootKeyPurposePKILocalKeyCipher = "speed.reference-app.pki.local_key_cipher.v1"
	rootKeyPurposeAuthnBlindIndex   = "speed.reference-app.authn.blind_index.v1"
	rootKeyPurposeAuthnPIICipher    = "speed.reference-app.authn.pii_cipher.v1"
)

// devConfigKey is the master key used when APP_CONFIG_KEY is unset. It is
// the ascending 0x00..0x1f byte sequence -- a recognizable constant, never
// a secret -- because zero-setup standalone development (`go run
// ./cmd/server`, `task dev`, this app's tests) must work with no
// environment at all, while config's Sensitive items demand a real
// 32-byte key the moment one is declared (Attach fails with
// ErrCipherRequired otherwise).
//
// This default is a documented trade-off, not a pattern to copy: it is a
// key committed to the repository, which real hosts must never do. A real
// deployment must set APP_CONFIG_KEY from a secret store (or refuse to
// start); the constant exists so the *demo* keeps working out of the box,
// and its name and doc comment are the guard rails that keep it honest.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// devOrgIndexKey is the HMAC key used when APP_ORG_INDEX_KEY is unset --
// the descending 0xff..0xe0 byte sequence, chosen precisely so it is
// visibly a DIFFERENT 32 bytes from devConfigKey's ascending 0x00..0x1f
// (see orgIndexKeyEnv's own doc comment for why the two must never be the
// same secret). Like devConfigKey, this is a recognizable constant for
// zero-setup standalone development, never a secret a real deployment
// should keep.
var devOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// devPKILocalKeyCipherKey, devBlindIndexKey and devPIICipherKey are authn's
// (and pki's) own committed-key placeholders, the same documented trade-off
// as devConfigKey immediately above -- a real deployment must replace every
// one of them with real secret-manager material, never commit real keys the
// way this demo commits these. Each now has a real override path
// (pkiLocalKeyCipherKeyEnv / authnBlindIndexKeyEnv / authnPIICipherKeyEnv,
// or APP_ROOT_KEY deriving all three at once -- see rootKeyEnv's own doc
// comment); these three vars are consulted only as resolveKey's devDefault
// fallback in configFromEnv now, never referenced directly by buildServer
// any more.
//
// Each protects something different and each MUST stay stable across
// restarts for a different reason: devPKILocalKeyCipherKey seals go/pki's
// LocalSigner private-key column (pki_local_keys, via
// pki.RegisterLocalKeySerializer) -- authn's signing key itself needs no
// separate dev-seed derivation the way the deleted authn.KeySet default
// once did (this var occupies the byte range devSigningKeySeed used to,
// freed by that deletion): it is generated once by pki.Service.EnsurePurpose
// (this file's authn.WithKeySource wiring below) and PERSISTS in
// cfg.SQLitePath across restarts, exactly the durability
// docs/internal/22-pki.md's post-integration column describes; authn.WithBlindIndexKey's
// key must stay IDENTICAL across restarts or every already-stored
// email/phone blind index becomes unfindable; and devPIICipherKey seals
// authn's encrypted PII columns (email, phone, TOTP secrets) via
// authn.RegisterPIISerializer, deliberately a DIFFERENT key from
// devConfigKey -- dbkit's own key-separation rule (never let one key double
// as two different AEAD constructions) applies across modules, not only
// within one.
var (
	devPKILocalKeyCipherKey = []byte{
		0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
		0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	}
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
)

// devNotificationIndexKey is the HMAC key used when
// APP_NOTIFICATION_INDEX_KEY is unset -- the ascending 0x80..0x9f byte
// sequence, the next free 32-byte region of this file's recognizable
// constants and chosen so it is visibly a DIFFERENT 32 bytes from every key
// above it (see notificationIndexKeyEnv's own doc comment for why the
// notification index key and the config cipher key must never be the same
// secret). Like its siblings, this is a recognizable constant for
// zero-setup standalone development, never a secret a real deployment
// should keep.
var devNotificationIndexKey = []byte{
	0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
	0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
	0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
}

// demoHostTenants is a hard-coded, obviously-temporary Host -> TenantID
// lookup. It exists only so this reference app has *some* way to render a
// tenant-specific brand on the config module's pre-auth display endpoints
// (configModule's tenancy.NewDomainResolver wiring in buildServer below,
// go/tenancy/AGENTS.md's "Why there is no JWTResolver here") without a real
// custom-domain table.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real Resolver must derive the tenant from a source the server itself
// controls -- never an unauthenticated, static Host map like this one,
// which anyone can trigger just by setting the Host header on an HTTP
// request. See go/tenancy/resolver.go's own Resolver doc comment for the
// same rule stated as a hard requirement on every implementation.
//
// Host does NOT select the tenant for anything else in this app. The notes
// API, and every other route this app protects, resolve their tenant from
// the caller's ACCESS TOKEN instead (authn.NewPrincipalResolver, wired
// below) -- an unauthenticated caller cannot choose a tenant just by
// setting Host, which a Host-keyed lookup like this one would otherwise
// allow. See buildServer's middleware-chain doc comment for the full
// reasoning and go/authn/AGENTS.md's "The middleware chain is authn, then tenancy" section for
// why the chain runs authn.Middleware before tenancy.Middleware at all.
var demoHostTenants = map[string]pkgcore.TenantID{
	"acme.demo.localhost":   "tenant-acme",
	"globex.demo.localhost": "tenant-globex",
}

// demoMemberships is this app's small, in-process stand-in for the org
// module's real membership store -- authn's MembershipReader seam
// (go/authn/service.go), seeded by hand rather than backed by a real
// organizations table. Root CLAUDE.md's "org / rbac rounds" deferral is why
// there is no real one to wire yet.
//
// It starts empty, and who fills it depends on the boot:
//
//   - Every boot seeds the fixed demo header actors' rbac grants
//     (seedDemoGrants) but NO memberships: those actors have no database
//     row, so nothing can sign in as them.
//   - A boot with APP_DEMO_USERS_PASSWORD set additionally registers the
//     three demo accounts of demo_users.go through the real register route
//     and records their memberships here -- which is what makes those real
//     sign-ins succeed (authn's resolveTenant refuses an account with no
//     membership: go/authn/service.go's nil-or-unseeded MembershipReader
//     answer refuses rather than allows, and this store is exactly that
//     unseeded case on a boot that skipped the seed).
//   - Tests grant membership explicitly after registering an account
//     through the real HTTP surface (registerAndAuthenticate in
//     server_test.go, authn_e2e_test.go), keeping a reference to the same
//     store buildServer itself wires.
//
// Because this store is in-process, a membership never survives a restart:
// an account registered by an earlier boot answers "not a member" on the
// next one and sign-in fails closed -- the honest state for an account
// whose seed cannot be replayed (demo_users.go documents why), not a bug
// to paper over.
type demoMemberships struct {
	mu      sync.Mutex
	tenants map[string][]pkgcore.TenantID
}

// newDemoMemberships returns an empty membership store.
func newDemoMemberships() *demoMemberships {
	return &demoMemberships{tenants: make(map[string][]pkgcore.TenantID)}
}

// Grant records userID as an active member of tenant. It is idempotent:
// granting the same pair twice does not duplicate the entry.
func (m *demoMemberships) Grant(userID string, tenant pkgcore.TenantID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.tenants[userID] {
		if existing == tenant {
			return
		}
	}
	m.tenants[userID] = append(m.tenants[userID], tenant)
}

// ActiveMembership implements authn.MembershipReader.
func (m *demoMemberships) ActiveMembership(_ context.Context, userID string, tenant pkgcore.TenantID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.tenants[userID] {
		if existing == tenant {
			return true, nil
		}
	}
	return false, nil
}

// TenantsOf implements authn.MembershipReader.
func (m *demoMemberships) TenantsOf(_ context.Context, userID string) ([]pkgcore.TenantID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pkgcore.TenantID(nil), m.tenants[userID]...), nil
}

// compile-time check that *demoMemberships satisfies authn.MembershipReader.
var _ authn.MembershipReader = (*demoMemberships)(nil)

// demoOrgUserHeader is the header demoOrgSubjectResolver reads to identify the
// HTTP caller: a placeholder for the verified access-token claims authn
// will eventually supply, in exactly the spirit of demoHostTenants' own
// disclaimer above. A caller sets it to whatever user id it wants to act
// as, with no verification whatsoever -- which is fine for this reference
// app's own demonstration purposes and would be a critical vulnerability
// in any real deployment.
const demoOrgUserHeader = "X-Demo-User-Id"

// demoOrgSubjectResolver stands in for the SubjectResolver authn will
// eventually supply from a verified access token's claims, serving the
// two modules that declare the same structurally identical seam and keep
// their caller identity header-only in this app: org's two caller-scoped
// endpoints (creating and accepting an invitation) and every notification
// endpoint, which resolves its caller's inbox, contacts and preferences
// through it. It exists only so this reference app has *some* way to
// demonstrate those endpoints end to end before authn exists.
//
// Who a caller is is the X-Demo-User-Id header value, and nothing else:
// this resolver deliberately never falls back to the verified Principal
// authn.Middleware leaves in the request context. The org-web round's own
// rule governs -- an endpoint that resolves its caller from an
// unauthenticated, client-supplied header is scaffold, and a browser
// whose requests carry no demo header is refused until the resolver reads
// a real token -- and this app's flows pinned that refusal as the
// notification surface's identity gate (notification_flow_test.go's
// subject-less leg; demoRouteGuards names the path routePublic for the
// same reason): an authenticated caller with no demo header gets the
// module's own per-operation 401 (notification.subject_unresolved and
// org's sibling), never a fabricated user id. Notes' create handler is
// the one seam that additionally accepts principals -- it resolves
// through demoNotesSubjectResolver below, not through this type.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real SubjectResolver must derive the caller from a source the server
// itself verified (a validated access token's subject claim), never an
// unauthenticated, client-supplied header like this one -- see
// org.SubjectResolver's own doc comment for the same rule stated as a hard
// requirement.
type demoOrgSubjectResolver struct{}

// Subject implements org.SubjectResolver, notification.SubjectResolver and
// integration.SubjectResolver -- the third round-4 wired this same type
// onto, since all three seams share the identical
// (r *http.Request) (string, bool) shape with the identical fail-closed
// contract, and this app already has one instance to hand each of them. It
// fails closed: no header reports ("", false), and the module's own
// per-operation refusal (notification.subject_unresolved,
// integration.subject_unresolved, org's sibling) is what a caller then
// sees.
func (demoOrgSubjectResolver) Subject(r *http.Request) (string, bool) {
	userID := r.Header.Get(demoOrgUserHeader)
	return userID, userID != ""
}

// compile-time checks that demoOrgSubjectResolver satisfies the identical
// SubjectResolver seam the three modules it serves declare.
var (
	_ org.SubjectResolver          = demoOrgSubjectResolver{}
	_ notification.SubjectResolver = demoOrgSubjectResolver{}
	_ integration.SubjectResolver  = demoOrgSubjectResolver{}
)

// demoNotesSubjectResolver is what notes' create handler resolves the
// creating user from -- the notes.NewModule option buildServer wires
// below. It is demoOrgSubjectResolver's behavior plus one source: like
// its sibling it reads the X-Demo-User-Id header first, the attribution
// affordance every flow helper in this package sends and the namespace
// demo_notification.go's address table keys on; and only when no header
// is present does it fall back to the verified Principal authn.Middleware
// left in the request context. The fallback is what lets a browser-shaped
// caller with no demo header create notes: the accounts demo_users.go
// seeds (real users acting through their access tokens) are attributed
// through it, exactly as they pass the rbac gate through demoSubjectResolver's
// own fallback, and the note-created events their creates publish name
// their real user ids -- which resolve to no notification addresses, an
// ordinary skip (see demo_notification.go's demoUserAddresses).
//
// Only notes' creator seam gets this second source. Org's and the
// notification module's caller-scoped endpoints keep demoOrgSubjectResolver's
// header-only read -- notification because its subject-less refusal is a
// pinned behaviour of this app's rig (see demoOrgSubjectResolver's own
// comment), org because its browser-shaped flows are the org-web round's
// work -- and a request that reaches those surfaces without the header
// stays refused rather than acting as the principal's user id.
//
// This is a placeholder, not a pattern to copy into a real deployment:
// the header is exactly as unverifiable here as in demoOrgSubjectResolver,
// and a real deployment's resolver reads the creating user from the
// verified token the notes create handler already stands behind.
type demoNotesSubjectResolver struct{}

// Subject implements notes.SubjectResolver. It fails closed: no header
// and no Principal reports ("", false), and the module's own per-operation
// refusal (notes.subject_unresolved) is what a caller then sees.
func (demoNotesSubjectResolver) Subject(r *http.Request) (string, bool) {
	userID := r.Header.Get(demoOrgUserHeader)
	if userID == "" {
		principal, ok := authn.PrincipalFromContext(r.Context())
		if !ok || principal.UserID == "" {
			return "", false
		}
		userID = principal.UserID
	}
	return userID, true
}

// compile-time check that demoNotesSubjectResolver satisfies notes' own
// copy of the seam.
var _ notes.SubjectResolver = demoNotesSubjectResolver{}

// orgFeatureGate adapts a *config.Service that is filled in AFTER this
// app's org.Module is constructed into org.FeatureGate, read lazily -- the
// same "read a host seam at call time, never capture it at construction"
// idiom go/org's own hostSeams applies throughout the module (see
// go/org/events.go's doc comment on hostSeams for the identical reasoning).
//
// It exists because of a real ordering constraint in buildServer: the
// config module's Service is only produced by configModule.Attach, which
// per its own contract runs strictly AFTER Kernel.Bootstrap returns -- and
// org.Module must already be part of that same Bootstrap call so its
// permissions, audit actions, events and routes are declared. Passing the
// *config.Service variable directly to org.WithFeatureGate before Attach
// has run would capture a non-nil FeatureGate interface wrapping a nil
// *config.Service pointer, which panics the moment anything calls
// IsEnabled on it. Holding a pointer to the variable instead, and
// dereferencing it only when IsEnabled is actually called (during a real
// HTTP request, long after buildServer has finished wiring), sidesteps the
// ordering problem entirely.
type orgFeatureGate struct{ service **config.Service }

// IsEnabled implements org.FeatureGate.
func (g orgFeatureGate) IsEnabled(ctx context.Context, key string) (bool, error) {
	svc := *g.service
	if svc == nil {
		return false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return svc.IsEnabled(ctx, key)
}

// compile-time check that orgFeatureGate satisfies org.FeatureGate.
var _ org.FeatureGate = orgFeatureGate{}

// orgSubtreeResolver adapts org.Scope's Path method onto rbac.SubtreeResolver's
// NodePath -- the two no-import seams differ just enough (three return
// values vs. two; "not found" folded into an error vs. a plain boolean)
// that a one-line wrapper is needed, unlike orgFeatureGate/FeatureGate's
// exact structural match above. org.Scope is safe to hold directly (unlike
// *config.Service above): orgModule.Scope() is available the moment
// org.NewModule returns, with no Attach-ordering constraint, so this
// adapter captures it at construction rather than reading it lazily.
type orgSubtreeResolver struct{ scope org.Scope }

// NodePath implements rbac.SubtreeResolver.
func (r orgSubtreeResolver) NodePath(ctx context.Context, nodeID string) (string, bool, error) {
	path, err := r.scope.Path(ctx, nodeID)
	if err != nil {
		if appErr, ok := apperr.As(err); ok && appErr.Code == org.ErrNodeNotFound.Code {
			// rbac's own contract: an unresolvable node DENIES the binding
			// that named it rather than erroring, so the caller's Can/
			// DataScope decision reports "no such node" as ok == false,
			// never widening to the tenant (SubtreeResolver's own doc
			// comment).
			return "", false, nil
		}
		return "", false, err
	}
	return path, true, nil
}

// compile-time check that orgSubtreeResolver satisfies rbac.SubtreeResolver.
var _ rbac.SubtreeResolver = orgSubtreeResolver{}

// readTenantDurationConfig is the shared core sharingConfigReader and
// complianceConfigReader below both call into: it resolves key's
// tenant-then-system-then-schema-default effective value through cfg
// (go/config's own three-tier resolution, Service.Get's own doc comment)
// and reports ok = false when it resolved at the schema default -- i.e.
// neither a tenant nor a system row exists at all -- exactly the
// "tenant has configured none" signal both go/sharing's TenantConfigReader
// and go/compliance's ExportDeliveryExpiryReader document. Using Get
// (rather than the type-erasing config.GetTyped) is what makes that
// distinction visible at all: GetTyped throws away Value.Scope, so it
// cannot tell a genuinely unset key from one that happens to resolve to
// its own declared Default.
func readTenantDurationConfig(ctx context.Context, cfg *config.Service, key string, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	tenantCtx := pkgcore.WithTenant(ctx, tenant)
	v, err := cfg.Get(tenantCtx, key)
	if err != nil {
		return 0, false, err
	}
	if v.Scope == "" {
		// Resolved to the schema default: no explicit tenant or system row
		// exists, so report "unconfigured" and let the caller's own
		// fallback (identical to that same schema Default) apply, exactly
		// as if no reader were wired at all.
		return 0, false, nil
	}
	d, ok := v.Data.(time.Duration)
	if !ok {
		return 0, false, fmt.Errorf("reference-app: config key %q did not resolve to a duration value", key)
	}
	return d, true, nil
}

// sharingConfigReader adapts a *config.Service that is filled in AFTER
// this app's sharing.Module is constructed into sharing.TenantConfigReader,
// read lazily -- the identical ordering problem and the identical fix
// orgFeatureGate's own doc comment explains at length: configModule.Attach
// (which produces the real *config.Service) runs strictly after
// Kernel.Bootstrap returns, and sharing.Module must already be part of
// that same Bootstrap call, so sharing.WithTenantConfigReader has to
// receive something today that becomes live only later. Holding a
// pointer to the configService variable, and dereferencing it only when
// ShareDefaultExpiry is actually called (during a real request, long
// after buildServer has finished wiring), sidesteps the ordering problem
// exactly like orgFeatureGate does for org.FeatureGate.
type sharingConfigReader struct{ service **config.Service }

// ShareDefaultExpiry implements sharing.TenantConfigReader.
func (r sharingConfigReader) ShareDefaultExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	svc := *r.service
	if svc == nil {
		return 0, false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return readTenantDurationConfig(ctx, svc, sharing.ConfigDefaultExpiry, tenant)
}

// compile-time check that sharingConfigReader satisfies
// sharing.TenantConfigReader.
var _ sharing.TenantConfigReader = sharingConfigReader{}

// complianceConfigReader is compliance's own copy of sharingConfigReader,
// adapting the same lazily-filled *config.Service into
// compliance.ExportDeliveryExpiryReader. It is a separate type, not a
// shared one, because the two seams are structurally different Go
// interfaces (ShareDefaultExpiry vs ExportDeliveryExpiry, per each
// module's own declared method name) even though their bodies both do
// nothing but call readTenantDurationConfig with a different config
// key -- forcing one type to implement both method names would be a
// coincidental unification neither module's own design asked for.
type complianceConfigReader struct{ service **config.Service }

// ExportDeliveryExpiry implements compliance.ExportDeliveryExpiryReader.
func (r complianceConfigReader) ExportDeliveryExpiry(ctx context.Context, tenant pkgcore.TenantID) (time.Duration, bool, error) {
	svc := *r.service
	if svc == nil {
		return 0, false, fmt.Errorf("reference-app: the config service is not attached yet")
	}
	return readTenantDurationConfig(ctx, svc, compliance.ConfigExportDeliveryExpiry, tenant)
}

// compile-time check that complianceConfigReader satisfies
// compliance.ExportDeliveryExpiryReader.
var _ compliance.ExportDeliveryExpiryReader = complianceConfigReader{}

// serverConfig is main.go's own bootstrap wiring configuration -- the
// values a process must know before anything else can start (deployment
// mode, port, database path, the config master key, the optional Redis
// address, the demo host map). It is a plain struct read from the
// environment by configFromEnv, NOT the dynamic configuration the config
// module serves: dynamic configuration lives in the configs table and can
// never hold the very key that encrypts it, so this bootstrap struct is
// the deliberate exception to "a plain struct, not pkgcore/config's
// dynamic configuration" -- it is main.go's own wiring, which never goes
// through Module.Register either.
type serverConfig struct {
	DeploymentMode       pkgcore.DeploymentMode
	Port                 string
	SQLitePath           string
	ConfigKey            []byte
	OrgIndexKey          []byte
	NotificationIndexKey []byte

	// PKILocalKeyCipherKey, AuthnBlindIndexKey and AuthnPIICipherKey are
	// the three key materials that had NO environment-variable override
	// path at all before this round -- see pkiLocalKeyCipherKeyEnv's,
	// authnBlindIndexKeyEnv's and authnPIICipherKeyEnv's own doc comments
	// above for what each protects and why each is a separate secret.
	// configFromEnv resolves all six key fields on this struct (these
	// three plus ConfigKey/OrgIndexKey/NotificationIndexKey above) through
	// the same three-tier precedence: an explicitly-set individual
	// environment variable wins over a APP_ROOT_KEY derivation, which
	// wins over the hardcoded development default.
	PKILocalKeyCipherKey []byte
	AuthnBlindIndexKey   []byte
	AuthnPIICipherKey    []byte

	RedisAddr   string
	HostTenants map[string]pkgcore.TenantID

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region and S3UseSSL
	// compose a real S3-compatible ObjectStore for the "objectstore" seam
	// (objectstore/s3.NewObjectStore) when S3Endpoint is non-empty --
	// s3EndpointEnv's own doc comment above has the completeness rule.
	// Empty S3Endpoint (the default) leaves "objectstore" on the Preset's
	// local-directory default.
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3UseSSL    bool

	// ObjectStoreRoot, when non-empty, replaces the "objectstore" seam's
	// Preset default with pkgcore.NewLocalObjectStore over this fixed
	// directory -- the local twin of the S3 fields above, and an
	// alternative to them (configFromEnv refuses both a complete S3
	// composition and APP_OBJECT_STORE_ROOT, per objectStoreRootEnv's own
	// doc comment). buildServer injects it declaring the SurvivesRestart
	// capability alone, never MultiReplicaSafe: the directory survives a
	// process restart, but nothing about a single-process local store is
	// replica-safe. The field exists because a host that needs its objects
	// to outlive one process must name the directory itself -- the Preset
	// default is a throwaway MkdirTemp -- which is exactly what the
	// two-boot expiry-sweep flow test (periodic_scheduler_flow_test.go)
	// needs: boot 2's sweep must find the bytes boot 1 wrote.
	ObjectStoreRoot string

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword compose a real SMTP
	// Mailer for the "mailer" seam (pkgcore.NewSMTPMailer) when SMTPHost is
	// non-empty -- smtpHostEnv's own doc comment above has the completeness
	// rule. Empty SMTPHost (the default) leaves "mailer" on the Preset's
	// console default, exactly like an unset Mailer field below.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// SMSGatewayURL composes authn's real SMS transport
	// (authn.NewHTTPSMSSender) for its "SMS sender" seam when non-empty.
	// See smsGatewayURLEnv's own doc comment above for what an empty value
	// means under each deployment mode.
	SMSGatewayURL string

	// DisableQueueWorker, when true, makes buildServer skip
	// standaloneQueue.Start -- see disableQueueWorkerEnv's own doc comment
	// above for why this exists and what it changes. configFromEnv sets it
	// from APP_DISABLE_QUEUE_WORKER; false (the default) is byte-identical
	// to this field never having existed.
	DisableQueueWorker bool

	// DisableDemoUserHeader, when true, makes buildServer wire every
	// permission-gated route's SubjectResolver through
	// demoSubjectResolverFor(true) instead of the header-first default --
	// see disableDemoUserHeaderEnv's own doc comment above for why this
	// exists and exactly what it changes. configFromEnv sets it from
	// APP_DISABLE_DEMO_USER_HEADER; false (the default) is byte-identical to
	// this field never having existed, matching DisableQueueWorker's own
	// contract just above.
	DisableDemoUserHeader bool

	// PeriodicTaskInterval is the cadence of this host's periodic-task
	// scheduler (periodic_scheduler.go), the ticker that enqueues the
	// jobs-driven mechanisms this app wired -- storage's per-tenant expiry
	// sweep and pki's signing-key expiry scan. configFromEnv never sets
	// it, so zero (the default) means the scheduler's own default,
	// defaultPeriodicTaskSchedulerInterval, and the flow tests inject a
	// sub-second interval to drive real ticks within test time -- the same
	// test-override shape Mailer and Memberships below use.
	PeriodicTaskInterval time.Duration

	// PKIPropagationWindow and PKIRenewalLeadTime override pki's rotation
	// timing when non-zero: buildServer applies pki.WithPropagationWindow
	// and pki.WithRenewalLeadTime only for values above zero, so the zero
	// default (what configFromEnv always leaves them at) keeps the
	// module's own DefaultPropagationWindow / DefaultRenewalLeadTime in
	// force, byte-identical to the fields never having existed. The pki
	// flow test injects a renewal lead time past a signing key's validity
	// so the very next expiry scan stages its replacement within test
	// time.
	PKIPropagationWindow time.Duration
	PKIRenewalLeadTime   time.Duration

	// Mailer overrides the console mailer the standalone Preset resolves
	// for the "mailer" seam when set. configFromEnv sets it to a real
	// pkgcore.NewSMTPMailer composition when SMTPHost is configured (see
	// SMTPHost's doc comment above); it is otherwise nil in production, so
	// this field also exists for server_test.go's org invitation flow
	// test, which needs the rendered mail back in-process to extract the
	// invitation token rather than parsing it out of console output.
	// buildServer injects Mailer with MailerCapabilities below, defaulting
	// to pkgcore.Stateless when that field is left at its zero value --
	// the honest capability for a throwaway test double, and the same
	// declaration this field carried before this round.
	Mailer pkgcore.Mailer

	// MailerCapabilities declares the capability bits buildServer wires
	// Mailer with, when Mailer is non-empty. configFromEnv sets it to
	// pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart -- the capabilities
	// the "mailer.smtp" builtin registration itself declares -- alongside
	// its real SMTP Mailer; every other caller (every existing test's
	// in-process double) leaves it at the zero value, which buildServer
	// treats as pkgcore.Stateless, preserving this field's behavior from
	// before this round.
	MailerCapabilities pkgcore.Capability

	// Memberships is the seam authn asks tenant-membership questions
	// through. Nil defaults to a fresh, empty demoMemberships in
	// buildServer; a test that needs to seed membership after registering
	// a demo user keeps its own reference by setting this field before
	// calling buildServer, rather than reaching into buildServer's
	// internals.
	Memberships *demoMemberships

	// DemoUsersPassword, when non-empty, makes buildServer seed the three
	// demo accounts of demo_users.go at the end of its composition --
	// register each through the composed handler, then grant the
	// membership and role its actor model declares -- so a browser visitor
	// can sign in as demo-owner@example.com and friends with a real
	// account, real membership and real rbac grants, and no demo header.
	// configFromEnv fills it from APP_DEMO_USERS_PASSWORD; the empty
	// default (the zero-external-dependency `go run ./cmd/server`
	// experience) skips the seed entirely.
	DemoUsersPassword string

	// SMSOutput is where authn's console SMS sender (the standalone
	// deployment mode's transport, go/authn/sms.go) writes delivered
	// messages. Nil defaults to os.Stdout.
	SMSOutput io.Writer

	// SocialProviders, RedirectAllowlist and TrustedProviders wire authn's
	// social sign-in channels. All three default to empty/zero, the safe
	// nothing-enabled state this app ships with today: no real OAuth app
	// credentials are configured for this example (dynamic-config
	// read-through for the per-provider credential items authn registers
	// is a documented deferral -- go/authn/AGENTS.md). authn_e2e_test.go
	// supplies a channel pointed at a local httptest server here, to prove
	// the social sign-in flow end to end without a live provider.
	SocialProviders   []authn.SocialProvider
	RedirectAllowlist authn.RedirectAllowlist
	TrustedProviders  []string

	// AIGatewayBaseURL and AIGatewayAPIKey, when AIGatewayAPIKey is
	// non-empty, make buildServer write a platform-wide ai-gateway
	// credential (aigateway.CredentialService.SetPlatformCredential) for
	// aigateway.ProviderOpenAICompatible at boot, so the consult module's
	// one route (cmd/server/consult.go) can actually reach a provider.
	// configFromEnv never sets either -- there is no real OpenAI-compatible
	// key committed to this repository, the same posture SocialProviders'
	// own doc comment above describes -- so the zero-setup `go run
	// ./cmd/server` experience leaves the consult route permanently
	// answering aigateway.ErrCredentialNotFound until an operator wires a
	// real key. consult_flow_test.go is what sets both: AIGatewayBaseURL to
	// an httptest.Server standing in for the OpenAI-compatible endpoint,
	// and AIGatewayAPIKey to a fixed test value, exactly the way cfg.Mailer
	// is a test-only override of a seam production leaves on its real
	// default.
	AIGatewayBaseURL string
	AIGatewayAPIKey  string

	// AIGatewayImageBaseURL and AIGatewayImageAPIKey are the image-side
	// mirror of AIGatewayBaseURL/AIGatewayAPIKey, above: when
	// AIGatewayImageAPIKey is non-empty, buildServer writes a second
	// platform-wide ai-gateway credential for
	// aigateway.ProviderOpenAICompatibleImage at boot, so the smilesim
	// module's routes (cmd/server/smilesim.go) can actually reach an image
	// provider. The two credentials are deliberately independent rows of
	// the SAME ai_gateway_credentials table (keyed by provider name) --
	// see go/ai-gateway/AGENTS.md's round-2 section on why chat and image
	// credentials need no schema change to coexist. configFromEnv never
	// sets either, the identical zero-setup posture AIGatewayAPIKey's own
	// doc comment describes; smilesim_flow_test.go is what sets both.
	AIGatewayImageBaseURL string
	AIGatewayImageAPIKey  string

	// WebhookURLValidator and WebhookHTTPClient override go/integration's
	// two SSRF enforcement points together (integration.
	// WithWebhookURLValidator's own doc comment explains why the two must
	// always move together) for ONE Module instance this buildServer call
	// composes -- never a production weakening, since configFromEnv never
	// sets either and buildServer leaves both options unset (the module's
	// own strict default) whenever WebhookURLValidator is nil. This exists
	// for webhook_flow_test.go alone: it is this app's only way to prove a
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

	// OnRBACReady, when non-nil, receives the live *rbac.Service buildServer
	// attaches, immediately after seedDemoGrants seeds every configured
	// tenant's built-in roles and demo grants. It exists purely for a test
	// that needs to grant a role scoped to an organization node CREATED
	// AFTER the server starts serving HTTP -- the org-route-guards round's
	// subtree-scoped grant test (org_route_guards_test.go), which cannot
	// know a node's id at boot time, since org builds its tree through real
	// HTTP calls the test itself drives once the server is up. Nil in every
	// production boot and every other test is a complete no-op, mirroring
	// WebhookURLValidator's identical "test-only override of what buildServer
	// already wires, never a production weakening" shape above.
	OnRBACReady func(*rbac.Service)
}

// parseHexKeyEnv decodes encoded -- envName's raw value -- as a hex-encoded
// 32-byte key, returning a precise error naming envName when encoded is not
// exactly configKeyHexLength hex characters or is not valid hex, rather than
// letting a subtly wrong value surface later as an opaque dbkit.NewCipher /
// dbkit.NewBlindIndexer / dbkit.DeriveKey error. Every one of this file's
// six key-material environment variables, plus rootKeyEnv itself, share
// this exact validation.
func parseHexKeyEnv(envName, encoded string) ([]byte, error) {
	if len(encoded) != configKeyHexLength {
		return nil, fmt.Errorf(
			"reference-app: %s must hold %d hex characters (a 32-byte key), got %d",
			envName, configKeyHexLength, len(encoded))
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("reference-app: %s: %w", envName, err)
	}
	return decoded, nil
}

// resolveKey applies the three-tier precedence configFromEnv uses for every
// one of the six key materials rootKeyEnv's own doc comment lists: an
// explicitly-set individualEnv always wins, over a rootKey-derived value
// (dbkit.DeriveKey(rootKey, purpose), computed only when rootKey is
// non-nil -- i.e. APP_ROOT_KEY was set), which in turn always wins over
// devDefault, the hardcoded development fallback that existed before this
// round and still applies unchanged when neither rootKey nor individualEnv
// is set. This precedence lets a deployment set one root secret and still
// override any single derived key independently, for a fine-grained
// rotation cadence that key alone needs (go/dbkit/AGENTS.md's "Key
// derivation" section has the full rationale).
func resolveKey(rootKey []byte, purpose, individualEnv string, devDefault []byte) ([]byte, error) {
	key := devDefault
	if rootKey != nil {
		derived, err := dbkit.DeriveKey(rootKey, purpose)
		if err != nil {
			return nil, fmt.Errorf("reference-app: derive %s from %s: %w", individualEnv, rootKeyEnv, err)
		}
		key = derived
	}
	if encoded := os.Getenv(individualEnv); encoded != "" {
		decoded, err := parseHexKeyEnv(individualEnv, encoded)
		if err != nil {
			return nil, err
		}
		key = decoded
	}
	return key, nil
}

// configFromEnv reads serverConfig from the environment, defaulting to the
// standalone deployment mode on SQLite so `go run ./cmd/server` genuinely
// starts a working server with zero external dependencies.
func configFromEnv() (serverConfig, error) {
	deploymentModeStr := os.Getenv("APP_DEPLOYMENT_MODE")
	if deploymentModeStr == "" {
		deploymentModeStr = string(pkgcore.DeploymentModeStandalone)
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(deploymentModeStr)
	if err != nil {
		return serverConfig{}, err
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv("APP_DB_PATH")
	if dbPath == "" {
		dbPath = defaultSQLitePath
	}

	// redisAddr stays empty when unset (or explicitly emptied): the
	// standalone composition then resolves the "eventbus" seam from the
	// Preset to the in-process bus, keeping the zero-external-dependency
	// default intact.
	redisAddr := os.Getenv(redisAddrEnv)

	// The root secret: APP_ROOT_KEY when set (a hex-encoded 32-byte key --
	// see rootKeyEnv's own doc comment), nil otherwise. nil is the signal
	// resolveKey below reads as "no root key configured" -- every one of
	// the six key materials then falls back to its own hardcoded
	// development default exactly as if this round had never landed.
	var rootKey []byte
	if encoded := os.Getenv(rootKeyEnv); encoded != "" {
		decoded, decodeErr := parseHexKeyEnv(rootKeyEnv, encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		rootKey = decoded
	}

	// Each of the six key materials this app assembles resolves through
	// the identical three-tier precedence: an explicitly-set individual
	// environment variable wins over what APP_ROOT_KEY would derive for
	// it, which wins over the hardcoded development default -- see
	// resolveKey's own doc comment and rootKeyEnv's above for the full
	// rationale. A malformed individual value fails startup with a precise
	// message rather than surfacing later as an opaque cipher error;
	// hex.DecodeString rejects anything that is not valid
	// lowercase-or-uppercase hex, and the length check parseHexKeyEnv runs
	// first rejects anything that does not decode to exactly 32 bytes.
	configKey, err := resolveKey(rootKey, rootKeyPurposeConfigCipher, configKeyEnv, devConfigKey)
	if err != nil {
		return serverConfig{}, err
	}
	orgIndexKey, err := resolveKey(rootKey, rootKeyPurposeOrgIndex, orgIndexKeyEnv, devOrgIndexKey)
	if err != nil {
		return serverConfig{}, err
	}
	notificationIndexKey, err := resolveKey(rootKey, rootKeyPurposeNotificationIndex, notificationIndexKeyEnv, devNotificationIndexKey)
	if err != nil {
		return serverConfig{}, err
	}
	pkiLocalKeyCipherKey, err := resolveKey(rootKey, rootKeyPurposePKILocalKeyCipher, pkiLocalKeyCipherKeyEnv, devPKILocalKeyCipherKey)
	if err != nil {
		return serverConfig{}, err
	}
	authnBlindIndexKey, err := resolveKey(rootKey, rootKeyPurposeAuthnBlindIndex, authnBlindIndexKeyEnv, devBlindIndexKey)
	if err != nil {
		return serverConfig{}, err
	}
	authnPIICipherKey, err := resolveKey(rootKey, rootKeyPurposeAuthnPIICipher, authnPIICipherKeyEnv, devPIICipherKey)
	if err != nil {
		return serverConfig{}, err
	}

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the Preset's local-directory
	// default; when any one of them is set, all four are required --
	// s3EndpointEnv's own doc comment explains why a partial S3 target is
	// refused rather than silently ignored.
	s3Endpoint := os.Getenv(s3EndpointEnv)
	s3Bucket := os.Getenv(s3BucketEnv)
	s3AccessKey := os.Getenv(s3AccessKeyEnv)
	s3SecretKey := os.Getenv(s3SecretKeyEnv)
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		var missing []string
		if s3Endpoint == "" {
			missing = append(missing, s3EndpointEnv)
		}
		if s3Bucket == "" {
			missing = append(missing, s3BucketEnv)
		}
		if s3AccessKey == "" {
			missing = append(missing, s3AccessKeyEnv)
		}
		if s3SecretKey == "" {
			missing = append(missing, s3SecretKeyEnv)
		}
		if len(missing) > 0 {
			return serverConfig{}, fmt.Errorf(
				"reference-app: an S3 ObjectStore composition needs %s set too (got some but not all of %s/%s/%s/%s)",
				strings.Join(missing, ", "), s3EndpointEnv, s3BucketEnv, s3AccessKeyEnv, s3SecretKeyEnv)
		}
	}
	s3UseSSL := false
	if raw := os.Getenv(s3UseSSLEnv); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("reference-app: %s must be a valid bool, got %q: %w", s3UseSSLEnv, raw, parseErr)
		}
		s3UseSSL = parsed
	}

	// objectStoreRoot is the local-directory twin of the S3 composition
	// above: unset leaves "objectstore" on the Preset's throwaway
	// temp-directory default, set names a fixed directory whose contents
	// survive this process's restart (objectStoreRootEnv's own doc
	// comment has the capability reasoning). Both compositions at once
	// would name two different stores for the one seam, so a non-empty
	// root alongside a complete S3 target is refused rather than
	// silently preferring one.
	objectStoreRoot := os.Getenv(objectStoreRootEnv)
	if objectStoreRoot != "" && s3Endpoint != "" {
		return serverConfig{}, fmt.Errorf(
			"reference-app: %s and an APP_S3_* composition name two different ObjectStores for one seam; set only one of them",
			objectStoreRootEnv)
	}

	// smtpHost/smtpPortRaw mirror s3Endpoint/... above: both unset leaves
	// the "mailer" seam on the Preset's console default, and a partial
	// APP_SMTP_* set is refused rather than silently ignored.
	smtpHost := os.Getenv(smtpHostEnv)
	smtpPortRaw := os.Getenv(smtpPortEnv)
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
		// Both unset: the "mailer" seam stays on its Preset default.
	case smtpHost == "" || smtpPortRaw == "":
		return serverConfig{}, fmt.Errorf(
			"reference-app: an SMTP Mailer composition needs both %s and %s set", smtpHostEnv, smtpPortEnv)
	default:
		parsed, parseErr := strconv.Atoi(smtpPortRaw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("reference-app: %s must be a valid port number, got %q: %w", smtpPortEnv, smtpPortRaw, parseErr)
		}
		smtpPort = parsed
	}

	cfg := serverConfig{
		DeploymentMode:        deploymentMode,
		Port:                  port,
		SQLitePath:            dbPath,
		ConfigKey:             configKey,
		OrgIndexKey:           orgIndexKey,
		NotificationIndexKey:  notificationIndexKey,
		PKILocalKeyCipherKey:  pkiLocalKeyCipherKey,
		AuthnBlindIndexKey:    authnBlindIndexKey,
		AuthnPIICipherKey:     authnPIICipherKey,
		RedisAddr:             redisAddr,
		S3Endpoint:            s3Endpoint,
		S3Bucket:              s3Bucket,
		S3AccessKey:           s3AccessKey,
		S3SecretKey:           s3SecretKey,
		S3Region:              os.Getenv(s3RegionEnv),
		S3UseSSL:              s3UseSSL,
		ObjectStoreRoot:       objectStoreRoot,
		SMTPHost:              smtpHost,
		SMTPPort:              smtpPort,
		SMTPUsername:          os.Getenv(smtpUsernameEnv),
		SMTPPassword:          os.Getenv(smtpPasswordEnv),
		SMSGatewayURL:         os.Getenv(smsGatewayURLEnv),
		DisableQueueWorker:    os.Getenv(disableQueueWorkerEnv) != "",
		DisableDemoUserHeader: os.Getenv(disableDemoUserHeaderEnv) != "",
		HostTenants:           demoHostTenants,
		// Empty when unset: the demo-user seed is opt-in (its own doc
		// comment in demo_users.go says why the default skips it).
		DemoUsersPassword: os.Getenv(demoUsersPasswordEnv),
	}
	if smtpHost != "" {
		// A real SMTP composition: declare the capabilities the
		// "mailer.smtp" builtin registration itself declares
		// (builtin_implementations.go), so this app's own env-driven
		// SMTP wiring is capability-honest rather than borrowing the
		// Stateless declaration server_test.go's in-process double uses
		// (Mailer's own doc comment above explains the split).
		cfg.Mailer = pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
			Host:     smtpHost,
			Port:     smtpPort,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
		})
		cfg.MailerCapabilities = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart
	}
	return cfg, nil
}

// buildServer wires the reference app's Kernel -- the authn, notes, org,
// config, rbac, storage, demo, notification, ai-gateway and audit Modules --
// their migrations, the job queue the storage and notification modules
// share, the demo notification glue (cmd/server/demo_notification.go), the
// consult glue (cmd/server/consult.go, go/ai-gateway's mandatory first
// consumer), and the authn+tenancy middleware chain into a single
// http.Handler. It is the one place that wiring logic lives -- main() and
// the end-to-end tests (server_test.go, authn_e2e_test.go,
// org_flow_test.go, storage_flow_test.go, notification_flow_test.go and
// consult_flow_test.go) all call it, so the two can never drift into
// testing a different wiring than the one that actually runs.
//
// It returns the composed handler, a cleanup function that closes
// everything buildServer opened (the services, the injected Redis bus and
// its client when one was built, and the underlying database connection),
// and the wired *compliance.Module -- the compliance_flow_test.go's one
// reach into the retention/erasure/export services this app bootstraps,
// since buildServer exposes no module itself otherwise. The caller must
// call cleanup once done with the handler.
//
// The deployment mode no longer refuses anything here: the Kernel it
// bootstraps is what validates the assembled composition against
// cfg.DeploymentMode, failing startup with pkgcore's own capability error
// (ErrCapabilityUnsatisfied) when the resolved composition cannot run in
// the declared mode. Every stateful seam this app knows about -- eventbus,
// kv, mailer, objectstore, plus authn's own "SMS sender" seam -- can now be
// pointed at a real, MultiReplicaSafe-capable implementation through the
// environment variables configFromEnv reads (redisAddrEnv, s3EndpointEnv
// and friends, objectStoreRootEnv, smtpHostEnv and friends,
// smsGatewayURLEnv); every one of
// them defaults to the standalone Preset's in-process implementation when
// unset, so a plain `go run ./cmd/server` is unaffected by this round. With
// none of them set, the distributed deployment mode still always fails
// capability validation, naming the first unsatisfied seam in resolution
// order ("eventbus" first, per Kernel.Bootstrap's fixed order); with all of
// them set to a genuinely MultiReplicaSafe composition (real Redis, real
// S3-compatible storage, real SMTP, and authn's real HTTP SMS transport),
// the distributed deployment mode now genuinely BOOTS, which is the
// property this file's own TestBuildServer_DistributedDeploymentMode_*
// tests pin the positive half of, and
// examples/reference-app/integration_test/distributed_mode_test.go proves
// end to end against real Docker-backed infrastructure.
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, *compliance.Module, error) {
	// Deliberately NOT setting dbkit.Options.AuditBus here, even though
	// notes.Note implements dbkit.Auditable (see model.go): every note
	// write in this app goes through dbkit.Repository[Note], which wraps
	// Create in a WithTenantSession transaction. This USED TO deadlock a
	// same-SQLite-file persister into "database is locked" (SQLITE_BUSY)
	// on every single note creation, confirmed empirically while wiring
	// this app, because dbkit.auditCapturePlugin's write-capture callback
	// published synchronously, *inside* that still-open transaction, so a
	// persister (audit.Module, below) writing into this SAME SQLite file
	// tried to open a second write session against a database that
	// already held an uncommitted write transaction on the very same OS
	// thread. A later round removed that hazard for exactly this shape:
	// the plugin now buffers its captured events on the write's own
	// context and WithTenantSession publishes them only once its own
	// transaction has genuinely committed, reproduced and proven closed
	// against two real connections to one real SQLite file in
	// go/dbkit/audit_capture_test.go's
	// TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks.
	// See go/dbkit/AGENTS.md's "Audit trail collection" section (Known
	// limitation, now marked resolved for this shape) for the full
	// write-up.
	//
	// This app still does not wire AuditBus here, though, since a
	// genuinely separate persister connection remains the simpler,
	// clearer choice even with the deadlock gone -- not because the
	// automatic mechanism would still fail. It instead persists its audit
	// trail through the declarative audit.Emit call notes/handler.go's
	// NotesCreateNote makes explicitly -- after h.repo.Create has already
	// returned, i.e. after that transaction has committed, which is why
	// Emit's call site never depended on the fix above in the first
	// place.
	//
	// authn's own PII columns (email, phone, TOTP secrets) must have their
	// serializer registered BEFORE dbkit.Open: GORM resolves a model's
	// serializer while it parses the schema, and this module's registry is
	// process-global (authn.RegisterPIISerializer's own doc comment; trap
	// #9 of this round's frozen plan).
	piiCipher, err := dbkit.NewCipher(cfg.AuthnPIICipherKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: register authn's PII serializer: %w", regErr)
	}

	// go/pki's LocalSigner private-key column needs its own serializer
	// registered before dbkit.Open too, for the identical reason
	// authn.RegisterPIISerializer does -- GORM resolves a model's serializer
	// while it parses the schema (pki.RegisterLocalKeySerializer's own doc
	// comment).
	pkiLocalKeyCipher, err := dbkit.NewCipher(cfg.PKILocalKeyCipherKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build pki's local-key cipher: %w", err)
	}
	if regErr := pki.RegisterLocalKeySerializer(pkiLocalKeyCipher); regErr != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: register pki's local-key serializer: %w", regErr)
	}

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: open database: %w", err)
	}

	// configService and rbacService are filled by their Attach calls below
	// (nil until then), standaloneQueue by the pki module's wiring above
	// (nil until then), and smileSimReconcilerStop and
	// periodicTaskSchedulerStop by their start calls below (both nil until
	// then); redisBus and redisClient are filled below when cfg.RedisAddr
	// selects the injected Redis-backed composition (nil otherwise).
	// cleanup closes the services and the job queue first -- stopping the
	// two ticker loops, config's anti-loss poller, rbac's cache janitor
	// and the queue's workers so none of them drains a job, enqueues a
	// task or runs a poll against a connection that is being torn down --
	// then the injected bus, stopping its readers so no remote event can
	// still be delivered to a handler writing the database, then the
	// client this host owns (eventbusredis.EventBus never closes it), and
	// the database last. Every close is attempted even when an earlier one
	// failed; the first error wins.
	var (
		configService             *config.Service
		rbacService               *rbac.Service
		standaloneQueue           *jobs.StandaloneQueue
		redisBus                  *eventbusredis.EventBus
		redisClient               *redis.Client
		smileSimReconcilerStop    func()
		periodicTaskSchedulerStop func()
	)

	cleanup := func() error {
		var firstErr error
		// keepErr records err as the cleanup failure only when it is the
		// first one seen -- every close below is attempted regardless, so
		// neither an early nor a late failure can hide the other. It is a
		// helper rather than an inline "closeErr != nil && firstErr == nil"
		// guard because the very first site would make that guard a
		// tautology (firstErr is provably nil there).
		keepErr := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if smileSimReconcilerStop != nil {
			// Stopped first, before standaloneQueue.Close and the database
			// close below: the reconciler sweep calls both
			// standaloneQueue.Get and the database (through its own
			// smilesim.ReservationStore), so it must not still be ticking
			// against either while they are being torn down.
			smileSimReconcilerStop()
		}
		if periodicTaskSchedulerStop != nil {
			// Stopped next, before standaloneQueue.Close below for the
			// same reason: the scheduler's tick body enqueues onto the
			// queue's task table, so it must not still be ticking while
			// the pool that drains it is being torn down (the blocking
			// stop also guarantees no enqueue is in flight when Close
			// runs -- see periodic_scheduler.go's own doc comment).
			periodicTaskSchedulerStop()
		}
		if configService != nil {
			keepErr(configService.Close())
		}
		if rbacService != nil {
			keepErr(rbacService.Close())
		}
		if standaloneQueue != nil {
			// StandaloneQueue.Close stops the dispatcher and waits for
			// in-flight jobs to finish, bounded by the same timeout that
			// bounds HTTP graceful shutdown. Close is idempotent, so an
			// error path that runs before Start is ever called is safe.
			queueCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			keepErr(standaloneQueue.Close(queueCtx))
			cancel()
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
		return nil, nil, nil, fmt.Errorf("reference-app: build the config master cipher: %w", err)
	}

	// org's Invitation.Email column is encrypted at rest under this same
	// cipher (registered here, before anything touches the Invitation
	// model, since GORM resolves a named serializer at struct-parse time)
	// and made queryable by a SEPARATE HMAC key -- see orgIndexKeyEnv's own
	// doc comment for why reusing cfg.ConfigKey for both would be exactly
	// the AES-key-doubling-as-an-HMAC-key weakness dbkit warns against.
	dbkit.RegisterEncryptedSerializer(org.EmailSerializerName, cipher)
	orgIndexer, err := dbkit.NewBlindIndexer("email_index", cfg.OrgIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the org email indexer: %w", err)
	}

	// notification's Contact.Address column is encrypted at rest under this
	// same config cipher (registered here, before anything touches the
	// Contact model, since GORM resolves a named serializer at struct-parse
	// time) and made queryable by a SEPARATE HMAC key -- see
	// notificationIndexKeyEnv's own doc comment for why reusing
	// cfg.ConfigKey for both would be exactly the AES-key-doubling-as-an-
	// HMAC-key weakness dbkit warns against. One key serves the email and
	// the phone indexers alike (authn's single blind-index key precedent),
	// each named so an error message tells which column it failed on.
	dbkit.RegisterEncryptedSerializer(notification.ContactAddressSerializerName, cipher)
	contactEmailIndexer, err := dbkit.NewBlindIndexer("contact_email_index", cfg.NotificationIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact email indexer: %w", err)
	}
	contactPhoneIndexer, err := dbkit.NewBlindIndexer("contact_phone_index", cfg.NotificationIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact phone indexer: %w", err)
	}

	// ai-gateway's ai_gateway_credentials.api_key column is encrypted at
	// rest under this same config cipher, registered here for the identical
	// "before anything touches the model" reason as org's and notification's
	// registrations immediately above. Unlike those two, no separate HMAC
	// key is needed: a credential is only ever looked up by (provider,
	// scope, tenant_id), never by its own value, so reusing cfg.ConfigKey's
	// cipher carries none of the AES-key-doubling-as-an-HMAC-key risk their
	// comments warn about -- there is no second, HMAC construction here to
	// double as.
	dbkit.RegisterEncryptedSerializer(aigateway.CredentialAPIKeySerializerName, cipher)

	// integration's WebhookSubscription.Secret column is encrypted at rest
	// under this same config cipher, registered here for the identical
	// "before anything touches the model" reason as every registration
	// above. Like a webhook secret must be READ BACK IN PLAINTEXT to sign
	// every delivery attempt (go/integration/AGENTS.md's "Round 2's two
	// tables" section), never merely compared, so no separate HMAC blind
	// index is needed here either -- the identical reasoning
	// aigateway.CredentialAPIKeySerializerName's own registration comment
	// gives.
	dbkit.RegisterEncryptedSerializer(integration.WebhookSecretSerializerName, cipher)

	// hostByTenant is demoHostTenants' reverse index: which demo Host
	// belongs to a given tenant, which is what an invitation's accept link
	// must point at -- InviteService.Accept resolves strictly inside the
	// tenant the REQUEST's own context already carries, never a tenant read
	// out of the token (go/org/invite.go's own doc comment on Accept), so
	// the link has to arrive at that tenant's own entry point to be
	// acceptable at all.
	hostByTenant := make(map[pkgcore.TenantID]string, len(cfg.HostTenants))
	for host, tenant := range cfg.HostTenants {
		hostByTenant[tenant] = host
	}

	// configService is also read here, lazily, by org's feature gate -- see
	// orgFeatureGate's own doc comment for why it cannot be handed
	// configService directly at this point in the wiring.
	orgModule := org.NewModule(db,
		org.WithEmailIndexer(orgIndexer),
		org.WithFeatureGate(orgFeatureGate{service: &configService}),
		org.WithSubjectResolver(demoOrgSubjectResolver{}),
		org.WithMailFrom("invitations@reference-app.example"),
		org.WithInvitationLinkBuilder(func(ctx context.Context, token string) (string, error) {
			tenant, tenantErr := pkgcore.MustTenantFromContext(ctx)
			if tenantErr != nil {
				return "", tenantErr
			}
			host, ok := hostByTenant[tenant]
			if !ok {
				return "", fmt.Errorf("reference-app: no host configured for tenant %q", tenant)
			}
			// This app ships no frontend invitation-acceptance page: its
			// consumer shell (examples/reference-app/web) carries no org
			// views, org's accept flow being demoed at the API level. The
			// link names org's real
			// POST /api/v1/org/invitations/accept endpoint and carries the
			// token as a query parameter purely so it is one recognizable
			// string a person (or, in server_test.go's end-to-end suite, a
			// test) can extract the token back out of -- a real frontend
			// would render its own page at this URL and POST the token
			// from there, as the spec's org_acceptInvitation operation
			// requires.
			return fmt.Sprintf("https://%s/api/v1/org/invitations/accept?token=%s", host, url.QueryEscape(token)), nil
		}),
	)

	// standaloneQueue is this app's one job queue, shared by every module
	// whose asynchronous work runs on the jobs mechanism: a
	// jobs.StandaloneQueue over this app's own database connection, the
	// standalone mode's SQLite-backed worker pool whose task table its
	// own Start creates below (no migration of this host's is involved).
	// It is constructed here, ahead of pkiModule just below -- the first
	// module whose NewModule options need the queue -- and ahead of the
	// storage, integration, notification, ai-gateway, compliance and
	// admin modules that take it as well. Its lifecycle is bound to this
	// host's: the drain loop below moves every registry-declared task
	// handler onto it and Start launches the pool, both after Bootstrap
	// (only then do Register's declarations exist), and cleanup's Close
	// stops the pool before the shared database closes.
	standaloneQueue = jobs.NewStandaloneQueue(db)

	// pki owns authn's signing-key lifecycle: LocalSigner (its own
	// zero-external-dependency default) generates and stores the key in
	// cfg.SQLitePath, so it persists across restarts with no dev-seed
	// derivation required -- see devPKILocalKeyCipherKey's own doc comment.
	// pki is not part of saasctl's --with selection set
	// (docs/internal/22-pki.md's section on where pki sits in saasctl's
	// module selection set),
	// but the reference app assembles authn directly rather than through
	// saasctl, so it wires pki here exactly as any authn-containing
	// generated project's own server.go now does.
	//
	// The queue is part of that wiring too: WithQueue hands pki the
	// standaloneQueue constructed above, which is what makes Register
	// attach the queue to Service and CAService and declare the module's
	// two job handlers onto the registry -- the signing-key expiry-scan
	// task (pki.expiry_scan), which this host's periodic-task scheduler
	// enqueues on its tick, and the CRL-regenerate task, declared and
	// drained like every other registered handler but never scheduled:
	// this app consumes no X.509/CRL surface, so regenerating a CRL
	// nobody reads would be work for its own sake (periodic_scheduler.go's
	// doc comment and go/pki/AGENTS.md record that honestly). The two
	// rotation knobs below are applied only when a test injects them: a
	// zero PKIPropagationWindow or PKIRenewalLeadTime (what configFromEnv
	// always leaves them at) keeps pki's own DefaultPropagationWindow /
	// DefaultRenewalLeadTime in force, byte-identical to this app's
	// pre-round rotation behavior.
	pkiOpts := []pki.Option{pki.WithQueue(standaloneQueue)}
	if cfg.PKIPropagationWindow > 0 {
		pkiOpts = append(pkiOpts, pki.WithPropagationWindow(cfg.PKIPropagationWindow))
	}
	if cfg.PKIRenewalLeadTime > 0 {
		pkiOpts = append(pkiOpts, pki.WithRenewalLeadTime(cfg.PKIRenewalLeadTime))
	}
	pkiModule := pki.NewModule(db, pkiOpts...)

	memberships := cfg.Memberships
	if memberships == nil {
		memberships = newDemoMemberships()
	}
	smsOutput := cfg.SMSOutput
	if smsOutput == nil {
		smsOutput = os.Stdout
	}

	authnOpts := []authn.Option{
		authn.WithKeySource(pkiModule.Service()),
		authn.WithBlindIndexKey(cfg.AuthnBlindIndexKey),
		authn.WithMembershipReader(memberships),
		authn.WithDeploymentMode(cfg.DeploymentMode),
		authn.WithSocialProviders(cfg.SocialProviders...),
		authn.WithRedirectAllowlist(cfg.RedirectAllowlist),
		authn.WithTrustedProviders(cfg.TrustedProviders...),
	}
	// The "SMS sender" seam, following the same conditional-injection shape
	// as every other seam this file wires: a configured gateway URL always
	// wins, under either deployment mode, and composes authn's real HTTP
	// transport (authn.NewHTTPSMSSender); absent that, the standalone
	// deployment mode falls back to the console transport exactly as this
	// app did before this round, while the distributed deployment mode is
	// left deliberately UNWIRED -- authn.NewModule's own newOptions then
	// fails closed with authn.ErrMissingDistributedSMSSender rather than
	// this app silently keeping a console sender nobody in a distributed
	// replica pool is reading (see smsGatewayURLEnv's doc comment, and
	// TestBuildServer_DistributedDeploymentMode_NoSMSGateway_FailsClosed
	// for the regression proof).
	switch {
	case cfg.SMSGatewayURL != "":
		authnOpts = append(authnOpts, authn.WithSMSSender(authn.NewHTTPSMSSender(cfg.SMSGatewayURL)))
	case cfg.DeploymentMode != pkgcore.DeploymentModeDistributed:
		authnOpts = append(authnOpts, authn.WithSMSSender(authn.NewConsoleSMSSender(smsOutput)))
	}
	authnModule, err := authn.NewModule(db, authnOpts...)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: build the authn module: %w", err)
	}

	// notes' creator seam is demoNotesSubjectResolver (declared above):
	// notes' create handler reads the creating user's id through it and
	// stamps it on the note and the event it publishes (internal/notes
	// module.go's NoteCreatedPayload), so an unwired resolver -- NewModule's
	// default -- would fail every create closed, with notes.subject_unresolved
	// (see internal/notes handler.go's ErrSubjectUnresolved). The resolver
	// reads the X-Demo-User-Id header first and falls back to the verified
	// Principal only when no header is present -- the fallback that lets the
	// seeded accounts of demo_users.go create notes through their access
	// tokens alone (see its own doc comment, and demo_subject.go's
	// demoNotesCreatorUserID).
	notesModule := notes.NewModule(db, notes.WithSubjectResolver(demoNotesSubjectResolver{}))

	// auditModule is go/dbkit/audit's persister. It shares notesModule's
	// own database connection -- no new infra dependency is needed for
	// this app to have a real, queryable audit trail -- and subscribes to
	// audit.EventRecorded, the event notes/handler.go's NotesCreateNote
	// publishes through audit.Emit after a note is successfully created
	// (see the doc comment on db's own construction above for why this
	// app uses that mechanism rather than dbkit's automatic
	// AuditBus-driven write capture).
	auditModule := audit.New(db)

	// The config module shares notes' database and is given everything it
	// needs to serve its two endpoints: the master cipher (notes declares a
	// Sensitive item, so Attach would refuse a cipher-less module), a
	// resolver, and the default anti-loss poller cadence.
	//
	// The resolver is tenancy.NewDomainResolver -- deliberately NOT the
	// authn-derived resolver that gates the notes API below -- and its
	// default tenant is deliberately empty. config's public endpoints are
	// pre-auth display decisions, the one case go/tenancy's DomainResolver
	// doc comment blesses with unmatched-host leniency; an empty default
	// tenant maps that leniency onto the endpoint's own "platform
	// defaults" tier (a host that resolves to no tenant reads system-scope
	// rows, never an error), which is exactly the login-page rule of
	// docs/internal/11-cross-cutting.md's dynamic-config section applied
	// to this app's brand snapshot. config's own internal resolver runs
	// entirely independently of the outer tenancy.Middleware wired at the
	// bottom of this function -- see this function's own middleware-chain
	// comment below for why both coexist.
	configModule := config.NewModule(db,
		config.WithCipher(cipher),
		config.WithResolver(tenancy.NewDomainResolver(
			func(host string) (pkgcore.TenantID, bool) {
				tid, ok := cfg.HostTenants[host]
				return tid, ok
			},
			"",
		)),
	)

	// rbac needs nothing from this host but a database: it declares its own
	// permissions during Register and reads EVERY module's declarations
	// once, in Attach, after Bootstrap. WithSubtreeResolver IS wired, onto
	// orgModule's own Scope (orgSubtreeResolver below, an adapter over the
	// two seams' slightly different signatures -- see its own doc comment):
	// this app's organization tree is real (org_flow_test.go's multi-level
	// DSO tree; the org-route-guards round's own subtree-scoped grant test),
	// so a node-scoped rbac binding must actually resolve against it rather
	// than deny for want of a resolver. Every demo grant seedDemoGrants
	// makes is still tenant-wide (rbac.Scope{}) -- the wiring below is what
	// makes a NARROWER grant (a role assigned with a real node id) mean
	// something, for a host that wants one, not what makes one mandatory.
	rbacModule := rbac.NewModule(db, rbac.WithSubtreeResolver(orgSubtreeResolver{scope: orgModule.Scope()}))

	// storageModule is the reference app's first consumer of go/storage.
	// Its asynchronous work -- the thumbnail-derive task every completed
	// image object enqueues, and the expiry-sweep task this host's
	// periodic-task scheduler enqueues per tenant (periodic_scheduler.go)
	// -- runs on the host's standaloneQueue above
	// (storage.WithQueue), drained and claimed below like every other
	// registry-declared handler.
	storageModule := storage.NewModule(db, storage.WithQueue(standaloneQueue))

	// sharingModule is the reference app's first real consumer of
	// go/sharing end to end (sharing_flow_test.go), the round-2 mandatory
	// first-consumer proof AGENTS.md's "No real consumer yet" section named
	// as the compensating obligation round 1 carried. Its one public route
	// resolves a Share's ResourceRef through storageSharingResolver
	// (sharing_resolver.go), the structurally-typed adapter over
	// storageModule's own ObjectService -- sharing never imports go/storage
	// itself (resolver.go's own doc comment explains why), so this
	// composition is entirely this app's own, the same way every other
	// no-import-edge seam in this file (orgFeatureGate,
	// demoOrgSubjectResolver, ...) is wired. WithTenantConfigReader wires
	// sharingConfigReader (defined above, alongside orgFeatureGate), so a
	// tenant's own sharing.default_expiry override -- once config.Service
	// exists, after Attach below -- actually governs Service.Create's
	// resolved expiry instead of always falling back to
	// defaultShareExpiry.
	sharingModule := sharing.NewModule(db,
		sharing.WithResourceResolver(&storageSharingResolver{svc: storageModule.ObjectService()}),
		sharing.WithTenantConfigReader(sharingConfigReader{service: &configService}),
	)

	// integrationModule is the reference app's mandatory first consumer of
	// go/integration's round-2 outbound-webhook surface (go/integration/
	// AGENTS.md's "No reference-app consumer yet" section named this the
	// compensating obligation the round carried until a real host wired
	// it). orgMemberJoinedWebhookMapping (webhooks.go) is this app's own
	// EventMapping -- a Module construction-time Option, per
	// integration.WithEventMapping's own doc comment, since the mapping's
	// Transform closes over org's own MemberJoined payload shape, which
	// go/integration may not import. WithWebhookQueue shares the same
	// standaloneQueue every other module's asynchronous work already runs
	// on. WebhookURLValidator/WebhookHTTPClient are test-only overrides
	// (see their own doc comments on serverConfig above): nil in every
	// production boot, which leaves go/integration's SSRF protection
	// exactly as strict as it has always been. WithSubjectResolver is
	// round 5's own addition, over the identical demoOrgSubjectResolver
	// instance org's and notification's own wiring already share -- it is
	// what lets the module's spec-generated HTTP surface (mounted below
	// through the generic mountModuleRoutes loop) resolve a creator at
	// all: round 5's integration_createAPIKey and round 7's
	// integration_createWebhookSubscription both read it for their
	// request's CreatedBy, the input those two create operations need and
	// every other operation this surface mounts does not (round 1's and
	// round 2's Service-level APIs, exercised directly by this module's
	// own tests, never needed one).
	integrationOpts := []integration.Option{
		integration.WithEventMapping(orgMemberJoinedWebhookMapping),
		integration.WithWebhookQueue(standaloneQueue),
		integration.WithSubjectResolver(demoOrgSubjectResolver{}),
	}
	if cfg.WebhookURLValidator != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookURLValidator(cfg.WebhookURLValidator))
	}
	if cfg.WebhookHTTPClient != nil {
		integrationOpts = append(integrationOpts, integration.WithWebhookHTTPClient(cfg.WebhookHTTPClient))
	}
	integrationModule := integration.NewModule(db, integrationOpts...)

	// notificationModule is the reference app's first consumer of
	// go/notification, wired as the round's mandatory-first-consumer proof
	// (see cmd/server/demo_notification.go for the host-side glue that
	// drives it -- the note-created subscription and the demo
	// patient-message route -- and notification_flow_test.go for the
	// end-to-end legs). Its six required seams are all host-supplied here:
	// the console SMS sender writes to the same smsOutput the authn module's
	// sender writes to (the standalone deployment mode's transport,
	// go/notification/sms.go); the mail transport is whatever the "mailer"
	// seam resolves to -- the console mailer in production, cfg.Mailer in
	// tests; the two blind indexers built above make a contact's encrypted
	// email and phone address queryable by exact match; the delivery queue
	// is the same standaloneQueue storage's derive task runs on, so a
	// completed note-created or patient-reminder dispatch is drained by the
	// same worker pool (its Register call declares the delivery job handler
	// on the registry, and the drain loop below moves every handler onto
	// the queue); and the user-address resolver is the demo directory that
	// demo_notification.go owns. WithSubjectResolver hands the HTTP surface
	// the same demo identity layer org's and notes' handlers use, so a
	// caller is whoever the X-Demo-User-Id header says -- the module
	// resolves identity per operation and never reads it from the request
	// otherwise.
	notificationModule := notification.NewModule(db,
		notification.WithSMSSender(notification.NewConsoleSMSSender(smsOutput)),
		notification.WithMailFrom("notifications@reference-app.example"),
		notification.WithContactEmailIndexer(contactEmailIndexer),
		notification.WithContactPhoneIndexer(contactPhoneIndexer),
		notification.WithDeliveryQueue(standaloneQueue),
		notification.WithUserAddressResolver(demoUserAddressResolver{}),
		notification.WithSubjectResolver(demoOrgSubjectResolver{}),
	)

	// demoModule is the carrier of the app's demo notification type
	// (demo.patient_reminder) and nothing else. It must sit inside the
	// Bootstrap module set for the same reason every message-shipping
	// module must: Kernel.Bootstrap freezes the merged catalog from the
	// Locales() of the modules it is given, and the notification module
	// renders every dispatch from that frozen catalog -- a type whose copy
	// lives outside the set can never render (internal/demo module.go's
	// package comment says so at length).
	demoModule := demo.NewModule()

	// billingModule is the reference app's mandatory first consumer of
	// go/billing (root CLAUDE.md's Reference App section: "a module API
	// that it does not actually use is not considered done") -- both of the
	// module's judgment halves, at that: the credits ledger
	// (docs/internal/15-roadmap.md's M2 exit condition's
	// credit-pack/reserve/refund/usage-display leg) through Credits(), and
	// the subscription-derived entitlement path through Entitlements(),
	// which aiGatewayModule's own construction right below wires onto
	// go/ai-gateway's optional Entitlements seam. It shares this app's own
	// db connection, the same pattern every other module here uses.
	//
	// It is deliberately constructed BEFORE aiGatewayModule: Go evaluates
	// statements in order, and the WithEntitlements option below needs
	// billingModule's already-built EntitlementsService in scope. That is
	// safe even though both modules' Register calls (and the gateway's own
	// first Check) come later: billing.NewModule constructs its services
	// at construction time -- no I/O, nothing deferred to Bootstrap (see
	// go/billing/module.go's own NewModule doc comment) -- and
	// EntitlementsService.Check reads the subscription/plan rows fresh on
	// every call, so judging cannot start before the demo seed below has
	// run, no matter how early the service is built.
	//
	// The one deliberately absent wiring is a UsageReader (nil): quota-kind
	// grants need go/billing's real-time usage counter through that reader,
	// and this app has no metering composition behind it -- which is why
	// demo_entitlements.go's seed grants are Boolean, never Quota (see that
	// file's own doc comment). No WithQueue either: this round wires no
	// payment-channel gateway, so PollingService's active-polling fallback
	// has nothing to poll -- see go/billing/AGENTS.md's own scope table for
	// why an actual payment-gateway integration (a real Stripe/Alipay/
	// WeChat sandbox charge) stays explicitly deferred, untouched by this
	// round.
	billingModule := billing.NewModule(db, nil)

	// aiGatewayModule is the reference app's mandatory first consumer of
	// go/ai-gateway (root CLAUDE.md's "Reference App" section): the
	// internal/consult service (wired below, after Bootstrap) calls its
	// Gateway.Chat under consult.LogicalModel ("chat:default"), which
	// WithModelRoute routes to the module's own zero-external-dependency
	// default provider, aigateway.ProviderOpenAICompatible. The vendor
	// model id ("gpt-4o-mini") is opaque to this app -- it is passed
	// through to whatever OpenAI-compatible endpoint the resolved
	// credential's base URL actually names (the real OpenAI API in a real
	// deployment, an httptest.Server in consult_flow_test.go) -- and never
	// seen by consult's own code, per aigateway.ChatRequest.Model's own
	// doc comment on why business code never hardcodes a vendor model id.
	// aiGatewayModule additionally wires round 2's image-generation
	// pipeline: the internal/smilesim service (wired below, after
	// Bootstrap) calls Gateway.GenerateImage under smilesim.LogicalModel
	// ("image:smile-simulation"), routed to the module's own
	// zero-external-dependency default image provider,
	// aigateway.ProviderOpenAICompatibleImage. WithImageGeneration shares
	// this app's own standaloneQueue -- the same pool storage's
	// thumbnail-derive task and notification's delivery task already run
	// on -- and storageModule's own ObjectService, so the job handler
	// go/ai-gateway registers on reg.Jobs (drained onto standaloneQueue
	// below, alongside every other module's job handlers) reads the
	// patient photo and writes the generated simulation back through the
	// very same storage this app's other consumers use.
	//
	// WithEntitlements is this round's addition: it wires
	// billingModule.Entitlements() -- the real billing.EntitlementsService
	// constructed above -- onto go/ai-gateway's optional, structurally-
	// typed Entitlements seam, so BOTH halves of this one Gateway instance
	// (Chat for consult, GenerateImage for smilesim; they share the very
	// same *aigateway.Gateway) gate every request on the calling tenant's
	// subscription before any provider is reached, exactly go/ai-gateway's
	// own checkEntitlement design: key "model:"+logicalModel, requested 1,
	// denial answered with ErrEntitlementDenied before the provider sees
	// the call. The adapter is an aigateway.EntitlementsFunc closure rather
	// than a direct assignment because the two modules' Decision types are
	// distinct named types (billing.Decision vs aigateway.Decision) -- the
	// exact no-adapter-to-write claim go/ai-gateway/seams.go's own doc
	// comment makes is a deliberate simplification that does not compile;
	// the closure shape below is that file's own documented example,
	// verbatim. An unwired seam (nil) would let every request through; a
	// wired one judged against an empty database would deny everything --
	// seedDemoEntitlements (boot, below) is what keeps the seeded demo
	// tenants on the allowed side from the very first request.
	aiGatewayModule := aigateway.NewModule(db,
		aigateway.WithModelRoute(consult.LogicalModel, aigateway.ProviderOpenAICompatible, "gpt-4o-mini"),
		aigateway.WithModelRoute(smilesim.LogicalModel, aigateway.ProviderOpenAICompatibleImage, "dall-e-3"),
		aigateway.WithImageGeneration(standaloneQueue, storageModule.ObjectService()),
		aigateway.WithEntitlements(aigateway.EntitlementsFunc(
			func(ctx context.Context, featureKey string, requested int64) (aigateway.Decision, error) {
				decision, checkErr := billingModule.Entitlements().Check(ctx, featureKey, requested)
				if checkErr != nil {
					return aigateway.Decision{}, checkErr
				}
				return aigateway.Decision{Allowed: decision.Allowed, Reason: string(decision.Reason)}, nil
			},
		)),
	)

	// complianceModule is the reference app's first consumer of
	// go/compliance: admin's D7 audit-query HTTP shell reads through
	// complianceModule.AuditQuery(), which itself is a read-only wrapper
	// over the SAME audit.Repository (over this same database connection)
	// auditModule's own write-capture persister already writes into --
	// sharing one connection to one audit_events table, exactly as
	// auditModule shares notesModule's own connection above. WithQueue is
	// the same standaloneQueue every other module's asynchronous work
	// already shares; compliance.Module.Register refuses to proceed
	// without one (ErrQueueRequired) regardless of whether a caller
	// happens to use that half of the module. WithSharing wires the
	// already-constructed sharingModule's own Service() as
	// compliance.SharingCreator -- go/compliance/AGENTS.md's round-2 notes
	// this seam as deliberately unwired "until an owning module opts in";
	// go/admin's D7 export leg (docs/internal/23-admin.md) is that
	// module, so this is where compliance.ExportService.Export gains its
	// first genuine delivery path (a real, single-view go/sharing link)
	// rather than refusing every call with ErrSharingRequired.
	// WithExportConfigReader wires complianceConfigReader (defined above,
	// alongside orgFeatureGate and sharingConfigReader), so a tenant's own
	// compliance.export_delivery_expiry override -- once config.Service
	// exists, after Attach below -- actually governs
	// ExportService.Export's minted delivery-link expiry instead of always
	// falling back to defaultExportDeliveryExpiry.
	complianceAuditRepo := audit.NewRepository(db)
	complianceModule := compliance.NewModule(complianceAuditRepo,
		compliance.WithQueue(standaloneQueue),
		compliance.WithSharing(sharingModule.Service()),
		compliance.WithExportConfigReader(complianceConfigReader{service: &configService}),
	)

	// adminModule is the reference app's mandatory first consumer of
	// go/admin's round 1 AND round 2 (docs/internal/23-admin.md): D3's
	// tenant ledger, D5's impersonation pipeline, D6's cross-tenant user
	// search, D7's audit-query shell and export leg, D8's role management
	// and D9's usage dashboard (D9's go/metering/go/billing wiring is
	// deliberately not part of this, since neither module has a real
	// reference-app consumer of its own yet -- go/admin/AGENTS.md's Known
	// limitations records this the same way go/pki's X.509 layer and
	// go/billing/go/metering themselves already do for their own unwired
	// surfaces). WithAuthn takes the *authn.Module itself, not its
	// Service() -- see go/admin/AGENTS.md's wiring-contract section for
	// why, and admin.Module.DependsOn()'s own doc comment for the
	// resulting "authn" dependency Kernel.Bootstrap's sort honors below.
	// WithQueue is the same standaloneQueue every other module's
	// asynchronous work already shares -- D7's export leg enqueues onto
	// it rather than running compliance.ExportService.Export synchronously
	// inside the request.
	adminModule := admin.NewModule(db,
		admin.WithAuthn(authnModule),
		admin.WithOrg(orgModule),
		admin.WithCompliance(complianceModule),
		admin.WithNotification(notificationModule),
		admin.WithQueue(standaloneQueue),
	)

	migrationRegistry := dbkit.NewMigrationRegistry()
	if regErr := migrationRegistry.Register(pkiModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(authnModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(rbacModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(notesModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(orgModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(configModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(auditModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(storageModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(sharingModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(integrationModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	// demoModule is deliberately absent from this registry: it ships no
	// migrations (its Migrations() is an empty FS -- see internal/demo's
	// module doc), so there is nothing to register or apply for it.
	if regErr := migrationRegistry.Register(notificationModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(aiGatewayModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(billingModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	// complianceModule is deliberately absent from this registry too: it
	// ships no migrations of its own (Migrations() is an empty FS) --
	// every row it reads or writes lives in auditModule's own
	// audit_events table, already registered above.
	if regErr := migrationRegistry.Register(adminModule); regErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: apply migrations: %w", applyErr)
	}

	// Bootstrap registers the modules in argument order -- pki leads the
	// set (a position no other module's Register depends on, mirroring
	// its lead in the migration registry above: authn's construction
	// already consumed pkiModule.Service(), so this app's composition
	// order simply starts with pki), with authn immediately behind it, so
	// authn's Register-time declarations (its config items, its
	// permissions, its events) precede the modules that lean on them,
	// then notes and org before config, so the configuration items and
	// feature flags their Register calls declare (notes' own, and org's
	// read-only reliance on the org.invitations / org.invitation_email
	// flags it declares itself) are in the registry before the config
	// module's own Register runs, and then Attach freezes the schema
	// snapshot those declarations fold into, exactly the sequence config's
	// Attach doc comment prescribes ("after Kernel.Bootstrap has
	// returned"). rbac comes next so its own Attach -- which snapshots
	// every permission every module declared during Register -- runs
	// against a Registry that has already seen every module's
	// declarations. storage follows rbac simply because its own order is
	// not load-bearing: its DependsOn is nil, the queue its Register
	// validates is the host seam built above, and the permissions it
	// declares are folded into rbac's Attach snapshot no matter where it
	// sits. demo and notification follow storage for the same reason:
	// neither's position is load-bearing -- demo's Register only declares
	// its notification type, and notification's Register only validates
	// its host seams and registers its delivery job handler -- but both
	// must sit inside this set, and together: the merged catalog freezes
	// once after every module has registered, and notification renders
	// every dispatch (the demo patient-reminder copy included) from that
	// frozen catalog, so a demo whose templates lived outside the set
	// could never render (internal/demo module.go's package comment says
	// so at length). aiGatewayModule follows notification for the same
	// not-load-bearing reason: its own Register declares nothing but the
	// SystemPurposeCredentialWrite system purpose (go/ai-gateway/module.go's
	// Register doc comment), which the platform-credential write below
	// only needs registered before it runs, not before any other module's
	// Register. billingModule follows aiGatewayModule for the same
	// not-load-bearing reason: its own Register (module.go) declares only
	// its permissions, its five credit-ledger audit actions and its two
	// published events, none of which any other module's own Register
	// depends on -- its DependsOn is nil, matching go/metering's and
	// go/pki's own identical answer for the same reason. complianceModule
	// follows for the same not-load-bearing
	// reason (it ships no migrations and validates only its own queue seam);
	// adminModule follows it and IS load-bearing in one respect --
	// admin.Module.DependsOn() names "authn", so Bootstrap's own dependency
	// sort (sortModulesByDependency) runs authn's Register before admin's
	// regardless of argument order, which is what makes
	// authnModule.Service() non-nil by the time admin's Register reads it
	// (go/admin/AGENTS.md's wiring-contract section has the detail). audit
	// last is not load-bearing order -- its Module.DependsOn
	// is nil, and its subscriptions are valid to install before or after
	// any publisher registers (see audit's Module.DependsOn doc comment)
	// -- it simply reads naturally as "the business-facing modules, then
	// the cross-cutting persister watching them."
	//
	// A single Registry -- and so a single EventBus, reg.EventBus() --
	// serves every module Bootstrap registers here, which is what lets
	// auditModule's subscriptions (installed inside its own Register)
	// actually receive the audit.EventRecorded event notesModule's
	// handler publishes through audit.Emit (see NewHandler's wiring
	// below), and what lets orgModule's own subscriptions receive
	// authn's UserCreated event on that same bus: no separate bus
	// construction is needed the way it would be if this app wired
	// dbkit.Options.AuditBus (see db's own doc comment above for why it
	// deliberately does not).
	//
	// WithDeploymentMode(cfg.DeploymentMode) declares the topology the
	// composition is validated against; it never selects an implementation
	// (docs/internal/03-deployment-modes.md's orthogonality rule). Every
	// stateful seam below follows the exact same conditional-injection
	// shape APP_REDIS_ADDR originally established for the "eventbus" seam
	// alone: an unset env var leaves that seam on the Preset's in-process
	// default (so a plain `go run ./cmd/server` is byte-for-byte unaffected
	// by this round), and a configured one injects a real implementation
	// with the capability bits that implementation genuinely carries.
	//
	// When APP_REDIS_ADDR is set, WithEventBus injects a REAL
	// Redis-backed EventBus -- eventbus/redis's NewEventBus over a go-redis
	// client this host constructs and owns -- declaring
	// MultiReplicaSafe|SurvivesRestart, the capabilities the Redis Streams
	// implementation genuinely carries. The SAME client also backs the
	// "kv" seam (kv/redis.NewKVStore takes the identical *redis.Client
	// type): one Redis instance backing both seams is this app's
	// deliberate minimal-footprint choice, mirroring go/notification's own
	// Redis integration leg, which shares one client across two bus
	// instances. This is not merely a convenience -- a distributed
	// deployment mode requires MultiReplicaSafe of every seam that carries
	// shared state (docs/internal/03-deployment-modes.md's capability
	// table), so wiring the "eventbus" seam alone, as this app did before
	// this round, can never let a distributed composition succeed: the
	// very next seam Kernel.Bootstrap resolves and validates, "kv", would
	// still fail on the Preset's in-process default.
	//
	// When the APP_S3_* variables are set (configFromEnv's own doc
	// comment on s3EndpointEnv has the completeness rule), WithObjectStore
	// injects a REAL S3-compatible ObjectStore (objectstore/s3.
	// NewObjectStore, reaching MinIO, Aliyun OSS or AWS S3 through the
	// minio-go client), declaring the same MultiReplicaSafe|SurvivesRestart
	// the "objectstore.s3" builtin registration itself declares. A
	// non-empty cfg.ObjectStoreRoot (from APP_OBJECT_STORE_ROOT, or
	// injected straight onto the struct by a test) is the local twin of
	// that composition and an alternative to it: WithObjectStore then
	// injects pkgcore.NewLocalObjectStore over the fixed directory,
	// declaring SurvivesRestart alone -- the directory genuinely survives
	// a process restart, and genuinely nothing about a single-process
	// local store is replica-safe, so MultiReplicaSafe is never claimed
	// for it.
	//
	// The Mailer override -- cfg.Mailer, non-nil either because
	// configFromEnv composed a real pkgcore.NewSMTPMailer from the
	// APP_SMTP_* variables, or because a test (server_test.go's org/
	// notification flow tests) injected an in-process capture double --
	// rides along as a fourth conditional option. Its capability
	// declaration is cfg.MailerCapabilities when set (MultiReplicaSafe|
	// SurvivesRestart for the real SMTP composition, matching the
	// "mailer.smtp" builtin's own declaration) and pkgcore.Stateless
	// otherwise -- the honest capability for a throwaway in-process test
	// double, and the same declaration this override carried before this
	// round.
	//
	// Nothing about the rest of this wiring changes when any of these are
	// injected: audit.Emit still publishes on reg.EventBus() (the injected
	// bus), auditModule's subscriptions run synchronously on the
	// publishing side exactly as they do on the in-memory bus, and any
	// OTHER process consuming the same Redis streams, the same S3 bucket
	// or the same SMTP relay observes the same effects.
	//
	// go-redis is imported here -- and go.mod therefore requires it
	// directly -- because the app is the assembly host that eventbus/redis's
	// EventBus contract names as the client's owner: NewEventBus builds on a
	// client the host constructs and keeps owning ("the client the bus was
	// built on stays open, because the host owns it" -- Close's own doc
	// comment), which is exactly why cleanup below closes redisClient
	// itself. The no-concrete-infrastructure-implementation rule constrains
	// business modules, not the application that assembles them.
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
	if cfg.ObjectStoreRoot != "" {
		kernelOptions = append(kernelOptions,
			pkgcore.WithObjectStore(pkgcore.NewLocalObjectStore(cfg.ObjectStoreRoot), pkgcore.SurvivesRestart))
	}
	if cfg.Mailer != nil {
		mailerCapabilities := cfg.MailerCapabilities
		if mailerCapabilities == 0 {
			mailerCapabilities = pkgcore.Stateless
		}
		kernelOptions = append(kernelOptions, pkgcore.WithMailer(cfg.Mailer, mailerCapabilities))
	}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, pkiModule, authnModule, notesModule, orgModule, configModule, rbacModule, storageModule, sharingModule, integrationModule, demoModule, notificationModule, aiGatewayModule, billingModule, complianceModule, adminModule, auditModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: bootstrap kernel: %w", err)
	}
	// integrationModule's Attach must run after Bootstrap for the same
	// reason config's and rbac's do just below: its Service reads
	// reg.Events.Bus() and reg.AuditActions, which Bootstrap only finishes
	// wiring once every module's Register call has returned (module.go's
	// own Attach doc comment). Unlike config's and rbac's Attach calls, its
	// ordering relative to them is not load-bearing -- nothing here reads a
	// permission or configuration snapshot. Its return value was once the
	// *integration.Service webhooks.go's wireIntegrationWebhooks mounted its
	// demo route from; round 7 retired that hand-mounted route (every
	// spec-generated surface the module mounts reads the Service at call
	// time through Handler's own Register-time forwarding wrapper instead),
	// so the value is discarded here.
	if _, attachErr := integrationModule.Attach(reg); attachErr != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the integration module: %w", attachErr)
	}
	configService, err = configModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the config module: %w", err)
	}
	// rbac's Attach must also come after Bootstrap, and for a sharper
	// reason than config's: what it freezes is the snapshot of every
	// permission every module declared, so a snapshot taken any earlier
	// would be missing whatever registered after it -- and a permission
	// missing from that catalog cannot be granted at all.
	rbacService, err = rbacModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: attach the rbac module: %w", err)
	}
	if seedErr := seedDemoGrants(ctx, rbacService, cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}
	if cfg.OnRBACReady != nil {
		cfg.OnRBACReady(rbacService)
	}
	// seedDemoCredits is the demo, NOT-a-real-payment stand-in for
	// docs/internal/15-roadmap.md's M2 exit condition's buy-a-credit-pack
	// leg -- see that function's own doc comment for exactly why a real
	// Stripe/Alipay/WeChat sandbox charge stays out of scope here. It runs
	// unconditionally, like seedDemoGrants just above,
	// regardless of cfg.DemoUsersPassword: internal/smilesim's own tests
	// (smilesim_flow_test.go) need a real, non-zero starting balance on
	// tenant-acme to exercise the successful-generation leg, exactly the
	// way seedDemoGrants' own roles are needed by every test that gates a
	// route on a permission, demo password or not.
	if seedErr := seedDemoCredits(ctx, billingModule.Credits(), cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}

	// seedDemoEntitlements is the demo, NOT-a-real-purchase stand-in for
	// the "tenant buys a subscription, the payment channel confirms it"
	// leg of docs/internal/06-billing-and-metering.md's full flow -- see
	// that function's own doc comment for exactly why a real
	// Stripe/Alipay/WeChat sandbox charge stays out of scope here. It runs
	// unconditionally, like seedDemoCredits just above: since the gateway
	// below is wired with WithEntitlements, every consult/smilesim request
	// is gated on the calling tenant holding an Active subscription to a
	// Plan granting that route's model key -- without this seed the demo
	// tenants would have none and every demo AI route would answer
	// aigateway.entitlement_denied. Running it here, before any route can
	// serve, also means the seam is live (never nil and never judging an
	// empty database) from the very first request.
	if seedErr := seedDemoEntitlements(ctx, billingModule.Plans(), billingModule.Subscriptions(), cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, nil, seedErr
	}

	// notes' retention participant is registered here, after Bootstrap --
	// compliance's Register is what attaches the Retention registrar the
	// kernel's reg.Retention seat resolves to, so Add before Bootstrap
	// would silently register onto a registrar nothing sweeps with (see
	// internal/notes/retention_participant.go's own doc comment). The
	// participant is built over the very dbkit.Open *gorm.DB the notes
	// Module already uses -- share the connection, never a second pool --
	// with notes.NewRepository(db) the same fresh-repository-over-the-
	// shared-db shape every earlier host-side seam wiring in this file
	// follows. Add returns ErrDuplicateRetentionParticipant on a repeated
	// Name, refused here like any other wiring failure.
	if err := reg.Retention.Add(notes.NewRetentionParticipant(notes.NewRepository(db))); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: register notes retention participant: %w", err)
	}

	// admin's D8 role-management surface needs the real *rbac.Service --
	// which, like config's and rbac's own Attach calls above, exists only
	// after Bootstrap has returned. This is why go/admin's RoleService is
	// wired through a distinct, post-Bootstrap Module.AttachRBAC call
	// rather than a WithXxx(*rbac.Module) construction-time Option the
	// way authn/org/compliance/notification are -- see AttachRBAC's own
	// doc comment for the full reasoning.
	adminModule.AttachRBAC(rbacService)

	// The ai-gateway platform credential: written only when cfg.AIGatewayAPIKey
	// is set (see its own doc comment on serverConfig for why the default is
	// empty). This must run after Bootstrap, because aiGatewayModule.Register
	// is what calls pkgcore.RegisterSystemPurpose(aigateway.SystemPurposeCredentialWrite)
	// -- WithSystemContext below refuses an unregistered purpose -- and the
	// write itself needs the module's own CredentialService, which
	// aiGatewayModule.Credentials() exposes regardless of Bootstrap having
	// run (constructing a Module performs no I/O), so nothing here strictly
	// needs to wait for Bootstrap except the purpose registration.
	if cfg.AIGatewayAPIKey != "" {
		sysCtx, sysErr := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
			Actor:   "reference-app-boot",
			Purpose: aigateway.SystemPurposeCredentialWrite,
		})
		if sysErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: build the ai-gateway credential system context: %w", sysErr)
		}
		if credErr := aiGatewayModule.Credentials().SetPlatformCredential(
			sysCtx, aigateway.ProviderOpenAICompatible, cfg.AIGatewayAPIKey, cfg.AIGatewayBaseURL,
		); credErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: set the ai-gateway platform credential: %w", credErr)
		}
	}
	// The ai-gateway image-generation platform credential -- the same
	// system-context path as the chat credential above, written only when
	// cfg.AIGatewayImageAPIKey is set (see its own doc comment on
	// serverConfig).
	if cfg.AIGatewayImageAPIKey != "" {
		sysCtx, sysErr := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
			Actor:   "reference-app-boot",
			Purpose: aigateway.SystemPurposeCredentialWrite,
		})
		if sysErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: build the ai-gateway image credential system context: %w", sysErr)
		}
		if credErr := aiGatewayModule.Credentials().SetPlatformCredential(
			sysCtx, aigateway.ProviderOpenAICompatibleImage, cfg.AIGatewayImageAPIKey, cfg.AIGatewayImageBaseURL,
		); credErr != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: set the ai-gateway image platform credential: %w", credErr)
		}
	}

	// Drain the registry's job handlers onto the standalone queue and
	// start the pool. Only now -- after Bootstrap -- can the handlers be
	// moved: the modules' Register calls declared them on the registry
	// (each handler's backing service attached its seams in the same
	// call), and reg.Jobs.Handlers() is the map those declarations
	// filled. Each entry must actually be a jobs.Handler; anything else
	// is a wiring bug between a module and the queue contract, refused
	// here rather than mis-typed into a worker at job-claim time. Start
	// is non-blocking -- it launches the dispatcher and worker goroutines
	// and returns -- so the first enqueued job (a completed object's
	// thumbnail derivation) waits only as long as a poll of the queue's
	// own task table.
	for jobType, handler := range reg.Jobs.Handlers() {
		jobsHandler, ok := handler.(jobs.Handler)
		if !ok {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: registry job handler %q is not a jobs.Handler", jobType)
		}
		if err := standaloneQueue.RegisterHandler(jobsHandler); err != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: register job handler %q: %w", jobType, err)
		}
	}
	// cfg.DisableQueueWorker skips Start entirely rather than merely
	// declining to enqueue: RegisterHandler above still runs (so a stray
	// Enqueue call from another module's wiring is never refused with
	// jobs.ErrHandlerNotRegistered), but with no dispatcher and no worker
	// goroutines launched, this replica can never claim or execute a Job of
	// any type -- see disableQueueWorkerEnv's own doc comment for why.
	if !cfg.DisableQueueWorker {
		if err := standaloneQueue.Start(ctx); err != nil {
			_ = cleanup()
			return nil, nil, nil, fmt.Errorf("reference-app: start the job queue: %w", err)
		}
		// Start the host's periodic-task scheduler on the same gate: a
		// task this replica can never execute is pointless to enqueue, so
		// the ticker that enqueues the wired mechanisms' tasks (storage's
		// per-tenant expiry sweep over cfg.HostTenants, pki's signing-key
		// expiry scan) only starts once the queue worker did --
		// see periodic_scheduler.go. context.Background(), never ctx, per
		// startPeriodicTaskScheduler's own doc comment: the enqueues must
		// keep running until cleanup's own periodicTaskSchedulerStop call,
		// not be cut short by whatever cancels buildServer's own ctx.
		periodicTaskSchedulerStop = startPeriodicTaskScheduler(
			context.Background(),
			cfg.PeriodicTaskInterval,
			cfg.HostTenants,
			storageModule.LifecycleService(),
			pkiModule.Service(),
		)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)
	orgGuardDeps := orgRouteGuardDeps{scope: orgModule.Scope(), members: orgModule.Members()}
	adminHandler, mountErr := mountModuleRoutes(mux, reg, rbacService, orgGuardDeps, cfg.DisableDemoUserHeader)
	if mountErr != nil {
		_ = cleanup()
		return nil, nil, nil, mountErr
	}

	// wireDemoNotification adds the reference app's demo glue on top of the
	// mounted module routes: the subscription that turns notes' note-created
	// event into a notification dispatch for the note's creator, and the
	// hand-written demo patient-message route that dispatches the demo
	// module's patient-reminder type to a verified external contact. The
	// bus is reg.EventBus() -- the same bus Bootstrap gave every module, so
	// the note-created subscription hears exactly what notesModule's handler
	// publishes on that bus -- and the notificationModule services are the
	// module's own accessors, the same instances its Register validated and
	// its HTTP handler drives (see demo_notification.go for the seam
	// contracts, and notification_flow_test.go for the end-to-end legs).
	// The call cannot fail: nothing it does returns an error.
	wireDemoNotification(mux, reg.EventBus(), notificationModule)

	// wireConsult mounts go/ai-gateway's mandatory-first-consumer route
	// (cmd/server/consult.go): consultService shares notesModule's own
	// database connection through a fresh notes.Repository, exactly the way
	// auditModule shares it above -- no new infrastructure dependency is
	// needed for this app to have a real consult surface -- and asks
	// aiGatewayModule's own Gateway, the same instance
	// aiGatewayModule.Register validated. The call cannot fail: nothing it
	// does returns an error.
	consultService := consult.NewService(notes.NewRepository(db), aiGatewayModule.Gateway())
	wireConsult(mux, consultService)

	// wireSmileSim mounts go/ai-gateway round 2's mandatory-first-consumer
	// routes (cmd/server/smilesim.go): smileSimService asks
	// aiGatewayModule's own Gateway -- the same instance
	// aiGatewayModule.Register validated -- to run an async smile
	// simulation over a patient photo already uploaded through
	// storageModule's own HTTP surface, and the job-status route polls the
	// same standaloneQueue every other async task in this app shares.
	// billingModule.Credits() is the same *billing.CreditService instance
	// seedDemoCredits granted the demo tenants' starting balance against
	// above: smilesim.Service reserves smilesim.CreditsPerSimulation credits from it
	// before ever calling Gateway.GenerateImage, and settles that
	// reservation (Confirm/Refund) once the async job reaches a terminal
	// status -- see internal/smilesim/service.go's own "Credit accounting"
	// and "Settlement reachability" package doc sections.
	//
	// smileSimReservationStore shares this app's own db connection (like
	// every other module above) and gets its own tiny table created
	// imperatively via EnsureSchema, mirroring go/jobs.StandaloneQueue's
	// own "create the persistence schema if it does not already exist"
	// pattern rather than joining migrationRegistry -- see
	// reservation_store.go's own doc comment for why. standaloneQueue is
	// passed to NewService too, alongside the store: it is what
	// ReconcileOutstandingCredits polls for each outstanding reservation's
	// job status, and StartReconciler below wraps that sweep in a ticker
	// loop this app starts once at boot (smileSimReconcilerStop, stopped
	// first thing in cleanup above), so a reservation nobody ever polls
	// to completion -- or one whose settlement was in flight when this
	// process last restarted -- still gets settled rather than staying
	// Reserved forever.
	smileSimReservationStore := smilesim.NewReservationStore(db)
	if err := smileSimReservationStore.EnsureSchema(ctx); err != nil {
		_ = cleanup()
		return nil, nil, nil, fmt.Errorf("reference-app: ensure smilesim credit reservation schema: %w", err)
	}
	smileSimService := smilesim.NewService(aiGatewayModule.Gateway(), billingModule.Credits(), reg.EventBus(), standaloneQueue, smileSimReservationStore)
	// context.Background(), never ctx, per StartReconciler's own doc
	// comment: the sweep must keep running until cleanup's own
	// smileSimReconcilerStop call, not be cut short by whatever cancels
	// buildServer's own ctx.
	smileSimReconcilerStop = smileSimService.StartReconciler(context.Background(), 0)
	wireSmileSim(mux, smileSimService, standaloneQueue)

	// wireIntegrationAuthenticated mounts go/integration round 6's
	// mandatory-first-consumer route (cmd/server/integration_authenticate.go):
	// a minimal "whoami" demo endpoint gated by the module's own new
	// AuthMiddleware, proving the previously-undischargeable property
	// go/integration/AGENTS.md's round-5 section named -- a key
	// authenticates, a rotated-away key is refused, a revoked key is
	// refused -- through this app's own real, composed HTTP stack, and
	// wiring LayeredLimiter/HTTPGuard in front of a real Authenticate-gated
	// surface for the first time. reg.KVStore() is the same resolved
	// KVStore seam every other rate-limited mechanism in this codebase
	// would use. The call cannot fail: nothing it does returns an error.
	wireIntegrationAuthenticated(mux, integrationModule, reg.KVStore())

	// The middleware chain: authn.Middleware(verifier) FIRST, then
	// tenancy.Middleware(authn.NewPrincipalResolver()) -- the deliberate
	// deviation from docs/internal/01-architecture.md's originally
	// documented order (tenancy before authn), recorded there and in
	// go/authn/AGENTS.md's "The middleware chain is authn, then tenancy" section: a
	// tenancy.Resolver's signature (Resolve(*http.Request)
	// (pkgcore.TenantID, error)) cannot hand a verified JWT's claims to
	// anything downstream, so running tenancy first would force verifying
	// every token twice over two code paths free to drift. Running
	// authn.Middleware first verifies once; NewPrincipalResolver then just
	// reads the already-verified Principal out of the request context.
	//
	// The consequence that matters here: authn.Middleware is OPTIONAL
	// auth (a missing token proceeds with no Principal; an invalid one
	// 401s immediately), so tenancy.Middleware's own fail-closed default
	// -- refuse a request whose (method, path) is not on the allowlist AND
	// whose resolver failed -- is what makes EVERY route this app mounts
	// require a valid Principal by default, with NO extra wrapping needed
	// per route: an unauthenticated request to the notes API now gets 403
	// (tenant unresolved, because there is no Principal to read a tenant
	// from) exactly the way an unrecognized Host used to. The routes
	// listed in the allowlist below are the ONLY ones that work with no
	// Principal at all -- healthz, metrics, config's two pre-auth display
	// endpoints (still gated by their own internal DomainResolver, see
	// configModule's wiring above -- entirely independent of this outer
	// middleware), and authn's own pre-auth operations (register, sign-in,
	// token refresh, social authorize/callback), which Handler itself
	// (go/authn/handler.go) additionally decides whether to require a
	// Principal for, operation by operation.
	//
	// Both GET and HEAD are allowlisted for healthz/metrics, not GET
	// alone: net/http's ServeMux automatically serves HEAD from a
	// registered "GET "+path pattern (Go's long-standing GET-implies-HEAD
	// convenience), but tenancy.Middleware does NOT extend WithAllowlist's
	// exemption the same way -- its own doc comment says so explicitly.
	// Allowlisting GET alone would leave HEAD one middleware change away
	// from a 403 the moment anything probes it with HEAD instead of GET.
	// admin.ImpersonationMiddleware sits between authn.Middleware and
	// tenancy.Middleware, exactly as go/admin/AGENTS.md's wiring-contract
	// section (and pipeline.go's own doc comment) describes: it never
	// reorders this chain, it reads the real, already-verified
	// authn.Principal authn.Middleware just installed, and -- only when
	// the request carries a valid X-Admin-Impersonation grant id -- it
	// substitutes a Principal naming the impersonation target for
	// everything downstream, including tenancy.Middleware's own tenant
	// resolution. A request with no such header, or an invalid one, is
	// unaffected: this decorator is a no-op for every route notes/org/
	// storage/etc. serve today unless an operator has actually started an
	// impersonation session.
	//
	// admin's OWN mounted route is deliberately excluded from that branch
	// entirely -- topMux below dispatches it straight from
	// authn.Middleware's own output, through guardAdminRoute's
	// adminSubjectResolver (demo_admin.go) and nothing else. This is what
	// go/admin/AGENTS.md's wiring-contract section requires and what
	// mountModuleRoutes' own doc comment explains: admin's five
	// permissions are evaluated in rbac.SystemDomain against the CALLER'S
	// OWN real, unsubstituted Principal, regardless of whichever tenant
	// their session happens to be currently scoped to and regardless of
	// any impersonation grant that may be active on the request -- neither
	// tenancy.Middleware's tenant resolution nor
	// admin.ImpersonationMiddleware's identity substitution has anything
	// to contribute to that decision, and letting either run first was a
	// real privilege-escalation gap found in review (an ordinary tenant's
	// own Owner role, or an impersonated identity, could otherwise reach
	// admin's console purely because rbac.BuiltinRoleOwner and the shared
	// global permission catalog carry no domain partitioning of their
	// own).
	restOfAppChain := admin.ImpersonationMiddleware(adminModule.Impersonation())(
		tenancy.Middleware(authn.NewPrincipalResolver(), append([]tenancy.MiddlewareOption{
			// tenancy.WithTenantStatusResolver is D4's enforcement seam
			// (docs/internal/23-admin.md, go/tenancy/tenant_status.go):
			// admin's own tenant ledger (*admin.TenantService, D3)
			// implements tenancy.TenantStatusResolver structurally --
			// admin is its one real implementer, but the interface itself
			// does not know admin exists, the identical no-import-in-
			// either-direction shape org.FeatureGate/rbac.SubtreeResolver
			// already use. This is what turns "an operator marked a
			// tenant suspended in admin's console" into every OTHER
			// route (notes, storage, org, ...) actually refusing that
			// tenant's requests on the very next one, rather than being
			// a ledger fact nothing downstream ever consults.
			tenancy.WithTenantStatusResolver(adminModule.Tenants()),
			tenancy.WithAllowlist(http.MethodGet, healthzPath),
			tenancy.WithAllowlist(http.MethodHead, healthzPath),
			tenancy.WithAllowlist(http.MethodGet, metricsPath),
			tenancy.WithAllowlist(http.MethodHead, metricsPath),
			tenancy.WithAllowlist(http.MethodGet, config.PathPublic),
			tenancy.WithAllowlist(http.MethodHead, config.PathPublic),
			tenancy.WithAllowlist(http.MethodGet, config.PathSystemFeatures),
			tenancy.WithAllowlist(http.MethodHead, config.PathSystemFeatures),
			// sharing.PathAccess is the one genuinely public, unauthenticated
			// route this app mounts: an anonymous visitor holding a bearer
			// share token carries no Principal and therefore no tenant claim
			// at all, by design (go/sharing's Handler doc comment) --
			// sharing.Service.AccessPublic resolves the tenant itself, from
			// the token alone, once this allowlist entry lets the request
			// reach it at all. GET only: the fragment defines no other
			// method on this path.
			tenancy.WithAllowlist(http.MethodGet, sharing.PathAccess),
			// integrationWhoamiPath is the identical shape, one layer
			// removed: its caller carries an API key, not a share token, and
			// like sharing.PathAccess it resolves ITS OWN tenant --
			// integration.AuthMiddleware, mounted ahead of this outer chain's
			// tenancy.Middleware reaching this route at all, via
			// Service.Authenticate (cmd/server/integration_authenticate.go).
			// Without this allowlist entry, tenancy.Middleware would refuse
			// every request here with tenancy.tenant_unresolved before
			// AuthMiddleware ever got the chance to resolve one from the
			// presented key.
			tenancy.WithAllowlist(http.MethodGet, integrationWhoamiPath),
		}, authnPreAuthAllowlist()...)...)(mux),
	)

	topMux := http.NewServeMux()
	topMux.Handle(adminRoutePath, adminHandler)
	topMux.Handle(adminRoutePath+"/", adminHandler)
	topMux.Handle("/", restOfAppChain)

	handler := authn.Middleware(authnModule.Service().Verifier())(topMux)
	// The demo-user seed runs last, once the composed handler exists: it
	// registers the demo accounts through the same register route a browser
	// would use, which needs the whole chain above it. It is opt-in
	// (APP_DEMO_USERS_PASSWORD, see configFromEnv); an empty password
	// leaves everything above exactly as it was.
	if cfg.DemoUsersPassword != "" {
		if seedErr := seedDemoUsers(ctx, handler, memberships, rbacService, orgModule, cfg.HostTenants, cfg.DemoUsersPassword); seedErr != nil {
			_ = cleanup()
			return nil, nil, nil, seedErr
		}
		// seedDemoPlatformStaff is admin's own first-consumer demo account
		// (demo_admin.go): a real registered user whose ONLY membership is
		// rbac.SystemDomain, holding BuiltinRoleOwner there -- every
		// admin:* permission included, since owner carries every
		// permission any module declared.
		if _, seedErr := seedDemoPlatformStaff(ctx, handler, memberships, rbacService, cfg.DemoUsersPassword); seedErr != nil {
			_ = cleanup()
			return nil, nil, nil, seedErr
		}
	}
	return handler, cleanup, complianceModule, nil
}

// authnAPIPath is authn's own HTTP mount point -- duplicated from
// go/authn/module.go's private apiPath constant of the same value, because
// this app's wiring, not authn's package, is what needs to name individual
// routes under it: the module owns and mounts the routes, this file owns
// which of them a caller may reach before proving who they are.
const authnAPIPath = "/api/v1/authn"

// authnPreAuthAllowlist lists every (method, path) pair under authnAPIPath
// that must work with no Principal at all -- registration, every sign-in
// entry point, token refresh, and the social authorize/callback pair (see
// go/authn/api/openapi.yaml's own path table for the exact literals, and
// go/authn/handler.go for which operations skip requirePrincipal).
//
// tenancy.WithAllowlist matches (method, path) exactly, with no prefix or
// wildcard (see its own doc comment), so the social channel's {provider}
// path parameter cannot be allowlisted generically: every channel this
// module ships gets its own two entries here, even though none is wired
// with real credentials in this example today (serverConfig.SocialProviders'
// own doc comment) -- otherwise enabling one later would silently need a
// code change here too, exactly the kind of drift this round's frozen plan
// warns about.
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

// mountModuleRoutes copies every route reg's modules mounted onto mux,
// with ONE deliberate exception: admin's own mounted route (adminRoutePath)
// is never added to mux at all. admin's HTTP surface must not sit behind
// ordinary tenancy.Middleware tenant resolution, and must not sit behind
// admin.ImpersonationMiddleware's identity substitution either --
// go/admin/AGENTS.md's wiring-contract section states both explicitly
// ("admin's OWN routes ... do not sit behind ImpersonationMiddleware --
// that decorator's effect is on the REST of the application's routes
// only"; "does NOT go through ordinary tenancy.Middleware tenant
// resolution"). buildServer therefore composes admin's own gated handler
// on a separate middleware branch that sits directly behind
// authn.Middleware and nothing else -- see buildServer's own composition
// comment -- and mountModuleRoutes returns that handler as its second
// value instead of mounting it into mux.
//
// net/http's ServeMux (since Go 1.22) distinguishes an exact-match pattern
// ("/api/v1/notes") from a subtree pattern ("/api/v1/notes/", matching
// everything below it): registering only the subtree pattern would make
// ServeMux redirect a bare request for the exact path with an HTTP
// redirect instead of serving it directly -- which would silently break a
// POST, since a redirect is not guaranteed to preserve the method or body
// across every client. pkgcore.MountedRoute's own doc comment says the
// Handler "serves every request below Path", meaning it must be reachable
// at Path itself AND at everything nested below it -- so both patterns are
// registered explicitly here, pointing at the same Handler, instead of
// relying on ServeMux's implicit redirect-on-missing-slash behavior.
//
// Every route also passes through guardModuleRoute on the way out, which
// is where rbac's permission gate is applied -- see demo_subject.go's
// demoRouteGuards. guardModuleRoute still runs for admin's own path too
// (dispatching to guardAdminRoute, per demoRouteGuards' adminRouteSentinel
// entry), so the table's exhaustiveness check keeps covering it; only the
// DESTINATION of the resulting handler differs. A path the table does not
// name fails the build here rather than being served.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry, az rbac.Authorizer, orgDeps orgRouteGuardDeps, demoHeaderDisabled bool) (http.Handler, error) {
	var adminHandler http.Handler
	for _, route := range reg.Routes.Routes() {
		handler, err := guardModuleRoute(az, route.Path, route.Handler, orgDeps, demoHeaderDisabled)
		if err != nil {
			return nil, err
		}
		if route.Path == adminRoutePath {
			adminHandler = handler
			continue
		}
		mux.Handle(route.Path, handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", handler)
		}
	}
	if adminHandler == nil {
		return nil, fmt.Errorf("reference-app: no module mounted %q; admin.Module.Register must run for this app to compose its dedicated middleware branch", adminRoutePath)
	}
	return adminHandler, nil
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
// Init first (see main.go), but this indirection keeps that an
// implementation detail of main.go rather than a hidden requirement on
// buildServer's caller: a test that calls buildServer directly (as
// server_test.go's TestBuildServer_Metrics_NoTenantRequired does) can mount
// the route and assert on it without needing to care whether obs.Init has
// run yet in this process, or ever will -- see that test's own doc comment
// for exactly which weaker property it falls back to proving as a result.
// A test that instead builds its own mux around this same handler, as
// TestMetricsAllowlist_ResolutionFailure_StillReturns200 does, is free to
// call obs.Init itself first for a deterministic answer.
func metricsHandler(w http.ResponseWriter, r *http.Request) {
	obs.MetricsHandler().ServeHTTP(w, r)
}
