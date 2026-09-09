package notes

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is notes' tenant-scoped data-access type. It embeds
// dbkit.Repository[Note] instead of holding a *gorm.DB directly (the
// multi-tenant isolation discipline) --
// Create, FindByID, Update, Delete, List and HardDelete are all promoted from
// the embedded base unchanged. It exists as notes' own named type, rather
// than every caller using *dbkit.Repository[Note] directly, so a query
// specific to notes that Repository[T]'s deliberately minimal surface cannot
// express has somewhere to live
// without changing any caller's import.
type Repository struct {
	*dbkit.Repository[Note]

	// db is the *gorm.DB the embedded Repository was built over, kept here
	// for WithTenantSession calls below. It is the very connection the
	// embedded Repository already uses -- never a second pool -- exactly as
	// go/compliance's internal/testutil FakeRepository holds the db it was
	// built over for its own tenant-session queries (the precedent this
	// shape follows).
	db *gorm.DB
}

// NewRepository returns a Repository backed by db. db is expected to come
// from dbkit.Open, already migrated with this module's own Migrations()
// (see Module.Migrations and internal/app's wiring for the exact sequence) --
// see dbkit.Repository's own doc comment for why db is expected to come
// from Open specifically.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[Note](db), db: db}
}

// listSoftDeletedBefore returns every note belonging to ctx's tenant that
// was soft-deleted (deleted_at set) at or before cutoff -- the candidate
// set a retention sweep is asked to reap. It is notes' own contribution to
// the retention mechanism (see retention_participant.go): the list is
// gathered here because Repository[T] deliberately exposes no
// "find soft-deleted rows by deletion time" method, and the physical
// removal itself is deliberately left to the caller, which must hard-delete
// each returned note one row at a time through the promoted
// Repository.HardDelete (never a bulk statement of its own -- the same
// one-row-at-a-time rule go/compliance's own retention tests pin).
//
// The query runs inside dbkit.WithTenantSession, exactly as
// go/compliance's FakeRepository.listSoftDeletedBefore precedent does: the
// tenant-scope plugin injects the tenant half of the WHERE clause from the
// ctx tenant (see go/dbkit/tenant_scope.go), so no tenant_id filter is
// written here by hand, and the tenant cannot be widened by an accidental
// omission. Unscoped() lifts only the soft-delete auto-scope's
// "deleted_at IS NULL" filter -- the whole point is to see soft-deleted
// rows -- never the tenant plugin's filter.
func (r *Repository) listSoftDeletedBefore(ctx context.Context, cutoff time.Time) ([]Note, error) {
	var notes []Note
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("deleted_at IS NOT NULL AND deleted_at <= ?", cutoff).Find(&notes).Error
	})
	return notes, err
}

// listByCreator returns every note belonging to ctx's tenant whose
// CreatorUserID is creatorUserID, live or soft-deleted -- the candidate set
// a right-to-erasure request targets (see retention_participant.go). The
// same WithTenantSession + plugin-injected tenant filter discipline as
// listSoftDeletedBefore applies; Unscoped lifts only the soft-delete
// auto-scope, so a soft-deleted note is as erasable as a live one, and the
// physical removal is again the caller's one-row-at-a-time HardDelete job.
func (r *Repository) listByCreator(ctx context.Context, creatorUserID string) ([]Note, error) {
	var notes []Note
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("creator_user_id = ?", creatorUserID).Find(&notes).Error
	})
	return notes, err
}
