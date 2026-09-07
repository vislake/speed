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
	// real field(s) a later milestone's module will actually need, and in
	// this app's own domain the real content is patient data.
	//
	// The audit:"redact" struct tag is dbkit's model-side capture opt-out
	// for a plaintext-sensitive column (go/dbkit/audit_capture.go's
	// fieldValuesMap doc comment): whenever dbkit's automatic write-capture
	// plugin captures this model on a connection whose capture scope admits
	// Note, the text column travels as the "[redacted]" marker -- never as
	// its plaintext -- while the column's key staying present keeps the
	// fact that the write touched the column visible to a diff reader. A
	// note's body is tenant user content that must never enter the
	// append-only audit trail verbatim, and declaring that on the field
	// itself makes the protection host-independent: it survives on any
	// connection that captures Note, whatever a host's capture-scope list
	// does (see the AuditResourceType doc comment below for why that
	// host-side exclusion alone cannot be the whole protection).
	Text string `gorm:"column:text;size:4000;not null" audit:"redact"`

	// CreatorUserID is the user id of the note's creator, attributed by the
	// host's SubjectResolver seam at create time (handler.go's
	// NotesCreateNote resolves it before the body is even read) -- never by
	// the request itself, and never written after Create. It is the
	// attribute that makes a note addressable as the compliance mechanism's
	// subject: retention_participant.go's Erase erases every note whose
	// CreatorUserID matches a pkgcore.SubjectRef's SubjectID, and Export
	// gathers a tenant's notes by creator. It shares its source with
	// NoteCreatedPayload.CreatorUserID (module.go) -- both carry the exact
	// string h.resolveSubject returned for the creating request -- so an
	// event subscriber and a later compliance read of the row can never
	// disagree about who created it.
	//
	// The column carries a NOT NULL DEFAULT '' so pre-existing rows and any
	// caller that creates a note without a resolvable creator (tests, seed
	// code) stay valid: an empty CreatorUserID is the "no attributable
	// creator" sentinel, and no compliance operation will ever target it,
	// just as resolveSubject treats an empty resolver answer as no answer.
	CreatorUserID string `gorm:"column:creator_user_id;size:64;not null;default:''"`

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
// plugin attaches to a Note write's WriteCapturedEvent on any connection
// whose capture scope admits Note.
//
// This app's own shared connection is a wired one: cmd/server's
// buildServer sets dbkit.Options.AuditBus on its dbkit.Open call (the
// same bus instance Kernel.Bootstrap later receives through
// WithEventBus), and that call's Options.AuditModels scope lists org's
// three models and deliberately nothing else. Note stays off that list by
// design -- one host-side list line -- because this module records its
// own note trail declaratively through audit.Emit (handler.go's
// NotesCreateNote, under the registered notes.note.create audit action):
// admitting Note to the automatic scope would double-record creation, and
// would do so with the note's full body, since dbkit's documented default
// when Options.AuditModels is empty or nil is capture-everything -- every
// Auditable model written through the connection lands, whole column
// values included, in an append-only trail.
//
// That asymmetry is why the plaintext protection cannot live only in the
// host's list line, where the model's own body cannot be read: a host
// dropping or relaxing the list -- or a consumer wiring AuditBus on a
// connection that writes this model under dbkit's default semantics --
// would silently start capturing every note verbatim, with no test or
// gate going red. The model-side half of the protection is therefore
// declared here, on the model: Note.Text carries dbkit's audit:"redact"
// capture opt-out (see that field's comment and go/dbkit/audit_capture.go's
// fieldValuesMap doc comment), so even on a capturing connection the text
// column travels as "[redacted]", never the plaintext. The tag travels
// with the model wherever it is written, which a host wiring file cannot
// do for the consumers of a library module; it is read only for Auditable
// models, which is why Note keeps the marker (and with it a declared
// resource kind) rather than dropping the interface and with it the
// question. (An earlier wiring deliberately left AuditBus unwired
// entirely because a same-file persister deadlocked under SQLite's single
// writer; that hazard was dissolved by dbkit's buffered post-commit
// publish, proven by audit_capture_test.go's
// TestAuditCapturePlugin_WithTenantSession_SameFileSynchronousPersister_NoLongerDeadlocks,
// so nothing in the current shape -- bus wired, Note excluded, Text
// tagged -- rests on that old limitation.)
//
// This app's actual audit trail for note creation runs through the
// declarative mechanism: handler.go's NotesCreateNote calls audit.Emit
// explicitly, after h.repo.Create has already returned (so after that
// write's own transaction has committed, which keeps Emit's write out of
// Create's transaction) -- see server_test.go's
// TestBuildServer_NoteCreate_PersistsAuditEvent for the end-to-end proof
// that a real POST /api/v1/notes request produces a persisted
// go/dbkit/audit.AuditEvent row with Action "notes.note.create", and
// model_test.go's
// TestNote_AuditCapture_DefaultScope_CapturesTextRedacted for the proof
// that even a default-scope automatic capture carries no note body.
func (Note) AuditResourceType() string { return "note" }

// compile-time check that Note satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = Note{}

// compile-time check that Note satisfies dbkit.Auditable.
var _ dbkit.Auditable = Note{}

// compile-time check that Note satisfies dbkit.SoftDeletable.
var _ dbkit.SoftDeletable = Note{}
