// Package main is examples/reference-app's minimal starter skeleton --
// exactly the kind of "minimal starter skeleton...freely editable by
// consumers" root CLAUDE.md's "Shape" section describes, not a business
// module. It never goes through pkgcore.Module.Register itself; its whole
// job is wiring one together (see buildServer below) and running it.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/storage"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

const (
	// defaultPort is used when the PORT environment variable is unset.
	defaultPort = "8080"

	// defaultSQLitePath is used when SPEED_DB_PATH is unset. It is a
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
	configKeyEnv = "SPEED_CONFIG_KEY"

	// configKeyHexLength is the encoded length of the required 32-byte key
	// (2 hex characters per byte), checked so a short or malformed
	// SPEED_CONFIG_KEY fails configuration loading with a precise message
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
	orgIndexKeyEnv = "SPEED_ORG_INDEX_KEY"

	// redisAddrEnv names the environment variable holding the Redis server
	// address ("host:port") the injected EventBus connects to. Empty -- the
	// default -- leaves the composition on the in-process bus the
	// PresetStandalone resolves, so zero-setup standalone development keeps
	// working with nothing else running; set it to compose a real
	// Redis-backed bus into the SAME standalone deployment mode, which is
	// the deployment-mode / implementation-composition orthogonality
	// docs/internal/03-deployment-modes.md draws (see buildServer's kernel
	// doc comment below for what the resulting composition proves).
	redisAddrEnv = "SPEED_REDIS_ADDR"
)

// devConfigKey is the master key used when SPEED_CONFIG_KEY is unset. It is
// the ascending 0x00..0x1f byte sequence -- a recognizable constant, never
// a secret -- because zero-setup standalone development (`go run
// ./cmd/server`, `task dev`, this app's tests) must work with no
// environment at all, while config's Sensitive items demand a real
// 32-byte key the moment one is declared (Attach fails with
// ErrCipherRequired otherwise).
//
// This default is a documented trade-off, not a pattern to copy: it is a
// key committed to the repository, which real hosts must never do. A real
// deployment must set SPEED_CONFIG_KEY from a secret store (or refuse to
// start); the constant exists so the *demo* keeps working out of the box,
// and its name and doc comment are the guard rails that keep it honest.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// devOrgIndexKey is the HMAC key used when SPEED_ORG_INDEX_KEY is unset --
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

