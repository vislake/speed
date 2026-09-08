package cases

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// casesTable is the persisted table of caseRecord.
const casesTable = "cases"

// casePhotosTable is the persisted table of casePhotoRecord.
const casePhotosTable = "case_photos"

// caseRecord is one patient Case: a treatment scenario a clinic staff
// member works, carrying the clinic-given patient record (embedded -- see
// the package doc comment's "Shape decision" section for why there is no
// separate patients table) and owning the case's photos (their rows live
// in case_photos; the case's per-photo simulations stay in smilesim's
// per-photo index, keyed by photo object id, never re-keyed here).
//
// It follows the exact tenant-data shape of notes.Note and smilesim's
// simulationRecord (internal/notes/model.go and
// internal/smilesim/simulation_store.go): it embeds dbkit.TenantModel for
// its tenant_id column and GetTenantID method -- a plain, non-key
// tenant_id column, the right shape because ID is an application-generated
// UUID (already globally unique on its own, so no composite
// (tenant_id, id) key is needed) -- with the tenant filter injected by
// dbkit's tenant-scoping plugin and by dbkit.Repository[caseRecord] itself,
// never written by hand.
//
// The row carries no status column: a "case status workflow" (open/closed/
// archived vocabulary and the transitions between states) is deliberately
// absent -- nothing in this app would read or write it, and a status
// vocabulary is a case-list-interaction decision (see the package doc
// comment's "Known limitations" section). The row is created once and
// read; nothing in this package updates it.
type caseRecord struct {
	// ID is an application-generated UUID (see service.go's Create), never
	// a database-generated one -- the backend coding standard (§5) requires
	// primary keys to be generated in the application, and forbids
	// PostgreSQL-only generators such as gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped) -- see the doc comment above for why
	// caseRecord embeds it instead of declaring TenantID directly.
	dbkit.TenantModel

	// PatientName is the patient's display name as the clinic entered it at
	// intake -- the case list and detail render it, and nothing else in
	// this domain reads it. Required, length-bounded in the application
	// (service.go's patientNameMaxLength; SQLite does not enforce a VARCHAR
	// length limit, so the check lives here, the same reasoning notes'
	// handler.go gives for its own maxTextLength).
	PatientName string `gorm:"column:patient_name;size:200;not null"`

	// PatientRef is the optional clinic-given identifier for the patient
	// (a chart number, an internal reference) -- display-only, deliberately
	// no PHI semantics beyond what a demo intake form needs (see the
	// package doc comment's "Known limitations" section). Empty is the
	// "the
	// clinic gave none" sentinel, the same NOT NULL DEFAULT '' convention
	// notes.Note's CreatorUserID column follows.
	PatientRef string `gorm:"column:patient_ref;size:64;not null;default:''"`

	// CreatorUserID is the user id of the clinic staff member who created
	// the case, attributed by the host's SubjectResolver seam at create
	// time (cmd/server's wireCasesRoutes resolves it before the body is
	// even read) -- never by the request itself, and never written after
	// Create. It is the row's recorded attribution: the case list is
	// clinic-wide (Service.List enumerates every case of the tenant,
	// never one creator's subset -- see the package doc comment's
	// "Shape decision" section for the clinic-wide decision), so this
	// column is history, not a list key. The column carries a NOT NULL
	// DEFAULT '' so pre-existing rows and any caller that creates a case
	// without a resolvable creator (tests, seed code) stay valid: an
	// empty CreatorUserID is the "no attributable creator" sentinel, and
	// no list is keyed on it.
	CreatorUserID string `gorm:"column:creator_user_id;size:64;not null;default:''"`

	// CreatedAt is populated by gorm's autoCreateTime on Create -- never
	// written by application code, and never NOW() in a migration (backend
	// coding standard §5's dual-dialect rule: SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins caseRecord to casesTable, so it does not depend on GORM's
// pluralization of the (unexported) type name.
func (caseRecord) TableName() string { return casesTable }

// casePhotoRecord is one photo of one case: a reference to an existing
// go/storage photo object the case's patient record groups with its other
// photos. The bytes never live here -- the object id is a reference only,
// recorded without any cross-module foreign key, and go/storage's own
// object access controls remain the protection when the bytes are opened
// (see the package doc comment's "Known limitations" section on what
// create does not verify).
//
// The row is tenant data, shaped exactly like caseRecord above (TenantModel
// embed, tenant filter injected, never hand-written). Its identity is its
// own application-generated UUID -- NOT the object id -- so that attaching
// is scoped per tenant: two tenants may each reference the same object id
// (each row's (tenant_id, object_id) uniqueness is what the
// uq_case_photos_tenant_object index enforces), while within one tenant an
// object can belong to at most one case -- the unique index backs the
// Service's pre-flight "already attached" refusal and keeps the
// before/after pairing unambiguous (a photo's per-photo simulation list
// shows in exactly one case of the tenant).
//
// Position is the photo's deterministic place in the case's attachment
// order, taken from the create request's photo list (0-based). The detail
// read orders by it; an add-photo operation, once one exists, appends at
// max+1 -- which is why the column exists rather than relying on
// timestamp luck.
type casePhotoRecord struct {
	// ID is an application-generated UUID (see service.go's Create), the
	// row's own identity -- the same application-generated-primary-key rule
	// caseRecord's ID doc comment states. It is what dbkit.Repository[T]'s
	// FindByID and friends key on.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID method
	// (satisfying dbkit.TenantScoped) -- see caseRecord's doc comment for
	// why both models embed it.
	dbkit.TenantModel

	// CaseID is the owning case's id (caseRecord.ID). No foreign key: the
	// case layer records references only, per the no-cross-module-FK rule
	// and the same intra-app convention notes and smilesim follow (their
	// rows likewise carry ids, never gorm associations).
	CaseID string `gorm:"column:case_id;size:36;not null"`

	// ObjectID is the referenced go/storage photo object's id -- the value
	// that feeds smilesim's per-photo enumeration (the before/after
	// pairing's "after" candidates) and, when bytes are needed,
	// go/storage's own open paths.
	ObjectID string `gorm:"column:object_id;size:64;not null"`

	// Position is the 0-based attachment-order index within the case (see
	// the doc comment above).
	Position int `gorm:"column:position;not null"`

	// CreatedAt is populated by gorm's autoCreateTime on Create.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

// TableName pins casePhotoRecord to casePhotosTable, so it does not depend
// on GORM's pluralization of the (unexported) type name.
func (casePhotoRecord) TableName() string { return casePhotosTable }

// compile-time checks that both models satisfy dbkit.TenantScoped.
var (
	_ dbkit.TenantScoped = caseRecord{}
	_ dbkit.TenantScoped = casePhotoRecord{}
)
