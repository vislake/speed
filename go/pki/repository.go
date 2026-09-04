package pki

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// SigningKeyRepository is the plain, non-tenant-scoped accessor for
// pki_signing_keys. SigningKey is platform data (see its own doc comment),
// so this wraps a bare *gorm.DB rather than embedding dbkit.Repository[T] --
// dbkit's own rule that identity/platform data must NOT implement
// TenantScoped means the generic base is not an option here.
type SigningKeyRepository struct {
	db *gorm.DB
}

// NewSigningKeyRepository returns a SigningKeyRepository over db. db is
// expected to come from dbkit.Open with this module's migrations applied.
func NewSigningKeyRepository(db *gorm.DB) *SigningKeyRepository {
	return &SigningKeyRepository{db: db}
}

// Create inserts key.
func (r *SigningKeyRepository) Create(ctx context.Context, key *SigningKey) error {
	return r.db.WithContext(ctx).Create(key).Error
}

// FindByID returns the row for id, or (nil, ErrKeyNotFound).
func (r *SigningKeyRepository) FindByID(ctx context.Context, id string) (*SigningKey, error) {
	var key SigningKey
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// FindActiveByPurpose returns the row currently in SigningKeyStatusActive
// for purpose, or (nil, ErrNoActiveKey) when none exists. The migration's
// partial unique index guarantees at most one such row can ever exist.
func (r *SigningKeyRepository) FindActiveByPurpose(ctx context.Context, purpose string) (*SigningKey, error) {
	var key SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status = ?", purpose, SigningKeyStatusActive).
		First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNoActiveKey
	}
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// ListVerifiableByPurpose returns every row for purpose whose Status is
// SigningKeyStatusPending, SigningKeyStatusActive or
// SigningKeyStatusRetiring, in no particular order. This is the query
// Service.VerificationKeys reads: "all still-verifiable keys". A pending
// key's public key is included per docs/internal/22-pki.md's "pending
// exists for the distributed race" section -- it is safe to publish for
// verification before it ever signs anything -- while a retired (or
// revoked) key is excluded: the whole point of the retiring overlap period
// is that it ends, and a key past it must stop being offered for
// verification, not merely stop being selected as ActiveSigner.
func (r *SigningKeyRepository) ListVerifiableByPurpose(ctx context.Context, purpose string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status IN ?", purpose, []string{
			SigningKeyStatusPending,
			SigningKeyStatusActive,
			SigningKeyStatusRetiring,
		}).
		Find(&keys).Error
	return keys, err
}

// Update persists every field of key, including its Status transition and
// the lifecycle timestamps (ActivatedAt/RetiringAt/RetiredAt) that go with
// it. Callers are expected to have loaded key from this repository (or
// built it fresh via Create) rather than constructing a partial row by
// hand, since Save writes every column.
func (r *SigningKeyRepository) Update(ctx context.Context, key *SigningKey) error {
	return r.db.WithContext(ctx).Save(key).Error
}

// PromoteToActive atomically promotes the pending key pendingID to
// SigningKeyStatusActive and, when previousActiveID is non-empty, demotes
// that other row from SigningKeyStatusActive to SigningKeyStatusRetiring in
// the SAME transaction -- the pending->active and active->retiring
// transitions docs/internal/22-pki.md's lifecycle diagram draws as one
// arrow (a new key's activation causing the old one's demotion into
// retiring), which is also why this
// module publishes no separate ".retiring" event: EventSigningKeyActivated
// communicates both halves of this one atomic write.
//
// The demotion runs FIRST, deliberately: uq_pki_signing_keys_active_purpose
// (migration 0001) is a partial unique index checked at each statement, not
// deferred to commit, so promoting pendingID to active before previousActiveID
// has left SigningKeyStatusActive would momentarily leave two active rows
// for the same purpose inside this very transaction and be refused by the
// database it is trying to write to. Demoting first briefly leaves the
// purpose with NO active row instead, which the index has nothing to say
// about.
//
// now is used for both RetiringAt (the demoted key) and ActivatedAt (the
// promoted key), so the two rows agree on exactly when the rotation
// happened.
func (r *SigningKeyRepository) PromoteToActive(ctx context.Context, pendingID string, previousActiveID string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if previousActiveID != "" {
			// A struct (not a map) as the Updates argument, with no .Model()
			// call: GORM infers the table from the struct type and, since
			// every other field is left at its zero value, writes only
			// Status and RetiringAt -- the same partial-update shape the
			// map form gave, without the raw-GORM-bypass entry point
			// (tools/semgrep_rules/raw-gorm-bypass.yml).
			if err := tx.Where("id = ? AND status = ?", previousActiveID, SigningKeyStatusActive).
				Updates(&SigningKey{
					Status:     SigningKeyStatusRetiring,
					RetiringAt: &now,
				}).Error; err != nil {
				return err
			}
		}
		res := tx.Where("id = ? AND status = ?", pendingID, SigningKeyStatusPending).
			Updates(&SigningKey{
				Status:      SigningKeyStatusActive,
				ActivatedAt: &now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrKeyNotFound
		}
		return nil
	})
}