// devSigningKeySeed, devBlindIndexKey and devPIICipherKey are authn's own
// committed-key placeholders, the same documented trade-off as devConfigKey
// immediately above -- a real deployment must replace every one of them
// with real secret-manager material, never commit real keys the way this
// demo commits these.
//
// Each protects something different and each MUST stay stable across
// restarts for a different reason: authn.WithSigningKeys has no safe
// generated-at-startup default (a fresh key on every restart invalidates
// every outstanding session); authn.WithBlindIndexKey's key must stay
// IDENTICAL across restarts or every already-stored email/phone blind index
// becomes unfindable; and devPIICipherKey seals authn's encrypted PII
// columns (email, phone, TOTP secrets) via authn.RegisterPIISerializer,
// deliberately a DIFFERENT key from devConfigKey -- dbkit's own
// key-separation rule (never let one key double as two different AEAD
// constructions) applies across modules, not only within one.
var (
	devSigningKeySeed = []byte{
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

// devSigningKeySet derives a stable Ed25519 signing key from
// devSigningKeySeed. It builds the TokenKey by hand (ed25519.NewKeyFromSeed)
// rather than through authn.GenerateTokenKey, which always draws from
// crypto/rand and so could never be reproducible across restarts the way
// this demo default needs to be.
func devSigningKeySet() (*authn.KeySet, error) {
	priv := ed25519.NewKeyFromSeed(devSigningKeySeed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("reference-app: derive the dev signing key's public half")
	}
	return authn.NewKeySet(authn.TokenKey{ID: "reference-app-dev", Private: priv, Public: pub})
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
//   - A boot with SPEED_DEMO_USERS_PASSWORD set additionally registers the
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

// demoOrgSubjectResolver stands in for the org.SubjectResolver authn will
// eventually supply from a verified access token's claims. It exists only
// so this reference app has *some* way to demonstrate org's two
// caller-scoped endpoints (creating and accepting an invitation) end to
// end before authn exists.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real SubjectResolver must derive the caller from a source the server
// itself verified (a validated access token's subject claim), never an
// unauthenticated, client-supplied header like this one -- see
// org.SubjectResolver's own doc comment for the same rule stated as a hard
// requirement.
type demoOrgSubjectResolver struct{}

// Subject implements org.SubjectResolver.
func (demoOrgSubjectResolver) Subject(r *http.Request) (string, bool) {
	userID := r.Header.Get(demoOrgUserHeader)
	return userID, userID != ""
}

// compile-time check that demoOrgSubjectResolver satisfies org.SubjectResolver.
var _ org.SubjectResolver = demoOrgSubjectResolver{}

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
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string
	ConfigKey      []byte
	OrgIndexKey    []byte
	RedisAddr      string
	HostTenants    map[string]pkgcore.TenantID

	// Mailer overrides the console mailer the standalone Preset resolves
	// for the "mailer" seam when set. configFromEnv never sets it --
	// production always takes the Preset's real default -- so this only
	// exists for server_test.go's org invitation flow test, which needs
	// the rendered mail back in-process to extract the invitation token
	// rather than parsing it out of console output; buildServer injects
	// it with the pkgcore.Stateless declaration (see its kernel-options
	// comment at the Bootstrap call below for why that is the honest
	// capability for a throwaway test composition).
	Mailer pkgcore.Mailer

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
	// configFromEnv fills it from SPEED_DEMO_USERS_PASSWORD; the empty
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
}

// configFromEnv reads serverConfig from the environment, defaulting to the
// standalone deployment mode on SQLite so `go run ./cmd/server` genuinely
// starts a working server with zero external dependencies.
func configFromEnv() (serverConfig, error) {
	deploymentModeStr := os.Getenv("SPEED_DEPLOYMENT_MODE")
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

	dbPath := os.Getenv("SPEED_DB_PATH")
	if dbPath == "" {
		dbPath = defaultSQLitePath
	}

	// redisAddr stays empty when unset (or explicitly emptied): the
	// standalone composition then resolves the "eventbus" seam from the
	// Preset to the in-process bus, keeping the zero-external-dependency
	// default intact.
	redisAddr := os.Getenv(redisAddrEnv)

	// The config master key: SPEED_CONFIG_KEY when set (a hex-encoded
	// 32-byte key -- see configKeyEnv's doc comment), the documented
	// development default otherwise (see devConfigKey's). A malformed
	// value must fail startup with a precise message rather than surface
	// later as an opaque cipher error; hex.DecodeString rejects anything
	// that is not valid lowercase-or-uppercase hex, and the length check
	// below rejects anything that does not decode to exactly 32 bytes.
	configKey := devConfigKey
	if encoded := os.Getenv(configKeyEnv); encoded != "" {
		if len(encoded) != configKeyHexLength {
			return serverConfig{}, fmt.Errorf(
				"reference-app: %s must hold %d hex characters (a 32-byte key), got %d",
				configKeyEnv, configKeyHexLength, len(encoded))
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return serverConfig{}, fmt.Errorf("reference-app: %s: %w", configKeyEnv, err)
		}
		configKey = decoded
	}

	// The org invitation blind-index key: SPEED_ORG_INDEX_KEY when set,
	// devOrgIndexKey otherwise -- same parsing and same failure shape as
	// configKey above, and see orgIndexKeyEnv's own doc comment for why
	// this must be a key distinct from configKey rather than the same one
	// reused.
	orgIndexKey := devOrgIndexKey
	if encoded := os.Getenv(orgIndexKeyEnv); encoded != "" {
		if len(encoded) != configKeyHexLength {
			return serverConfig{}, fmt.Errorf(
				"reference-app: %s must hold %d hex characters (a 32-byte key), got %d",
				orgIndexKeyEnv, configKeyHexLength, len(encoded))
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return serverConfig{}, fmt.Errorf("reference-app: %s: %w", orgIndexKeyEnv, err)
		}
		orgIndexKey = decoded
	}

	return serverConfig{
		DeploymentMode: deploymentMode,
		Port:           port,
		SQLitePath:     dbPath,
		ConfigKey:      configKey,
		OrgIndexKey:    orgIndexKey,
		RedisAddr:      redisAddr,
		HostTenants:    demoHostTenants,
		// Empty when unset: the demo-user seed is opt-in (its own doc
		// comment in demo_users.go says why the default skips it).
		DemoUsersPassword: os.Getenv(demoUsersPasswordEnv),
	}, nil
}

// buildServer wires the reference app's Kernel -- the authn, notes, org,
// config, rbac, audit and storage Modules -- their migrations, the
// storage queue, and the authn+tenancy middleware chain into a single
// http.Handler. It is the one place that wiring logic lives -- main() and
// the end-to-end tests (server_test.go, authn_e2e_test.go and
// storage_flow_test.go) all call it, so the two can never drift into
// testing a different wiring than the one that actually runs.
//
// It returns the composed handler and a cleanup function that closes
// everything buildServer opened (the services, the injected Redis bus and
// its client when one was built, and the underlying database connection);
// the caller must call cleanup once done with the handler.
//
// The deployment mode no longer refuses anything here: the Kernel it
// bootstraps is what validates the assembled composition against
// cfg.DeploymentMode, failing startup with pkgcore's own capability error
// (ErrCapabilityUnsatisfied) when the resolved composition cannot run in
// the declared mode. This app wires the eventbus seam only -- the kv,
// mailer and objectstore seams resolve from the standalone Preset to their
// in-process implementations -- so today the distributed mode always fails
// that validation naming the first shortfall ("eventbus.memory" when no
// Redis address is configured, "kv.memory" when one is), and the standalone
// mode never does. Those two outcomes, rather than an app-level refusal,
// are exactly what this file's distributed-mode tests pin.
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, error) {
	// Deliberately NOT setting dbkit.Options.AuditBus here, even though
	// notes.Note implements dbkit.Auditable (see model.go): every note
	// write in this app goes through dbkit.Repository[Note], which wraps
	// Create in a WithTenantSession transaction, and
	// dbkit.auditCapturePlugin's After("gorm:create") callback runs
	// *inside* that still-open transaction. Wiring AuditBus to a bus whose
	// subscriber (audit.Module, below) writes into this SAME SQLite file
	// makes that subscriber try to open a second write session against a
	// database that already holds an uncommitted write transaction on the
	// very same OS thread -- SQLite allows only one writer at a time, so
	// this deadlocks into "database is locked" (SQLITE_BUSY) on every
	// single note creation, confirmed empirically while wiring this app.
	// See go/dbkit/AGENTS.md's "Audit trail collection" section for the
	// full write-up and the options for a future round to actually fix
	// the automatic-capture mechanism (deferring the plugin's publish
	// until after the enclosing transaction commits, most likely).
	//
	// This app instead persists its audit trail through the declarative
	// audit.Emit call notes/handler.go's NotesCreateNote makes explicitly
	// -- after h.repo.Create has already returned, i.e. after that
	// transaction has committed, which is exactly why Emit's call site
	// never hits the same hazard.
	//
	// authn's own PII columns (email, phone, TOTP secrets) must have their
	// serializer registered BEFORE dbkit.Open: GORM resolves a model's
	// serializer while it parses the schema, and this module's registry is
	// process-global (authn.RegisterPIISerializer's own doc comment; trap
	// #9 of this round's frozen plan).
	piiCipher, err := dbkit.NewCipher(devPIICipherKey)
	if err != nil {
		return nil, nil, fmt.Errorf("reference-app: build authn's PII cipher: %w", err)
	}
	if regErr := authn.RegisterPIISerializer(piiCipher); regErr != nil {
		return nil, nil, fmt.Errorf("reference-app: register authn's PII serializer: %w", regErr)
	}

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		return nil, nil, fmt.Errorf("reference-app: open database: %w", err)
	}

	// configService and rbacService are filled by their Attach calls below
	// (nil until then), standaloneQueue by the storage module's wiring next
	// to rbacModule (nil until then); redisBus and redisClient are filled
	// below when cfg.RedisAddr selects the injected Redis-backed
	// composition (nil otherwise). cleanup closes the services and the job
	// queue first -- stopping config's anti-loss poller, rbac's cache
	// janitor and the queue's workers so none of them drains a job or a
	// poll against a connection that is being torn down -- then the
	// injected bus, stopping its readers so no remote event can still be
	// delivered to a handler writing the database, then the client this
	// host owns (RedisEventBus never closes it), and the database last.
	// Every close is attempted even when an earlier one failed; the first
	// error wins.
	var (
		configService   *config.Service
		rbacService     *rbac.Service
		standaloneQueue *jobs.StandaloneQueue
		redisBus        *pkgcore.RedisEventBus
		redisClient     *redis.Client
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
		return nil, nil, fmt.Errorf("reference-app: build the config master cipher: %w", err)
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
		return nil, nil, fmt.Errorf("reference-app: build the org email indexer: %w", err)
	}

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

	signingKeys, err := devSigningKeySet()
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: build authn's signing key set: %w", err)
	}

	memberships := cfg.Memberships
	if memberships == nil {
		memberships = newDemoMemberships()
	}
	smsOutput := cfg.SMSOutput
	if smsOutput == nil {
		smsOutput = os.Stdout
	}

	authnModule, err := authn.NewModule(db,
		authn.WithSigningKeys(signingKeys),
		authn.WithBlindIndexKey(devBlindIndexKey),
		authn.WithMembershipReader(memberships),
		authn.WithSMSSender(authn.NewConsoleSMSSender(smsOutput)),
		authn.WithDeploymentMode(cfg.DeploymentMode),
		authn.WithSocialProviders(cfg.SocialProviders...),
		authn.WithRedirectAllowlist(cfg.RedirectAllowlist),
		authn.WithTrustedProviders(cfg.TrustedProviders...),
	)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: build the authn module: %w", err)
	}

	notesModule := notes.NewModule(db)

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
	// once, in Attach, after Bootstrap. No SubtreeResolver is wired because
	// this app has no organization tree yet -- see WithSubtreeResolver's
	// doc comment for why that is a supported configuration rather than a
	// gap, and demo_subject.go's seedDemoGrants for why every demo grant is
	// therefore tenant-wide.
	rbacModule := rbac.NewModule(db)

	// authn's migrations register first: it depends on nothing, and the
	// frozen plan for this round asks that its tables exist before notes'
	// and config's Apply runs, matching the order Bootstrap uses just
	// below.
	standaloneQueue = jobs.NewStandaloneQueue(db)

	// storageModule is the reference app's first consumer of go/storage.
	// Its asynchronous work -- the thumbnail-derive task every completed
	// image object enqueues -- runs on a jobs.StandaloneQueue sharing this
	// app's own database connection: the standalone mode's SQLite-backed
	// worker pool, whose task table StandaloneQueue.Start creates for
	// itself (no migration of this host's is involved). The queue is
	// drained and started below, after Bootstrap, because storage's
	// Register declares its task handlers on the registry and only the
	// host can move them onto a concrete queue; cleanup's Close stops the
	// pool before the shared database closes.
	storageModule := storage.NewModule(db, storage.WithQueue(standaloneQueue))

	migrationRegistry := dbkit.NewMigrationRegistry()
	if regErr := migrationRegistry.Register(authnModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(rbacModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(notesModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(orgModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(configModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(auditModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if regErr := migrationRegistry.Register(storageModule); regErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: register migrations: %w", regErr)
	}
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: apply migrations: %w", applyErr)
	}

	// Bootstrap registers all seven modules in argument order -- authn
	// first of all, so its Register-time declarations (its config items,
	// its permissions, its events) precede the modules that lean on them,
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
	// sits. audit last is not load-bearing order -- its Module.DependsOn
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
	// (docs/internal/03-deployment-modes.md's orthogonality rule). When
	// SPEED_REDIS_ADDR is set, WithEventBus injects a REAL Redis-backed
	// EventBus -- pkgcore.NewRedisEventBus over a go-redis client this host
	// constructs and owns -- declaring MultiReplicaSafe|SurvivesRestart,
	// the capabilities the Redis Streams implementation genuinely carries.
	// The other three seams resolve from the Preset to their in-process
	// implementations, so the assembled composition is a standalone
	// topology whose events cross through real Redis: a single-process
	// deployment that happens to use a multi-replica-safe, restart-surviving
	// bus -- the canonical demonstration that the two axes are independent.
	// Nothing about the rest of this wiring changes: audit.Emit still
	// publishes on reg.EventBus() (the injected bus), auditModule's
	// subscriptions run synchronously on the publishing side exactly as
	// they do on the in-memory bus, and any OTHER process consuming the
	// same Redis streams receives the events too.
	//
	// go-redis is imported here -- and go.mod therefore requires it
	// directly -- because the app is the assembly host that pkgcore's
	// RedisEventBus contract names as the client's owner: NewRedisEventBus
	// builds on a client the host constructs and keeps owning ("the client
	// the bus was built on stays open, because the host owns it" -- Close's
	// own doc comment), which is exactly why cleanup below closes
	// redisClient itself. The no-concrete-infrastructure-implementation
	// rule constrains business modules, not the application that assembles
	// them.
	//
	// The cfg.Mailer override rides along as a second conditional option --
	// the retrofit's WithMailer takes the capability its value declares,
	// and this app declares pkgcore.Stateless for an override that only
	// server_test.go's in-process capture double ever sets, in a throwaway
	// test composition where the durability banner would name no real loss
	// (configFromEnv never sets it; production always takes the preset's
	// console default).
	kernelOptions := []pkgcore.KernelOption{pkgcore.WithDeploymentMode(cfg.DeploymentMode)}
	if cfg.RedisAddr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
		redisBus = pkgcore.NewRedisEventBus(redisClient)
		kernelOptions = append(kernelOptions,
			pkgcore.WithEventBus(redisBus, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart))
	}
	if cfg.Mailer != nil {
		kernelOptions = append(kernelOptions, pkgcore.WithMailer(cfg.Mailer, pkgcore.Stateless))
	}
	reg, err := pkgcore.NewKernel(kernelOptions...).Bootstrap(ctx, authnModule, notesModule, orgModule, configModule, rbacModule, storageModule, auditModule)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: bootstrap kernel: %w", err)
	}
	configService, err = configModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: attach the config module: %w", err)
	}
	// rbac's Attach must also come after Bootstrap, and for a sharper
	// reason than config's: what it freezes is the snapshot of every
	// permission every module declared, so a snapshot taken any earlier
	// would be missing whatever registered after it -- and a permission
	// missing from that catalog cannot be granted at all.
	rbacService, err = rbacModule.Attach(reg)
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: attach the rbac module: %w", err)
	}
	if seedErr := seedDemoGrants(ctx, rbacService, cfg.HostTenants); seedErr != nil {
		_ = cleanup()
		return nil, nil, seedErr
	}

	// Drain the registry's job handlers onto the standalone queue and
	// start the pool. Only now -- after Bootstrap -- can the handlers be
	// moved: storage's Register declared them on the registry (their
	// backing services attached its seams in the same call), and
	// reg.Jobs.Handlers() is the map that declaration filled. Each entry
	// must actually be a jobs.Handler; anything else is a wiring bug
	// between a module and the queue contract, refused here rather than
	// mis-typed into a worker at job-claim time. Start is non-blocking --
	// it launches the dispatcher and worker goroutines and returns -- so
	// the first enqueued job (a completed object's thumbnail derivation)
	// waits only as long as a poll of the queue's own task table.
	for jobType, handler := range reg.Jobs.Handlers() {
		jobsHandler, ok := handler.(jobs.Handler)
		if !ok {
			_ = cleanup()
			return nil, nil, fmt.Errorf("reference-app: registry job handler %q is not a jobs.Handler", jobType)
		}
		if err := standaloneQueue.RegisterHandler(jobsHandler); err != nil {
			_ = cleanup()
			return nil, nil, fmt.Errorf("reference-app: register job handler %q: %w", jobType, err)
		}
	}
	if err := standaloneQueue.Start(ctx); err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: start the job queue: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)
	if mountErr := mountModuleRoutes(mux, reg, rbacService); mountErr != nil {
		_ = cleanup()
		return nil, nil, mountErr
	}

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
	// The demo-user seed runs last, once the composed handler exists: it
	// registers the demo accounts through the same register route a browser
	// would use, which needs the whole chain above it. It is opt-in
	// (SPEED_DEMO_USERS_PASSWORD, see configFromEnv); an empty password
	// leaves everything above exactly as it was.
	if cfg.DemoUsersPassword != "" {
		if seedErr := seedDemoUsers(ctx, handler, memberships, rbacService, cfg.HostTenants, cfg.DemoUsersPassword); seedErr != nil {
			_ = cleanup()
			return nil, nil, seedErr
		}
	}
	return handler, cleanup, nil
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

// mountModuleRoutes copies every route reg's modules mounted onto mux.
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
// Every route also passes through guardModuleRoute on the way to the mux,
// which is where rbac's permission gate is applied -- see
// demo_subject.go's demoRouteGuards. Mounting is the right place for it:
// a route that reaches the mux ungated is served ungated, so the check
// that every mounted path has a declared guard belongs on the only path
// that can mount one. A path the table does not name fails the build here
// rather than being served.
func mountModuleRoutes(mux *http.ServeMux, reg *pkgcore.Registry, az rbac.Authorizer) error {
	for _, route := range reg.Routes.Routes() {
		handler, err := guardModuleRoute(az, route.Path, route.Handler)
		if err != nil {
			return err
		}
		mux.Handle(route.Path, handler)
		if !strings.HasSuffix(route.Path, "/") {
			mux.Handle(route.Path+"/", handler)
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
