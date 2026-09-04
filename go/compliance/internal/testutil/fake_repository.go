package testutil

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// FakeRepository is a minimal dbkit.Repository[FakeNote]-backed
// repository, mirroring go/storage's ObjectRepository shape: it embeds
// the generic Repository[T] for the ordinary CRUD surface, and keeps its
// own db handle only to compose the two custom listing queries below
// through dbkit.WithTenantSession -- never .Table/.Model/.Raw -- exactly
// the pattern go/storage/repository.go's listStateRows and
// listExpiredCompleted already establish for the identical need
// (dbkit.Repository[T].List cannot express "soft-deleted rows past a
// cutoff", since it is scoped to non-deleted rows by the soft-delete
// auto-scope plugin).
type FakeRepository struct {
	*dbkit.Repository[FakeNote]
	db *gorm.DB
}

// NewFakeRepository returns a FakeRepository backed by db, which must
// already carry FakeNote's table (see NewDB).
func NewFakeRepository(db *gorm.DB) *FakeRepository {
	return &FakeRepository{Repository: dbkit.NewRepository[FakeNote](db), db: db}
}

// DB returns the underlying *gorm.DB, for a calling test's own direct
// assertions (backdating a timestamp, checking a row is gone including
// soft-deleted) that go beyond what Repository[FakeNote]'s own methods
// expose. Exported deliberately: this package exists to let another
// module's tests exercise the pkgcore.RetentionParticipant contract
// end to end, and that requires more low-level visibility into the
// fixture table than compliance's own production code ever needs.
func (r *FakeRepository) DB() *gorm.DB { return r.db }

// listSoftDeletedBefore returns every soft-deleted FakeNote of ctx's
// tenant whose DeletedAt is at or before cutoff -- the query a Sweep
// callback runs to find what a retention sweep should reap.
func (r *FakeRepository) listSoftDeletedBefore(ctx context.Context, cutoff time.Time) ([]FakeNote, error) {
	var rows []FakeNote
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Unscoped().
			Where("deleted_at IS NOT NULL AND deleted_at <= ?", cutoff).
			Find(&rows).Error
	})
	return rows, err
}

// listBySubject returns every FakeNote (soft-deleted or not) of ctx's
// tenant whose SubjectID matches subjectID -- the query an Erase callback
// runs: a right-to-erasure request bypasses the retention window
// entirely, so a row need not be soft-deleted first to be erased.
func (r *FakeRepository) listBySubject(ctx context.Context, subjectID string) ([]FakeNote, error) {
	var rows []FakeNote
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("subject_id = ?", subjectID).Find(&rows).Error
	})
	return rows, err
}

// NewParticipant returns a pkgcore.RetentionParticipant named name,
// backed by repo, implementing all three callbacks over repo's own
// dbkit.Repository[FakeNote] embedding: Sweep hard-deletes every soft-
// deleted row at or before the given cutoff, Erase hard-deletes every row
// belonging to the given subject (soft-deleted or not), and Export
// returns every live row belonging to the given tenant. This is this
// round's whole proof that the pkgcore.RetentionParticipant contract
// compiles and works end to end: every write below is a plain
// repo.HardDelete call, never compliance code -- or this fixture's own
// participant wrapper -- writing to compliance_test_fake_notes directly.
func NewParticipant(name string, repo *FakeRepository) pkgcore.RetentionParticipant {
	return pkgcore.RetentionParticipant{
		Name: name,
		Sweep: func(ctx context.Context, _ pkgcore.TenantID, cutoff time.Time) (int, error) {
			rows, err := repo.listSoftDeletedBefore(ctx, cutoff)
			if err != nil {
				return 0, err
			}
			reaped := 0
			for _, row := range rows {
				if err := repo.HardDelete(ctx, row.ID); err != nil {
					return reaped, err
				}
				reaped++
			}
			return reaped, nil
		},
		Erase: func(ctx context.Context, subject pkgcore.SubjectRef) (int, error) {
			rows, err := repo.listBySubject(ctx, subject.SubjectID)
			if err != nil {
				return 0, err
			}
			erased := 0
			for _, row := range rows {
				if err := repo.HardDelete(ctx, row.ID); err != nil {
					return erased, err
				}
				erased++
			}
			return erased, nil
		},
		Export: func(ctx context.Context, _ pkgcore.TenantID) (any, error) {
			return repo.List(ctx)
		},
	}
}
