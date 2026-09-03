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
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
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

// demoHostTenants is a hard-coded, obviously-temporary Host -> TenantID
// lookup standing in for the Resolver authn will eventually supply for
// authenticated requests -- see go/tenancy/AGENTS.md's "Why there is no
// JWTResolver here". It exists only so this reference app has *some* way
// to demonstrate real multi-tenant behavior end to end before authn
// exists.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real Resolver must derive the tenant from a source the server itself
// controls (a verified token's claims, a database-backed custom-domain
// table) -- never an unauthenticated, static Host map like this one, which
// anyone can trigger just by setting the Host header on an HTTP request.
// See go/tenancy/resolver.go's own Resolver doc comment for the same rule
// stated as a hard requirement on every implementation.
//
// An unrecognized Host deliberately has no entry here: strictHostResolver
// below (not tenancy.DomainResolver) is what buildServer wires up in front
// of this app's real routes, and it fails the request rather than invent a
// tenant for a Host that is missing from this map -- see
// strictHostResolver's own doc comment for why.
var demoHostTenants = map[string]pkgcore.TenantID{
	"acme.demo.localhost":   "tenant-acme",
	"globex.demo.localhost": "tenant-globex",
}

// strictHostResolver resolves a request's tenant from its Host header
// against a fixed lookup table, failing the request (a non-nil error,
// which tenancy.Middleware turns into 403 Forbidden) when the Host does
// not match any configured tenant.
//
// buildServer wires this, not tenancy.DomainResolver, in front of the
// notes Module's real routes -- on purpose. DomainResolver's own doc
// comment scopes its fallback-to-a-default-tenant behavior to
// UNAUTHENTICATED, pre-auth display decisions only ("rendering the right
// brand on a login page... it grants no data access"), and
// go/tenancy/AGENTS.md's "Rules" section says the same thing as a hard
// requirement on every other Resolver: "Do not copy DomainResolver's
// empty-Host-falls-back-to-default behavior into a general pattern...
// Every other Resolver should return a non-nil error rather than invent or
// default a tenant." This app's notes API is real, persisted CRUD data,
// not a pre-auth display decision, and this app has no login page or
// other pre-auth route that would need DomainResolver's leniency --
// /healthz needs no tenant at all, resolved or not, which is exactly what
// its WithAllowlist entry below is for. So an unrecognized Host must fail
// closed here exactly like every other resolution failure, instead of
// landing in a shared, unauthenticated bucket that any anonymous caller
// could read from and write to just by setting an arbitrary Host header.
//
// strictHostResolver stands in for the Resolver authn will eventually
// supply from a verified access token (see demoHostTenants' own doc
// comment); unlike tenancy.DomainResolver, it is the closer approximation
// of the two, since authn's resolver will also fail closed on a caller it
// cannot identify rather than default them into a shared tenant.
type strictHostResolver struct {
	hostTenants map[string]pkgcore.TenantID
}

// Resolve implements tenancy.Resolver.
func (s strictHostResolver) Resolve(r *http.Request) (pkgcore.TenantID, error) {
	if tid, ok := s.hostTenants[r.Host]; ok && tid != "" {
		return tid, nil
	}
	return "", fmt.Errorf("reference-app: no tenant configured for host %q", r.Host)
}

// compile-time check that strictHostResolver satisfies tenancy.Resolver.
var _ tenancy.Resolver = strictHostResolver{}

// demoUserHeader is the header demoSubjectResolver reads to identify the
// HTTP caller: a placeholder for the verified access-token claims authn
// will eventually supply, in exactly the spirit of demoHostTenants' own
// disclaimer above. A caller sets it to whatever user id it wants to act
// as, with no verification whatsoever -- which is fine for this reference
// app's own demonstration purposes and would be a critical vulnerability
// in any real deployment.
const demoUserHeader = "X-Demo-User-Id"

// demoSubjectResolver stands in for the org.SubjectResolver authn will
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
type demoSubjectResolver struct{}

// Subject implements org.SubjectResolver.
func (demoSubjectResolver) Subject(r *http.Request) (string, bool) {
	userID := r.Header.Get(demoUserHeader)
	return userID, userID != ""
}

