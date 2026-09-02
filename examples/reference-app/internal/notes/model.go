package notes

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// Note is notes' tenant-scoped placeholder resource: a minimal text record
// standing in for the real reference-app content that lands in later
// milestones (see this package's doc.go).
//
// It embeds dbkit.TenantModel for its tenant_id column and GetTenantID
// method, following the "Typical integration" pattern dbkit's own
// AGENTS.md documents for a tenant-scoped model that does not need
// tenant_id in a composite primary key. That is a deliberate choice, not
// an oversight: TenantModel's own gorm tag omits "primaryKey" on purpose
// (see dbkit's tenant_scope.go doc comment on TenantModel), because a
// tenant-scoped table's primary key should usually be the composite
// (tenant_id, id) per the backend coding standard's data-model rules
// (§5) -- but ID here is an application-generated UUID (see
// handler.go's use of uuid.NewString), already globally unique on its
// own, so a plain, non-key tenant_id column backed by its own secondary
// index (see migrations/sqlite/0001_create_notes.sql) is genuinely enough.
// A resource whose id space is not already globally unique on its own
// should instead declare TenantID directly, with its own primaryKey tag,
// exactly as dbkit's AGENTS.md's own Subscription example does, rather
// than embedding TenantModel.
//
// Do not redeclare a same-named TenantID field on Note to shadow the
// promoted one from TenantModel (for example, to add a primaryKey tag
// without giving up the embedding): dbkit's own tenant_scope.go doc
// comment on TenantModel documents exactly how that silently breaks
// GetTenantID and, with it, every tenant's own dbkit.Repository[Note]
// FindByID call -- not only an attacker's.
type Note struct {
	// ID is an application-generated UUID (see handler.go), never a
	// database-generated one -- the backend coding standard (§5) requires
	// primary keys to be generated in the application, and forbids
	// PostgreSQL-only generators such as gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped) -- see the doc comment above for why
	// Note embeds it instead of declaring TenantID directly.
	dbkit.TenantModel

	// Text is the note's placeholder content. It stands in for whatever
	// real field(s) a later milestone's module will actually need.
	Text string `gorm:"column:text;size:4000;not null"`

	// CreatedAt is populated by gorm's autoCreateTime on Create -- never
	// written by application code, and never NOW() in a migration (backend
	// coding standard §5's dual-dialect rule: SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// compile-time check that Note satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Note{}
