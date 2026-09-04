package sharing

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
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

// createWithTokenIndex inserts share and its shareTokenIndex row in one
// database transaction, so a share is never left reachable by its owner
// (an authenticated tenant caller) while being permanently unreachable by
// an anonymous visitor holding the same token -- the inconsistency a
// two-step, non-transactional write could otherwise leave behind
// indefinitely, since nothing else in this module ever repairs a missing
// index row. Service.Create calls this instead of the embedded
// Repository[Share].Create.
//
// The two writes share dbkit.WithTenantSession's single transaction rather
// than two separate calls: the tenant-scope GORM plugin still forces
// share's tenant_id on the first Create exactly as Repository[Share].Create
// itself relies on (it does not touch shareTokenIndex at all, since that
// type implements no dbkit.TenantScoped -- see its own doc comment), and a
// failure on either write rolls back both, never leaving one committed
// without the other.
func (r *ShareRepository) createWithTokenIndex(ctx context.Context, share *Share) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(share).Error; err != nil {
			return err
		}
		idx := &shareTokenIndex{TokenHash: share.TokenHash, TenantID: string(tenant)}
		return tx.Create(idx).Error
	})
}

// tenantForTokenHash resolves the tenant a token hash belongs to, with NO
// tenant predicate anywhere in the query -- the one deliberately narrow
// exception to this module's "every query is tenant-scoped" rule, and the
// mechanism AGENTS.md's "Tenant resolution for an unauthenticated viewer"
// section chose to close round 1's documented gap: a genuinely
// unauthenticated visitor holds no tenant claim, so nothing about their
// request can scope this lookup by tenant before it runs -- that is
// precisely the property this method exists to establish, not violate.
//
// This is not a second byTokenHash and not a general cross-tenant query
// capability: it reads shareTokenIndex, a table that was never tenant-
// scoped to begin with (see that type's own doc comment for the full data-
// domain reasoning), through an ordinary, unfiltered query -- dbkit's
// tenant-scope GORM plugin never engages here at all, because the plugin
// only ever acts on a model implementing dbkit.TenantScoped, and
// shareTokenIndex deliberately does not. Nothing here reaches for raw SQL,
// pkgcore.WithSystemContext, or any other escape hatch around a tenant-
// scoped query -- there is no tenant-scoped query to escape, because this
// method touches a different, narrower table than byTokenHash does. It
// returns a tenant id and nothing else: no ResourceRef, no ShareID, no
// Share row at all, so a caller cannot use this method to learn anything
// about a share beyond which tenant a token's hash belongs to.
//
// Service.AccessPublic is this method's only caller: it resolves the
// tenant here, attaches it to ctx with pkgcore.WithTenant, and re-enters
// the ordinary tenant-scoped Service.Access unchanged -- Access itself
// still performs its own byTokenHash lookup, its own password check, and
// its own outward-identical-answer handling exactly as it always has. An
// unrecognized hash here returns ErrNotAccessible, the same sentinel
// byTokenHash returns for an unrecognized hash under a known tenant, so a
// caller cannot distinguish "no such token anywhere" from "no such token
// in the tenant it otherwise resolved to" -- though AccessPublic's own doc
// comment records the one property this method does NOT hide: a genuinely
// unrecognized token costs one repository read here plus one burned
// password check, while a recognized-but-refused token costs one extra
// repository read (Access's own byTokenHash) on top of that -- a timing
// difference AGENTS.md's "The five mandatory rules" section's rule 5 was
// never written to cover, since it protects "which of these refusal
// reasons applied", not "does this token exist at all", which a valid
// token's own successful use already discloses to whoever holds it.
func (r *ShareRepository) tenantForTokenHash(ctx context.Context, hash string) (pkgcore.TenantID, error) {
	var idx shareTokenIndex
	err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&idx).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", ErrNotAccessible
	case err != nil:
		return "", ErrInternal.WithCause(err)
	}
	return pkgcore.TenantID(idx.TenantID), nil
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
