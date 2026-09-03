package notification

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/notification/locales"
	"github.com/vislake/speed/go/notification/migrations"
)

// moduleName is notification's pkgcore.Module.Name(). It is also the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "notification"

// Module implements pkgcore.Module for go/notification: the tenant's
// notification inbox, its per-type channel preference matrix and -- in this
// round's later blocks -- the consent ledger and delivery subscriber that
// fill it.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// The module's tables live in db, which the host opened through dbkit.Open
// (so it carries the tenant-isolation plugin) and migrated before Bootstrap.
// Register then contributes the module's declarations -- its event catalog,
// and the attachment of the host registry's notification-type registrar to
// the preference service -- to the host's registry. The module performs no
// I/O of its own during registration.
type Module struct {
	// db is the connection the module's tables live in. The inbox and
	// preference repositories are built over it, and the delivery subscriber
	// of a later block builds its own over this same connection.
	db *gorm.DB

	// prefs is the preference matrix's decision layer, built at construction
	// and served to consumers through Preferences(). Its type-taxonomy
	// reference is attached during Register (attachTypes); before that it is
	// nil, which the service treats as an empty taxonomy.
	prefs *PreferenceService
}

// Option configures a Module at construction time.
//
// The first concrete options arrive with the seams later blocks inject --
// the delivery subscriber's queue and preference reader above all. The
// variadic plumbing exists from this block on so that every option a later
// block adds is a drop-in, exactly as org's Option machinery works.
type Option func(*Module)

// NewModule returns a Module whose tables live in db. Constructing a Module
// performs no I/O: opening and migrating db is the host's responsibility,
// done once at startup before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{
		db:    db,
		prefs: NewPreferenceService(db),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Preferences returns the module's preference service -- the matrix's only
// sanctioned read/write face. A host hands this to its HTTP handler once
// Bootstrap has run, so the service's type-taxonomy reference is attached
// (Register) by the time any caller reaches it.
func (m *Module) Preferences() *PreferenceService {
	return m.prefs
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing.
//
// This is a real answer, not a stub. notification sits above jobs, config
// and dbkit in docs/internal/01-architecture.md's graph, but none of those
// is a pkgcore.Module -- they are libraries the host wires, and DependsOn
// enumerates only modules in the bootstrap set. notification must also NOT
// depend on authn, rbac or org: it learns about users, memberships and
// authorization from domain events and opaque ids, never from their Go
// types, and every id its tables store cites exactly that rule. Naming any
// of them here would make notification unbootable in a host that does not
// run them, and would invert the event-driven direction the module exists
// to serve.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of notification's
// error codes, in both supported languages with identical id sets. The
// bundle's entries travel under notification.* ids only -- the preference
// matrix's codes (errors.go) -- never templates for other modules'
// notification types, which live in the declaring modules' own bundles (see
// render.go's template-id convention).
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: nil.
//
// notification has no OpenAPI fragment yet, because it has no HTTP surface
// yet: the module's routes arrive in this round's later blocks, and the
// fragment -- go/notification/api/openapi.yaml, joining the spec-first
// pipeline of docs/internal/21-api-contract.md -- ships with them. Until
// then a nil spec contributes nothing to the merged document, which is the
// same nothing every fragment-less module contributes today.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db.
//
// This block contributes two declarations. The module's event catalog
// (EventInboxCreated, one declared event) is published first; then the host
// registry's notification-type registrar is attached to the preference
// service (attachTypes), giving it the live taxonomy every preference write
// validates against. No permissions, audit actions or routes are declared
// yet, because the module has no caller-scoped operation and no request
// path until the later blocks of this round build them; each arrives with
// the producer that needs it, exactly as errors.go's doc comment says of
// error codes.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Events.Publishes(inboxEventDecls...); err != nil {
		return err
	}
	m.prefs.attachTypes(reg.Notifications)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
