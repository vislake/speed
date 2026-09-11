package config

import (
	"embed"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/go/config/migrations"
)

const (
	// moduleName is config's pkgcore.Module.Name(). It is also the module
	// name dbkit.MigrationRegistry.Register keys its dependency graph on.
	moduleName = "config"
)

// bootstrapKeyDecl is the process-start key material this module's cipher
// depends on: the AES cipher key the host builds its dbkit.Cipher from
// (WithCipher) and every Sensitive item's stored value is sealed with.
//
// It is a bootstrap key rather than a configuration item for the reason the
// table states structurally: the key that encrypts the configs table cannot
// live in the configs table. It is also its own secret, never a value derived
// from another module's key material.
var bootstrapKeyDecl = pkgcore.BootstrapKey{
	Key:         "config.cipher_key",
	Format:      "hexkey",
	Default:     "documented non-secret development default",
	Sensitive:   true,
	Description: "The AES cipher key the config module seals every Sensitive dynamic-configuration value with (the configs table stores base64 ciphertext); the key that encrypts the table cannot live in the table, so it comes from the host's process-start input.",
	Group:       moduleName,
}

// SystemPurposeSystemWrite is the audited system purpose a host declares
// (pkgcore.RegisterSystemPurpose, or via tenancy.WithSystemContext's own
// registration path) when it builds the system context that authorizes
// platform-wide configuration writes: a ScopeSystem Set requires the
// context to carry a system reason, and the reason's Purpose is expected
// to be this one. Register calls RegisterSystemPurpose with it, so a host
// that bootstraps this module never needs to register it by hand; the
// constant exists so the host can name the purpose when building the
// reason.
const SystemPurposeSystemWrite pkgcore.SystemPurpose = "config.system_write"

// Module implements pkgcore.Module for go/config: the dynamic
// configuration and feature-flag runtime. Unlike a business module, it
// contributes no
// tenant-scoped behavior of its own -- its configs table is platform data,
// and its endpoints serve unauthenticated display decisions -- so its
// Register declares only the schema-less surfaces: the two endpoints, its
// one event type and its one audit action. The configuration schema and
// feature flags it serves come from the other modules' registrations, and
// are snapshotted by Attach, after Bootstrap has finished registering
// every module.
type Module struct {
	// db is the *gorm.DB the configs table lives in. It is opened and
	// migrated by the host before Register is ever called; the module
	// itself performs no I/O until Attach.
	db *gorm.DB

	// cipher is the host's cipher for Sensitive items (WithCipher). Nil
	// means no Sensitive item may be declared or served -- Attach refuses
	// such a schema with ErrCipherRequired.
	cipher *dbkit.Cipher

	// resolver maps an unauthenticated request to the tenant its public
	// configuration should be resolved for (WithResolver). Nil disables
	// tenant resolution: requests then read platform defaults. The
	// reference app wires tenancy.NewDomainResolver over its host map,
	// whose unmatched-host fallback is the documented, deliberate
	// exception for unauthenticated display decisions (go/tenancy/
	// resolver.go's doc comment) -- config's public endpoints are exactly
	// that kind of decision.
	resolver tenancy.Resolver

	// pollInterval is the anti-loss poller cadence of every Service this
	// Module attaches (WithPollInterval; default DefaultPollInterval).
	pollInterval time.Duration

	// service is the Service Attach produced, nil until then. Routes
	// mounted during Register and the module's Handle (handle.go) resolve
	// it lazily per call, so the window between Register and Attach
	// reports ErrServiceNotAttached.
	service *Service

	// handle is the module's lazy read handle, created by NewModule and
	// returned unchanged by Handle for the module's whole life; its reads
	// resolve m.service per call (handle.go's Handle doc comment).
	handle *Handle

	// attachMu serializes Attach calls. Attach is a once-per-Module
	// startup step, so the lock is uncontended in practice; it exists to
	// make the exactly-once guard in Attach atomic -- without it, two
	// concurrent Attach callers could both pass the m.service == nil
	// check, each building its own Service, subscribing it to the bus and
	// starting its own poller, and the caller whose Service lost the write
	// to m.service would hold a Service whose poller keeps running and
	// whose bus subscription keeps firing, with no way to learn it was
	// orphaned. Held across the whole call, a second concurrent Attach
	// deterministically observes the first's Service and fails with
	// ErrAlreadyAttached.
	attachMu sync.Mutex

	// afterRefreshLock, forwarded onto the attached Service's own field of
	// the same name before the poller starts, exists solely so a test can
	// deterministically synchronize with an in-flight, poller-triggered
	// Refresh call (see TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh).
	// Setting it here, before Attach constructs the Service and starts the
	// poller goroutine, avoids racing that goroutine's first read of the
	// field against a test setting it after the fact. Nil on every
	// production path; there is no exported Option for it.
	afterRefreshLock func()
}