// RetireRetiring marks the retiring key id as SigningKeyStatusRetired.
func (r *SigningKeyRepository) RetireRetiring(ctx context.Context, id string, now time.Time) error {
	// See PromoteToActive's comment: a struct Updates argument replaces the
	// map-plus-.Model() form to stay clear of the raw-GORM-bypass entry
	// point.
	res := r.db.WithContext(ctx).
		Where("id = ? AND status = ?", id, SigningKeyStatusRetiring).
		Updates(&SigningKey{
			Status:    SigningKeyStatusRetired,
			RetiredAt: &now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// ListByStatus returns every row in status, in no particular order. The
// expiry scan uses it for both the pending set (promotion candidates) and
// the retiring set (retirement candidates) -- both are expected to stay
// small (at most one row per purpose in the common case), so loading them
// in full is not a scan-scale concern the way ListActiveNearingExpiry's own
// not_after-indexed query is.
func (r *SigningKeyRepository) ListByStatus(ctx context.Context, status string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).Where("status = ?", status).Find(&keys).Error
	return keys, err
}

// ListActiveNearingExpiry returns every SigningKeyStatusActive row whose
// NotAfter is at or before before -- the expiry scan's staging query, read
// through idx_pki_signing_keys_not_after.
func (r *SigningKeyRepository) ListActiveNearingExpiry(ctx context.Context, before time.Time) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("status = ? AND not_after <= ?", SigningKeyStatusActive, before).
		Find(&keys).Error
	return keys, err
}

// Revoke marks the signing key id as SigningKeyStatusRevoked, recording
// revokedAt and reason -- the same guarded, status-checked update shape
// PromoteToActive/RetireRetiring use, so a concurrent revoke can never
// silently race a rotation and leave the row in an inconsistent state.
//
// Reports (true, nil) when this call performed the transition, and (false,
// nil) when id exists but was already SigningKeyStatusRevoked -- an
// idempotent no-op Service.RevokeSigningKey relies on to avoid publishing a
// second EventSigningKeyRevoked for an already-revoked key. Reports (false,
// ErrKeyNotFound) when id does not exist at all.
func (r *SigningKeyRepository) Revoke(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).
		Where("id = ? AND status != ?", id, SigningKeyStatusRevoked).
		Updates(&SigningKey{
			Status:           SigningKeyStatusRevoked,
			RevokedAt:        &now,
			RevocationReason: reason,
		})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	// RowsAffected == 0 is ambiguous on its own -- either id does not exist,
	// or it does but was already revoked. FindByID tells the two apart.
	if _, err := r.FindByID(ctx, id); err != nil {
		return false, err
	}
	return false, nil
}

// ExistsByPurposeAndStatus reports whether purpose already has a row in
// status. The expiry scan's staging step uses it to avoid staging a second
// pending key for a purpose that already has one in flight.
func (r *SigningKeyRepository) ExistsByPurposeAndStatus(ctx context.Context, purpose, status string) (bool, error) {
	// Count needs .Model() to know the table when nothing else in the chain
	// carries a struct type; a bounded Find into a typed slice gets the same
	// existence answer -- at most one row is ever fetched -- without the
	// raw-GORM-bypass entry point (tools/semgrep_rules/raw-gorm-bypass.yml).
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status = ?", purpose, status).
		Limit(1).
		Find(&keys).Error
	if err != nil {
		return false, err
	}
	count := int64(len(keys))
	return count > 0, nil
}

// AuthorityRepository is the plain, non-tenant-scoped accessor for
// pki_authorities. Authority is platform data, for the identical reason
// SigningKeyRepository above documents.
type AuthorityRepository struct {
	db *gorm.DB
}

// NewAuthorityRepository returns an AuthorityRepository over db.
func NewAuthorityRepository(db *gorm.DB) *AuthorityRepository {
	return &AuthorityRepository{db: db}
}

// Create inserts authority.
func (r *AuthorityRepository) Create(ctx context.Context, authority *Authority) error {
	return r.db.WithContext(ctx).Create(authority).Error
}

// FindByID returns the row for id, or (nil, ErrAuthorityNotFound).
func (r *AuthorityRepository) FindByID(ctx context.Context, id string) (*Authority, error) {
	var authority Authority
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&authority).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrAuthorityNotFound
	}
	if err != nil {
		return nil, err
	}
	return &authority, nil
}

// CertificateRepository is the tenant-scoped accessor for pki_certificates.
// Certificate is tenant data, so this embeds dbkit.Repository[Certificate]
// and inherits all three tenant-isolation layers, exactly like every other
// tenant-owned repository in this codebase.
type CertificateRepository struct {
	*dbkit.Repository[Certificate]
}

// NewCertificateRepository returns a CertificateRepository over db. db is
// expected to come from dbkit.Open (for isolation layer 1 underneath).
func NewCertificateRepository(db *gorm.DB) *CertificateRepository {
	return &CertificateRepository{Repository: dbkit.NewRepository[Certificate](db)}
}
