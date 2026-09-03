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

	// DeletedAt and DeletedBy are dbkit.SoftDeletable's required pair
	// (dbkit/soft_delete.go): implementing that interface below is what
	// makes dbkit.Repository[Note].Delete a mark-delete (one UPDATE
	// setting these two columns) instead of today's physical DELETE, and
	// what makes dbkit.Repository[Note].Restore meaningful for this model.
	// Neither field is ever set by application code directly -- both are
	// written through dbkit's own reflection-based field access, exactly
	// as TenantID is (see dbkit/repository.go's setTenantID /
	// setSoftDeleteFields).
	//
	// This is notes' service-level proof of the mark-delete round
	// (docs/internal/04-data-and-tenancy.md's delete-semantics section): repository_test.go's
	// TestRepository_DeleteThenRestoreThenDelete_HiddenFromNormalQueriesThroughoutLifecycle
	// drives Create/Delete/Restore/Delete straight through this package's
	// real, migrated Repository, promoted unchanged from
	// dbkit.Repository[Note] -- no code in this package's repository.go
	// itself needed to change for that proof to hold. Notes does not
	// expose a delete/restore HTTP endpoint yet (a good, named follow-up
	// scope, not required for this proof), and the delete-semantics
	// section's hard-delete half is proved at this same service level
	// rather than over HTTP: repository_test.go's
	// TestRepository_HardDelete_SoftDeletedNote_PhysicallyRemoved drives
	// dbkit.Repository[Note].HardDelete -- promoted unchanged from the
	// embedded base, exactly like Delete and Restore, with no code in
	// this package's repository.go needed -- through this same real,
	// migrated Repository. No migration change was needed or made for
	// it: HardDelete issues the physical DELETE the pre-soft-delete
	// schema already permitted, which is also why this migration's own
	// doc comment (migrations/{postgres,sqlite}/0002_add_soft_delete.sql)
	// says it adds nothing for HardDelete.
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	DeletedBy string     `gorm:"column:deleted_by;not null;default:''"`
}

// GetDeletedAt returns Note's soft-delete marker, satisfying
// dbkit.SoftDeletable. Like GetTenantID, this is never called by dbkit's
// soft-delete auto-scope plugin or by Repository[Note] itself -- it is a
// pure marker used only for the capability check that routes
// dbkit.Repository[Note].Delete onto the mark-delete path; the actual
// field writes go through reflection on fixed field names.
func (n Note) GetDeletedAt() *time.Time { return n.DeletedAt }

// AuditResourceType implements dbkit.Auditable: it names notes' audit
// resource kind "note", the label dbkit's automatic GORM write-capture
// plugin would attach to a Note write's WriteCapturedEvent if a host
// wired dbkit.Options.AuditBus for this model's database connection.
//
// cmd/server's buildServer deliberately does NOT wire AuditBus for this
// app's own shared connection -- see its own doc comment on the call to
// dbkit.Open for a real, empirically-confirmed deadlock (SQLite allows
// only one writer per file, and dbkit.Repository[Note]'s write transaction
// is still open when that plugin's callback would fire) that the
// automatic mechanism hits whenever the audit persister shares a database
// file with the model being captured. Note implements Auditable anyway,
// both because a future fix to that mechanism (or a host wiring a
// dedicated audit connection) should not require touching this model
// again, and because it is real, tested behavior in its own right (see
// model_test.go's TestNote_AuditResourceType_ReturnsNote and
// go/dbkit/example_test.go's ExampleAuditable).
//
// This app's actual audit trail for note creation goes through the
// declarative mechanism instead: handler.go's NotesCreateNote calls
// audit.Emit explicitly, after h.repo.Create has already returned (so
// after that write's own transaction has committed, which is exactly why
// Emit's call site does not hit the same hazard) -- see
// server_test.go's TestBuildServer_NoteCreate_PersistsAuditEvent for the
// end-to-end proof that a real POST /api/v1/notes request produces a
// persisted go/dbkit/audit.AuditEvent row with Action "notes.note.create".
func (Note) AuditResourceType() string { return "note" }

// compile-time check that Note satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Note{}

// compile-time check that Note satisfies dbkit.Auditable.
var _ dbkit.Auditable = Note{}

// compile-time check that Note satisfies dbkit.SoftDeletable.
var _ dbkit.SoftDeletable = Note{}