// Option configures a Module built by NewModule.
type Option func(*Module)

// WithCipher wires the cipher Attach uses to seal and unseal
// Sensitive configuration values at rest (dbkit's AES-GCM field-level
// machinery). A module whose schema declares no Sensitive items needs no
// cipher; one that does, and is Attached without one, fails with
// ErrCipherRequired. The module never invents a key of its own: where the
// key comes from (environment, secret store) is the host's business.
func WithCipher(cipher *dbkit.Cipher) Option {
	return func(m *Module) { m.cipher = cipher }
}

// WithResolver wires the request-to-tenant resolver the two unauthenticated
// endpoints use to pick whose public configuration to serve. The interface
// is tenancy's own Resolver so hosts reuse the same resolver type the
// tenancy middleware runs on authenticated routes; config never resolves a
// tenant itself, and never errors a request over an unmatched host -- a
// request whose host resolves to nothing (or for which no resolver is
// wired at all) reads platform defaults, the display decision the design
// mandates for the unauthenticated case.
func WithResolver(resolver tenancy.Resolver) Option {
	return func(m *Module) { m.resolver = resolver }
}

// WithPollInterval overrides the anti-loss poller cadence of the Service
// this Module attaches (default DefaultPollInterval). Zero disables the
// poller entirely; tests use that or a short interval.
func WithPollInterval(interval time.Duration) Option {
	return func(m *Module) { m.pollInterval = interval }
}

// withAfterRefreshLockForTest is unexported: it exists only so
// TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh can
// install the Service's afterRefreshLock hook before Attach starts the
// poller goroutine, never after -- see the Module field's own doc
// comment for why the ordering matters.
func withAfterRefreshLockForTest(hook func()) Option {
	return func(m *Module) { m.afterRefreshLock = hook }
}

// NewModule returns a Module whose configs table lives in db. Constructing
// a Module performs no I/O -- opening and migrating db is the caller's
// responsibility, done once at startup before Bootstrap ever calls
// Register (see examples/reference-app/internal/app's wiring for the exact
// sequence). db must not be nil by the time Attach runs; Register itself
// never touches it (per pkgcore.Module's "declares, never performs I/O"
// contract).
//
// It also creates the module's lazy read handle (Handle), so a host can
// capture it here and wire it into the seams that read configuration
// before Attach has run.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db, pollInterval: DefaultPollInterval}
	m.handle = &Handle{m: m}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. config depends on infrastructure
// (dbkit for the cipher and the table, tenancy for the resolver
// interface) only, never on another business module: the configuration
// schema it serves is declared BY other modules, but it must not depend on
// any of them, because no module may force a config dependency on hosts
// that do not boot that module. The empty list is deliberate, not
// aspirational.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: config ships no user-facing messages
// (its endpoints return structured codes, and the copy of any admin console
// that renders its items would live in whichever module owns that UI), so
// it contributes an empty file set. A module with no locale files at all
// contributes nothing to the catalog and is not an error (go/pkgcore/i18n
// catalog.go's AddModule doc comment).
func (m *Module) Locales() embed.FS { return embed.FS{} }

