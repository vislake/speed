package config

import (
	"embed"
	"errors"
	"net/http"
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
// configuration and feature-flag runtime (docs/internal/11-cross-cutting.
// md's config section). Unlike a business module, it contributes no
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

	// cipher is the host's master-key cipher for Sensitive items
	// (WithCipher). Nil means no Sensitive item may be declared or
	// served -- Attach refuses such a schema with ErrCipherRequired.
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
	// mounted during Register resolve it lazily per request, so the
	// window between Register and Attach reports ErrServiceNotAttached.
	service *Service
}

// Option configures a Module built by NewModule.
type Option func(*Module)

// WithCipher wires the master-key cipher Attach uses to seal and unseal
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

// NewModule returns a Module whose configs table lives in db. Constructing
// a Module performs no I/O -- opening and migrating db is the caller's
// responsibility, done once at startup before Bootstrap ever calls
// Register (see examples/reference-app/cmd/server's wiring for the exact
// sequence). db must not be nil by the time Attach runs; Register itself
// never touches it (per pkgcore.Module's "declares, never performs I/O"
// contract).
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db, pollInterval: DefaultPollInterval}
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
// in this milestone (its endpoints return structured codes, and the future
// admin console's copy will live in whichever module owns that UI), so it
// contributes an empty file set. A module with no locale files at all
// contributes nothing to the catalog and is not an error (go/pkgcore/i18n
// catalog.go's AddModule doc comment).
func (m *Module) Locales() embed.FS { return embed.FS{} }

// OpenAPISpec implements pkgcore.Module: nil. The two endpoints this
// module mounts are pre-auth display surfaces whose OpenAPI fragment is
// deferred to the authn round (recorded in go/config/AGENTS.md's known
// limitations), so there is no spec to embed yet.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it mounts the two
// endpoints, registers the module's own system purpose (idempotent,
// process-global), and declares the event and audit-action vocabulary. The
// configuration schema is deliberately NOT read here: other modules may
// register after this one, and the schema snapshot must be complete before
// it freezes, so the snapshot happens in Attach -- after Bootstrap has
// returned -- never in Register.
func (m *Module) Register(reg *pkgcore.Registry) error {
	pkgcore.RegisterSystemPurpose(SystemPurposeSystemWrite)

	reg.Routes.Mount(PathPublic, http.HandlerFunc(m.handlePublic))
	reg.Routes.Mount(PathSystemFeatures, http.HandlerFunc(m.handleFeatures))

	if err := reg.Events.Publishes(eventDecl); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(AuditActionConfigSet); err != nil {
		return err
	}
	return nil
}

// Attach freezes the schema snapshot and hands the caller the runtime
// Service. It must be called exactly once, after Kernel.Bootstrap has
// returned, with the registry Bootstrap produced: only then has every
// module registered, so reg.Config.Items() and reg.Features.Flags() are
// the complete declarations the runtime schema folds together (see
// buildSchema).
//
// Attach wires the Service's store to the Module's db, validates the
// cipher against the schema's Sensitive items (ErrCipherRequired when a
// Sensitive item exists without one), subscribes the Service to
// config.item.changed on the registry's bus, and starts the anti-loss
// poller. Routes mounted at Register resolve the Service lazily, so a
// request served between Register and Attach reports ErrServiceNotAttached;
// a host wires Attach immediately after Bootstrap, before serving.
//
// A second Attach on the same Module fails with ErrAlreadyAttached: the
// schema snapshot freezes at the first call, and a second snapshot could
// silently diverge from the first.
func (m *Module) Attach(reg *pkgcore.Registry) (*Service, error) {
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("config: Attach requires a non-nil *pkgcore.Registry (pass the registry Kernel.Bootstrap returned)")
	}
	if m.db == nil {
		return nil, errors.New("config: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	schema, err := buildSchema(reg.Config.Items(), reg.Features.Flags())
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
		schema:       schema,
		st:           &store{db: m.db},
		bus:          reg.Events.Bus(),
		cipher:       m.cipher,
		cache:        newValueCache(),
		watchers:     &watchers{byKey: make(map[string][]watch)},
		pollInterval: m.pollInterval,
	}
	reg.Events.Subscribe(EventConfigItemChanged, svc.onItemChanged)
	svc.startPoller()
	m.service = svc
	return svc, nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
