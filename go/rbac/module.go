package rbac

import (
	"embed"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/rbac/locales"
	"github.com/vislake/speed/go/rbac/migrations"
)

// moduleName is rbac's pkgcore.Module.Name(). It is also the module name
// dbkit.MigrationRegistry.Register keys its dependency graph on.
const moduleName = "rbac"

// DefaultCacheTTL is how long a process-local authorization decision may
// survive without confirmation when no invalidation event has arrived.
//
// It is a package-level named constant, the third of the three homes the
// backend coding standard §10 allows for a value like this ("stable domain
// defaults"), deliberately NOT a dynamic configuration item: reading one
// would make rbac depend on the config module, an edge the dependency
// graph in docs/internal/01-architecture.md does not have and that would
// be paid for by every consumer that boots rbac without config. A host
// that needs a different lifetime passes WithCacheTTL.
const DefaultCacheTTL = 30 * time.Second

// Permission strings rbac declares for its own management surface. They
// give a caller the vocabulary to gate role administration; rbac itself
// does not check them. That is deliberate: rbac is the decision engine,
// not its own gatekeeper -- an engine that authorized its own writes would
// have to answer "who may grant the first role" with a special case, and
// special cases in an authorization engine are where the holes live. The
// same posture config.Set takes.
const (
	// PermissionRead covers reading roles, their permissions and their
	// bindings.
	PermissionRead = "rbac:read"

	// PermissionManage covers defining roles and assigning or revoking
	// them.
	PermissionManage = "rbac:manage"
)

// Module implements pkgcore.Module for go/rbac: the role-based access
// control engine (docs/internal/05-identity-and-access.md).
//
// It declares its own permissions, events and audit actions during
// Register, and takes the frozen snapshot of EVERY module's declared
// permissions during Attach -- after Bootstrap has returned, because
// modules register in bootstrap order and a snapshot taken earlier would
// be partial. That is the same two-phase shape go/config uses for its
// configuration schema, for the same reason.
type Module struct {
	// db is the *gorm.DB rbac's three tables live in. It is opened and
	// migrated by the host before Register is ever called; the module
	// itself performs no I/O until Attach.
	db *gorm.DB

	// subtree is the host's organization-tree seam (WithSubtreeResolver),
	// nil when the host wired none. See Service.subtree for what nil means.
	subtree SubtreeResolver

	// cacheTTL is the decision cache's anti-loss expiry (WithCacheTTL;
	// default DefaultCacheTTL).
	cacheTTL time.Duration

	// service is the Service Attach produced, nil until then. It is what
	// makes a second Attach detectable.
	service *Service
}

// Option configures a Module built by NewModule.
type Option func(*Module)

// WithSubtreeResolver wires the host's organization-tree seam, the
// interface that maps a node id to its materialized path (see scope.go).
//
// It is optional. A host with no organization module wires none and every
// tenant-wide binding keeps working; only node-scoped bindings need it,
// and without it they DENY rather than widening to the tenant. Wiring one
// is how rbac reaches the org tree without importing org.
func WithSubtreeResolver(r SubtreeResolver) Option {
	return func(m *Module) { m.subtree = r }
}

// WithCacheTTL overrides the decision cache's anti-loss expiry (default
// DefaultCacheTTL). A non-positive value is ignored, so a caller that
// passes a zero duration by accident gets the safe default rather than a
// cache that never expires.
func WithCacheTTL(ttl time.Duration) Option {
	return func(m *Module) {
		if ttl > 0 {
			m.cacheTTL = ttl
		}
	}
}

// NewModule returns a Module whose tables live in db. Constructing a
// Module performs no I/O -- opening and migrating db is the caller's
// responsibility, done once at startup before Bootstrap ever calls
// Register. db must not be nil by the time Attach runs; Register itself
// never touches it, per pkgcore.Module's "declares, never performs I/O"
// contract.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db, cacheTTL: DefaultCacheTTL}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. rbac depends on infrastructure
// (pkgcore, dbkit, tenancy) only.
//
// The empty list is the module's defining property, not an oversight, and
// one entry in particular must never appear in it: authn. Authorization
// knows only Subject{TenantID, UserID}; the authenticating side assembles
// the Subject and calls in. An authn dependency here -- in this list, in
// an import, or in a test -- inverts the whole design.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: one message per error code in
// errors.go, in both zh-CN and en-US with identical id sets (the parity
// pkgcore/i18n's Builder.AddModule enforces while Kernel.Bootstrap merges
// the catalog).
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: nil.
//
// rbac mounts no HTTP endpoints of its own, on purpose. Role management is
// an admin-console surface, and the flat permission list a signed-in user
// needs belongs to /me, which authn owns -- rbac supplies the evaluation
// call authn's handler makes. rbac's contribution to the HTTP layer is the
// middleware in the fixed chain docs/internal/01-architecture.md names
// (authn.Middleware, then rbac's permission gate), not a route. Both
// deferrals are recorded in this module's AGENTS.md.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it declares this module's
// own permission vocabulary, its domain events and its audit actions, and
// touches neither the database nor the network.
//
// The permission CATALOG is deliberately not read here: other modules may
// register after this one, so the snapshot must be taken once every module
// has declared, which is what Attach does.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(PermissionRead, PermissionManage); err != nil {
		return err
	}
	if err := reg.Events.Publishes(eventDecls...); err != nil {
		return err
	}
	return reg.AuditActions.Add(auditActions...)
}

// Attach freezes the permission catalog and hands the caller the runtime
// Service. It must be called exactly once, after Kernel.Bootstrap has
// returned, with the registry Bootstrap produced: only then has every
// module registered, so reg.Permissions.Permissions() is the complete
// declaration the catalog snapshots.
//
// A second Attach on the same Module fails with ErrAlreadyAttached: the
// catalog freezes at the first call, and a second snapshot could silently
// diverge from the first -- which, for the set that decides whether a
// grant is legal, is a security-relevant difference rather than a
// cosmetic one.
func (m *Module) Attach(reg *pkgcore.Registry) (*Service, error) {
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("rbac: Attach requires a non-nil *pkgcore.Registry (pass the registry Kernel.Bootstrap returned)")
	}
	if m.db == nil {
		return nil, errors.New("rbac: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	svc := &Service{
		catalog:         newCatalog(reg.Permissions.Permissions()),
		roles:           NewRoleRepository(m.db),
		rolePermissions: NewRolePermissionRepository(m.db),
		bindings:        NewRoleBindingRepository(m.db),
		subtree:         m.subtree,
		bus:             reg.Events.Bus(),
		cacheTTL:        m.cacheTTL,
	}
	m.service = svc
	return svc, nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
