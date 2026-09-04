package integration

import (
	"embed"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/integration/locales"
	"github.com/vislake/speed/go/integration/migrations"
)

// moduleName is integration's pkgcore.Module.Name(). It is also the module
// name dbkit.MigrationRegistry.Register keys its dependency graph on.
const moduleName = "integration"

// MaxAPIKeyLifetime is both the forced expiry ceiling Service.Create
// enforces on every request (ErrExpiryExceedsMaximum past it) and the
// default it applies when a request names no ExpiresAt at all, per the
// design doc's "强制到期时间上限（默认 1 年）" -- the ceiling and the
// unspecified-request default are declared as the same one year, not two
// numbers that happen to agree.
//
// It is a package-level named constant, not a dynamic configuration item,
// for the identical reason go/rbac's DefaultCacheTTL gives: reading one
// from go/config would add a dependency edge this module's position in
// docs/internal/01-architecture.md's graph does not have, paid for by every
// consumer that boots this module without config. A host that genuinely
// needs a different ceiling is a later round's WithMaxAPIKeyLifetime
// option, not a reason to wire config in now.
const MaxAPIKeyLifetime = 365 * 24 * time.Hour

// Permission strings this module declares for its own API key management
// surface. Like go/rbac's own PermissionRead/PermissionManage, this module
// does not check them itself -- it declares the vocabulary; enforcement is
// whatever authorization layer the host wires in front of a future HTTP
// surface (round 1 ships no mounted routes; see AGENTS.md).
const (
	// PermissionRead covers listing a tenant's API keys.
	PermissionRead = "integration:apikey:read"

	// PermissionManage covers creating, rotating and revoking API keys.
	PermissionManage = "integration:apikey:manage"
)

// Audit actions this module contributes, following the
// "<module>.<entity>.<action>" convention (backend coding standard §8).
// Service calls audit.Emit under each of these after the corresponding
// mutation commits; see service.go.
const (
	// AuditActionAPIKeyCreate is emitted after Service.Create persists a
	// new key.
	AuditActionAPIKeyCreate = "integration.apikey.create"

	// AuditActionAPIKeyRotate is emitted after Service.Rotate persists the
	// replacement key and revokes its predecessor.
	AuditActionAPIKeyRotate = "integration.apikey.rotate"

	// AuditActionAPIKeyRevoke is emitted after Service.Revoke persists a
	// revocation.
	AuditActionAPIKeyRevoke = "integration.apikey.revoke"
)

// auditActionDecls is every audit action this module declares through
// Register, kept as one slice so Register and any test enumerating "every
// action this module contributes" read from a single place.
var auditActionDecls = []string{
	AuditActionAPIKeyCreate,
	AuditActionAPIKeyRotate,
	AuditActionAPIKeyRevoke,
}

// ErrAlreadyAttached reports a second Attach call on one Module.
var ErrAlreadyAttached = apperr.Internal("integration.already_attached")

// Module implements pkgcore.Module for go/integration.
//
// It declares its own permissions and audit actions during Register, and
// builds the runtime Service during Attach, mirroring the two-phase shape
// go/rbac and go/config both use: Register only declares (per
// pkgcore.Module's own contract, "must not perform I/O"), and Attach is
// where the seams a host wired through the New Module options actually get
// used.
type Module struct {
	// db is the *gorm.DB this module's one table lives in. It is opened and
	// migrated by the host before Register is ever called; the module
	// itself performs no I/O until Attach.
	db *gorm.DB

	// permissions is the mandatory PermissionLister seam (see seams.go and
	// WithPermissionLister). Left nil is a legal Module construction --
	// Attach does not refuse it -- but Service.Create then refuses any
	// request naming a non-empty Scopes with ErrPermissionListerUnavailable,
	// since a security check with nothing to check against must fail
	// closed, not silently skip itself.
	permissions PermissionLister

	// membership is the optional MembershipChecker seam (see seams.go and
	// WithMembershipChecker). Nil is a fully legal, permanent configuration:
	// it only blanks List's cosmetic CreatorLeft flag, never a security
	// property.
	membership MembershipChecker

	// clock is the injectable time source tests override through
	// withClock; production always uses the zero value's time.Now.
	clock func() time.Time

	// service is the Service Attach produced, nil until then. It is what
	// makes a second Attach detectable.
	service *Service
}

// Option configures a Module built by NewModule.
type Option func(*Module)

// WithPermissionLister wires the host's permission-listing seam. See
// PermissionLister's own doc comment in seams.go for why this is a
// structurally-typed seam rather than a direct go/rbac import, and for a
// concrete wiring example over a real rbac.Service.
func WithPermissionLister(l PermissionLister) Option {
	return func(m *Module) { m.permissions = l }
}

// WithMembershipChecker wires the host's optional "is this user still a
// member" seam. See MembershipChecker's own doc comment in seams.go for why
// this is optional where WithPermissionLister is not.
func WithMembershipChecker(c MembershipChecker) Option {
	return func(m *Module) { m.membership = c }
}

// withClock overrides the Service's time source. Unexported: it exists for
// this module's own tests (expiry-ceiling and rotation-timing assertions
// that would otherwise race the wall clock), never for a host to call.
func withClock(now func() time.Time) Option {
	return func(m *Module) { m.clock = now }
}

// NewModule returns a Module whose table lives in db. Constructing a Module
// performs no I/O -- opening and migrating db is the caller's
// responsibility, done once at startup before Bootstrap ever calls
// Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module.
//
// The empty list is deliberate, not an oversight: this module reaches
// permission listing and membership checking through the structurally-typed
// seams in seams.go, never through an import of go/rbac, go/authn or
// go/org, so none of them belongs in this list -- DependsOn names compile-
// time module dependencies for Kernel.Bootstrap's topological ordering, and
// this module has none among the business modules.
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
// Round 1 mounts no HTTP routes of its own -- see AGENTS.md's "Deferred to
// a later round" section for why a minimal Go-level 429 translation
// (httpguard.go) was judged worth building now while the full CRUD surface
// and its OpenAPI fragment were not.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract
// ("It must not perform I/O; it only declares"), it declares this module's
// permission vocabulary and its audit actions, and touches neither the
// database nor the network.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(PermissionRead, PermissionManage); err != nil {
		return err
	}
	return reg.AuditActions.Add(auditActionDecls...)
}

// Attach builds and returns the runtime Service. It must be called exactly
// once, after Kernel.Bootstrap has returned, with the registry Bootstrap
// produced -- the same convention go/rbac's Attach documents, for the same
// reason: only after Bootstrap has every module registered is
// reg.Events.Bus() and reg.AuditActions wired to the real, final set every
// other module contributed.
//
// A second Attach on the same Module fails with ErrAlreadyAttached.
func (m *Module) Attach(reg *pkgcore.Registry) (*Service, error) {
	if m.service != nil {
		return nil, ErrAlreadyAttached
	}
	if reg == nil {
		return nil, errors.New("integration: Attach requires a non-nil *pkgcore.Registry (pass the registry Kernel.Bootstrap returned)")
	}
	if m.db == nil {
		return nil, errors.New("integration: Attach requires the database NewModule was built with (its db argument must not be nil)")
	}

	clock := m.clock
	if clock == nil {
		clock = time.Now
	}

	svc := &Service{
		repo:         NewAPIKeyRepository(m.db),
		permissions:  m.permissions,
		membership:   m.membership,
		bus:          reg.Events.Bus(),
		auditActions: reg.AuditActions,
		now:          clock,
	}

	m.service = svc
	return svc, nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