// compile-time check that demoSubjectResolver satisfies org.SubjectResolver.
var _ org.SubjectResolver = demoSubjectResolver{}

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
// mode, port, database path, the config master key, the demo host map).
// It is a plain struct read from the environment by configFromEnv, NOT the
// dynamic configuration the config module serves: dynamic configuration
// lives in the configs table and can never hold the very key that encrypts
// it, so this bootstrap struct is the deliberate exception to "a plain
// struct, not pkgcore/config's dynamic configuration" -- it is main.go's
// own wiring, which never goes through Module.Register either.
type serverConfig struct {
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string
	ConfigKey      []byte
	OrgIndexKey    []byte
	HostTenants    map[string]pkgcore.TenantID

	// Mailer overrides the deployment mode's default Mailer
	// (pkgcore.NewKernel's own resolveMailer) when set. configFromEnv never
	// sets it -- production always takes the deployment mode's real default,
	// the console mailer in standalone mode -- so this only exists for
	// server_test.go's org invitation flow test, which needs the rendered
	// mail back in-process to extract the invitation token rather than
	// parsing it out of console output.
	Mailer pkgcore.Mailer
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
		HostTenants:    demoHostTenants,
	}, nil
}

// buildServer wires the reference app's Kernel, the notes Module, its
// migrations, and tenancy's middleware into a single http.Handler. It is
// the one place that wiring logic lives -- both main() and
// server_test.go's end-to-end test call it, so the two can never drift
// into testing a different wiring than the one that actually runs.
//
// It returns the composed handler and a cleanup function that closes the
// underlying database connection; the caller must call cleanup once done
// with the handler.
func buildServer(ctx context.Context, cfg serverConfig) (http.Handler, func() error, error) {
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		return nil, nil, fmt.Errorf(
			"reference-app: deployment mode %q is not wired in this example yet; only %q is supported until distributed infrastructure (PostgreSQL, Redis, ...) lands",
			cfg.DeploymentMode, pkgcore.DeploymentModeStandalone)
	}

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
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: cfg.SQLitePath})
	if err != nil {
		return nil, nil, fmt.Errorf("reference-app: open database: %w", err)
	}

	// configService and rbacService are filled by their Attach calls below
	// (nil until then); cleanup closes both before the database, so
	// neither config's anti-loss poller nor rbac's cache janitor is still
	// running against a connection that is being torn down.
	var (
		configService *config.Service
		rbacService   *rbac.Service
	)

	cleanup := func() error {
		var firstErr error
		if configService != nil {
			if closeErr := configService.Close(); closeErr != nil {
				firstErr = closeErr
			}
		}
		if rbacService != nil {
			if closeErr := rbacService.Close(); closeErr != nil && firstErr == nil {
				firstErr = closeErr
			}
		}
		if firstErr != nil {
			return firstErr
		}
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return dbErr
		}
		return sqlDB.Close()
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
		org.WithSubjectResolver(demoSubjectResolver{}),
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
			// This app ships no frontend invitation-acceptance page (M1
			// scope, root CLAUDE.md): the link names org's real
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
	// strictHostResolver gating the notes API -- and its default tenant is
	// deliberately empty. config's public endpoints are pre-auth display
	// decisions, the one case go/tenancy's DomainResolver doc comment
	// blesses with unmatched-host leniency; an empty default tenant maps
	// that leniency onto the endpoint's own "platform defaults" tier (a
	// host that resolves to no tenant reads system-scope rows, never an
	// error), which is exactly the login-page rule of
	// docs/internal/11-cross-cutting.md's dynamic-config section applied
	// to this app's brand snapshot. strictHostResolver would defeat the
	// purpose: its fail-closed 403 is right for real CRUD data (see its
	// own doc comment) and wrong for a request that must render a brand no
	// matter whose it is.
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

	migrationRegistry := dbkit.NewMigrationRegistry()
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
	if applyErr := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("reference-app: apply migrations: %w", applyErr)
	}

	// Bootstrap registers all five modules in argument order -- notes and
	// org before config, so the configuration items and feature flags their
	// Register calls declare (notes' own, and org's read-only reliance on
	// the org.invitations / org.invitation_email flags it declares itself)
	// are in the registry before the config module's own Register runs,
	// and then Attach freezes the schema snapshot those declarations fold
	// into, exactly the sequence config's Attach doc comment prescribes
	// ("after Kernel.Bootstrap has returned"). rbac comes next so its own
	// Attach -- which snapshots every permission every module declared
	// during Register -- runs against a Registry that has already seen
	// every module's declarations. audit last is not load-bearing order --
	// its Module.DependsOn is nil, and its subscriptions are valid to
	// install before or after any publisher registers (see audit's
	// Module.DependsOn doc comment) -- it simply reads naturally as "the
	// business-facing modules, then the cross-cutting persister watching
	// them."
	//
	// A single Registry -- and so a single EventBus, reg.EventBus() --
	// serves every module Bootstrap registers here, which is what lets
	// auditModule's subscriptions (installed inside its own Register)
	// actually receive the audit.EventRecorded event notesModule's
	// handler publishes through audit.Emit (see NewHandler's wiring
	// below), and what lets orgModule's own subscriptions receive
	// authn's UserCreated event once the authn module registers on the
	// same bus: no separate bus construction is needed the way it would be
	// if this app wired dbkit.Options.AuditBus (see db's own doc comment
	// above for why it deliberately does not).
	reg, err := pkgcore.NewKernel(cfg.DeploymentMode, pkgcore.WithMailer(cfg.Mailer)).Bootstrap(ctx, notesModule, orgModule, configModule, rbacModule, auditModule)
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

	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodGet+" "+healthzPath, healthzHandler)
	mux.HandleFunc(http.MethodGet+" "+metricsPath, metricsHandler)
	if mountErr := mountModuleRoutes(mux, reg, rbacService); mountErr != nil {
		_ = cleanup()
		return nil, nil, mountErr
	}

	// strictHostResolver -- not tenancy.DomainResolver -- gates the mux:
	// see its own doc comment above for why an unrecognized Host must fail
	// closed here instead of falling back to a shared default tenant.
	resolver := strictHostResolver{hostTenants: cfg.HostTenants}

	// The entire mux -- including /healthz -- runs behind
	// tenancy.Middleware; tenancy.WithAllowlist is what lets /healthz work
	// with no tenant resolved at all, rather than assembling a second,
	// unprotected mux by hand alongside it. Every route a real module
	// mounts stays fail-closed by default; only the one route named here
	// is exempt.
	//
	// This allowlist is not a theoretical safeguard here: strictHostResolver
	// genuinely fails resolution for any Host that is not
	// acme.demo.localhost or globex.demo.localhost -- including the Host a
	// plain `curl localhost:8080/healthz` sends, and a request with no Host
	// header at all -- so /healthz depends on this allowlist, in real
	// production wiring, to return 200 for exactly the orchestrators and
	// humans most likely to probe it without ever setting a demo Host.
	//
	// Both GET and HEAD are allowlisted for it, not GET alone: net/http's
	// ServeMux automatically serves HEAD /healthz from the "GET
	// "+healthzPath pattern registered above (Go's long-standing
	// GET-implies-HEAD convenience), but tenancy.Middleware does NOT apply
	// that same convenience to WithAllowlist -- its own doc comment says
	// so explicitly: "allowlist http.MethodHead explicitly if a health
	// check needs it too." Allowlisting GET alone would leave HEAD
	// /healthz 403ing under this exact resolver the moment anything (a
	// load balancer, a script) probes it with HEAD instead of GET --
	// exactly the class of bug this allowlist exists to prevent, just on
	// the one method easy to forget because the mux layer papers over it.
	//
	// The config module's two endpoints are allowlisted for the same
	// reason the health and metrics endpoints are -- but with a
	// difference: config's paths are named through the module's exported
	// constants (config.PathPublic, config.PathSystemFeatures) rather
	// than hand-written strings, because the module owns the paths and a
	// host that re-types them could drift. They are pre-auth display
	// surfaces by design (see go/config/http.go's doc comments): the
	// request must reach the config module's own handler so ITS resolver
	// -- tenancy.NewDomainResolver, see its wiring above -- can map the
	// Host onto the tenant whose brand to render, or onto platform
	// defaults when the Host matches nothing. strictHostResolver must not
	// 403 those requests first: a browser pointed at an unknown Host
	// still deserves a login page and its brand -- docs/internal/
	// 11-cross-cutting.md's dynamic-config section says an unmatched host
	// must resolve to platform defaults and never error, because an
	// unrenderable login page is the worst possible failure mode.
	// Allowlisting here is safe for exactly the reason the endpoints
	// exist: they serve only what the design marks public (display
	// decisions), never tenant data -- the notes API stays behind
	// strictHostResolver, fail-closed as before.
	handler := tenancy.Middleware(resolver,
		tenancy.WithAllowlist(http.MethodGet, healthzPath),
		tenancy.WithAllowlist(http.MethodHead, healthzPath),
		tenancy.WithAllowlist(http.MethodGet, metricsPath),
		tenancy.WithAllowlist(http.MethodHead, metricsPath),
		tenancy.WithAllowlist(http.MethodGet, config.PathPublic),
		tenancy.WithAllowlist(http.MethodHead, config.PathPublic),
		tenancy.WithAllowlist(http.MethodGet, config.PathSystemFeatures),
		tenancy.WithAllowlist(http.MethodHead, config.PathSystemFeatures),
	)(mux)
	return handler, cleanup, nil
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
