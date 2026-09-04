package pki

import (
	"context"
	"errors"

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

// ListVerifiableByPurpose returns every row for purpose whose Status is not
// SigningKeyStatusRevoked, in no particular order. This is the query
// Service.VerificationKeys reads: "all still-verifiable keys", which today
// (round 1, only the pending->active transition wired) means every key ever
// created for purpose except one a later round revoked.
func (r *SigningKeyRepository) ListVerifiableByPurpose(ctx context.Context, purpose string) ([]SigningKey, error) {
	var keys []SigningKey
	err := r.db.WithContext(ctx).
		Where("purpose = ? AND status <> ?", purpose, SigningKeyStatusRevoked).
		Find(&keys).Error
	return keys, err
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
