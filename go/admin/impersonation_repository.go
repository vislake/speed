package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ImpersonationRepository reads and writes admin_impersonation_grants.
//
// Like TenantRepository, it holds a plain *gorm.DB rather than embedding
// dbkit.Repository[T]: this table is platform data (it names a target
// tenant as a column, not as its own scope) and must not implement
// dbkit.TenantScoped. The same two rules apply: no .Table/.Model/.Raw, no
// hand-written tenant filter.
type ImpersonationRepository struct {
	db *gorm.DB
}

// NewImpersonationRepository binds db.
func NewImpersonationRepository(db *gorm.DB) *ImpersonationRepository {
	return &ImpersonationRepository{db: db}
}

// Create inserts grant, generating its ID when empty.
func (r *ImpersonationRepository) Create(ctx context.Context, grant *ImpersonationGrant) error {
	if grant.ID == "" {
		id, err := newGrantID()
		if err != nil {
			return err
		}
		grant.ID = id
	}
	return r.db.WithContext(ctx).Create(grant).Error
}

// Get returns the grant named by id, or ErrGrantNotFound.
func (r *ImpersonationRepository) Get(ctx context.Context, id string) (*ImpersonationGrant, error) {
	var g ImpersonationGrant
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&g).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrGrantNotFound.WithParam("id", id)
		}
		return nil, err
	}
	return &g, nil
}

// Save persists every field of grant, including EndedAt/EndedBy.
func (r *ImpersonationRepository) Save(ctx context.Context, grant *ImpersonationGrant) error {
	return r.db.WithContext(ctx).Save(grant).Error
}

// ListActive returns every grant that is Active at instant now -- the
// currently-effective grants an operator can inspect through
// GET /api/v1/admin/impersonation (D5's self-audit listing).
func (r *ImpersonationRepository) ListActive(ctx context.Context, now time.Time) ([]ImpersonationGrant, error) {
	var rows []ImpersonationGrant
	err := r.db.WithContext(ctx).
		Where("ended_at IS NULL AND expires_at > ?", now).
		Order("created_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// grantIDBytes is the random byte length ImpersonationGrant.ID is derived
// from -- 16 bytes (128 bits) hex-encoded to a 32-character string,
// comfortably inside the model's size:36 column.
const grantIDBytes = 16

// newGrantID returns a fresh, unguessable grant id: 16 random bytes
// hex-encoded to a 32-character string, comfortably inside the model's
// size:36 column and with 128 bits of entropy -- the credential itself
// (docs/internal/23-admin.md section 5's description of the grant id as
// the credential itself, randomly generated and unguessable),
// so it must never be derived from AdminUserID, TargetUserID or any other
// predictable input.
func newGrantID() (string, error) {
	buf := make([]byte, grantIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
