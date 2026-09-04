package sharing

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// ShareRepository is the tenant-scoped data-access type for Share. It
// embeds dbkit.Repository[Share] for the ordinary CRUD surface (Create,
// FindByID, Update, ...) and adds the extra query shapes Service needs on
// top, composed on the *gorm.DB the isolation plugin protects, inside
// dbkit.WithTenantSession -- no hand-written tenant predicate, no
// db.Table / db.Model / db.Raw, the identical construction rule
// go/org's InvitationRepository documents for its own extra methods.
type ShareRepository struct {
	*dbkit.Repository[Share]

	db *gorm.DB
}

// NewShareRepository returns a ShareRepository backed by db.
func NewShareRepository(db *gorm.DB) *ShareRepository {
	return &ShareRepository{Repository: dbkit.NewRepository[Share](db), db: db}
}

// byTokenHash returns the caller tenant's share whose stored hash is hash,
// or ErrNotAccessible.
//
// The tenant scoping is the security property that matters here, mirroring
// org.InvitationRepository.byTokenHash's identical reasoning: a token
// minted for another tenant simply does not match under this tenant's
// scope, so nothing about its existence can be learned from this call
// alone. Service.Access reports the plain ErrNotAccessible (rather than a
// code naming "not found") specifically so a caller cannot distinguish this
// outcome from a revoked, expired or view-exhausted share.
func (r *ShareRepository) byTokenHash(ctx context.Context, hash string) (*Share, error) {
	var share Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("token_hash = ?", hash).First(&share).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrNotAccessible
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &share, nil
}

// tryRecordView attempts to record one granted access against share's own
// expectation of the row's current state: it succeeds only if, at the
// moment the UPDATE actually runs, the row is still exactly as share
// describes it -- same view count, still not revoked, still not expired as
// of now, and (if MaxViews is set) still under it -- and reports whether
// this call is the one that recorded the view.
//
// This is a compare-and-swap guard expressed entirely through a WHERE
// clause and a struct passed to Updates, deliberately NOT a raw SQL
// increment (view_count = view_count + 1) reached through .Model(...): the
// three bypass entry points named by the discipline table
// (tools/semgrep_rules/raw-gorm-bypass.yml) are .Table/.Model/.Raw, and
// this codebase's own established pattern for a guarded write inside
// dbkit.WithTenantSession -- org.InvitationRepository.acceptIfPending is
// the precedent -- passes a struct to Where/Updates so the table resolves
// from the struct's own TableName rather than an explicit .Model() call.
//
// Service.Access retries on a lost race (this call returning won == false
// while the row it re-reads is still live) rather than treating ordinary
// concurrency as "not accessible" -- see its own doc comment.
func (r *ShareRepository) tryRecordView(ctx context.Context, share *Share, now time.Time) (won bool, err error) {
	var rowsAffected int64
	dbErr := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", share.ID).
			Where("view_count = ?", share.ViewCount).
			Where("revoked_at IS NULL").
			Where("expires_at > ?", now).
			Where("max_views IS NULL OR view_count < max_views").
			Updates(&Share{ViewCount: share.ViewCount + 1})
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if dbErr != nil {
		return false, ErrInternal.WithCause(dbErr)
	}
	return rowsAffected == 1, nil
}

// listExpiredOrExhausted returns every live (not yet revoked) row of the
// caller tenant whose ExpiresAt has passed as of now, or whose MaxViews has
// been reached -- the expiry sweep's own listing (cleanup.go).
func (r *ShareRepository) listExpiredOrExhausted(ctx context.Context, now time.Time) ([]Share, error) {
	var out []Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("revoked_at IS NULL").
			Where("expires_at <= ? OR (max_views IS NOT NULL AND view_count >= max_views)", now).
			Order("id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// AccessLogRepository is the tenant-scoped data-access type for
// AccessLogEntry. It embeds dbkit.Repository[AccessLogEntry] for Create --
// the only write this module ever performs against the table, per the
// type's own append-only doc comment -- and adds the one read shape
// Service.ListAccessLog needs.
type AccessLogRepository struct {
	*dbkit.Repository[AccessLogEntry]

	db *gorm.DB
}

// NewAccessLogRepository returns an AccessLogRepository backed by db.
func NewAccessLogRepository(db *gorm.DB) *AccessLogRepository {
	return &AccessLogRepository{Repository: dbkit.NewRepository[AccessLogEntry](db), db: db}
}

// listByShare returns every access log row of the caller tenant recorded
// against shareID, newest first and then by id so the order is total and
// stable -- the exact listing Service.ListAccessLog serves to a resource
// owner.
func (r *AccessLogRepository) listByShare(ctx context.Context, shareID string) ([]AccessLogEntry, error) {
	var out []AccessLogEntry
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("share_id = ?", shareID).
			Order("occurred_at DESC, id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}