// openAPISpecYAML is this module's OpenAPI fragment, embedded so it
// travels inside the module binary, exactly as go/sharing's identical
// openAPISpecYAML does for its own fragment.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// OpenAPISpec implements pkgcore.Module: config's own OpenAPI fragment,
// embedded from api/openapi.yaml. The fragment is the single source of the
// module's HTTP surface -- the api package's generated types and
// ServerInterface (api/config-server.gen.go, regenerated by task api:gen)
// derive from it, and (*Module) implements that interface (see http.go) --
// so a spec change without a matching handler change cannot compile.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it mounts the two
// endpoints through this module's spec fragment's generated wrapper
// (preAuthHandler), registers the module's own system purpose (idempotent,
// process-global), and declares the event and audit-action vocabulary. The
// configuration schema is deliberately NOT read here: other modules may
// register after this one, and the schema snapshot must be complete before
// it freezes, so the snapshot happens in Attach -- after Bootstrap has
// returned -- never in Register.
func (m *Module) Register(reg pkgcore.Registrar) error {
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)

	// One handler, two mounts: the fragment's wrapper dispatches on a
	// request's own literal path, so the same http.Handler serves both
	// paths -- the identical shape go/sharing's Register gives its own
	// fragment. Each mount's own host-applied gating (allowlisted or not)
	// therefore only ever wraps the requests it is meant to, because a
	// request for one path never matches the other's pattern.
	fragment := m.preAuthHandler()
	reg.RoutesSeat().Mount(PathPublic, fragment)
	reg.RoutesSeat().Mount(PathSystemFeatures, fragment)

	if err := reg.EventsSeat().Publishes(eventDecl); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(AuditActionConfigSet); err != nil {
		return err
	}
	// The process-start key material (bootstrapKeyDecl) is descriptor data:
	// the component descriptor carries it as BootstrapKeys, which the loader
	// resolves before anything is constructed.
	return nil
}

// Attach freezes the schema snapshot and hands the caller the runtime
// Service. It must be called exactly once, after Kernel.Bootstrap has
// returned, with the registry Bootstrap produced: only then has every
// module registered, so reg.Config.Items() and reg.Features.Flags() are
// the complete declarations the runtime schema folds together (see
// buildSchema).
//
// Attach wires the Service's store to the Module's db, captures the
// registry's KVStore as the backend of the pre-auth endpoints' rate
// limiting, validates the cipher against the schema's Sensitive items
// (ErrCipherRequired when a Sensitive item exists without one), subscribes
// the Service to config.item.changed on the registry's bus, and starts the
// anti-loss poller. Routes mounted at Register resolve the Service lazily, so a
// request served between Register and Attach reports ErrServiceNotAttached;
// a host wires Attach immediately after Bootstrap, before serving.
//
// A second Attach on the same Module fails with ErrAlreadyAttached: the
// schema snapshot freezes at the first call, and a second snapshot could
// silently diverge from the first. The guard is atomic -- Attach holds an
// internal lock for its whole body -- so the same holds under concurrent
// callers: exactly one Attach succeeds and every other call fails with
// ErrAlreadyAttached, never a second Service with its own poller and bus
// subscription.
func (m *Module) Attach(reg pkgcore.Registrar) (*Service, error) {
	m.attachMu.Lock()
	defer m.attachMu.Unlock()
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("config: Attach requires a non-nil *pkgcore.Registry (pass the registry Kernel.Bootstrap returned)")
	}
	if m.db == nil {
		return nil, errors.New("config: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	schema, err := buildSchema(reg.ConfigSeat().Items(), reg.FeaturesSeat().Flags())
	if err != nil {
		return nil, err
	}
	needsCipher := false
	for _, item := range schema.items {
		if item.sensitive {
			needsCipher = true
			break
		}
	}
	if needsCipher && m.cipher == nil {
		return nil, ErrCipherRequired
	}

	svc := &Service{
		schema:           schema,
		st:               &store{db: m.db},
		bus:              reg.EventBus(),
		kv:               reg.KVStore(),
		cipher:           m.cipher,
		cache:            newValueCache(),
		watchers:         &watchers{byKey: make(map[string][]watch)},
		pollInterval:     m.pollInterval,
		afterRefreshLock: m.afterRefreshLock,
	}
	reg.EventsSeat().Subscribe(EventConfigItemChanged, svc.onItemChanged)
	svc.startPoller()
	m.service = svc
	return svc, nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
