package aigateway

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// credentialStore is the ai_gateway_credentials table accessor. It is
// deliberately a thin row accessor over a plain *gorm.DB -- no
// dbkit.Repository[T], mirroring go/config's own store.go, because
// credentialRow is platform data, not tenant data (see model.go's own doc
// comment). Scope and system-context rules live in CredentialService above
// it; the store itself does not know what a CredentialScope means beyond
// the column it maps to.
type credentialStore struct {
	db *gorm.DB
}

// get returns the row for one exact (provider, scope, tenantID) triple, or
// (nil, nil) when no such row exists. tenantID is empty for
// CredentialScopeSystem rows, the empty-string sentinel those rows carry in
// the tenant_id column.
func (s *credentialStore) get(ctx context.Context, provider string, scope CredentialScope, tenantID string) (*credentialRow, error) {
	var out credentialRow
	err := s.db.WithContext(ctx).
		Where("provider = ? AND scope = ? AND tenant_id = ?", provider, string(scope), tenantID).
		First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// put inserts or updates the row for one exact (provider, scope, tenantID)
// triple, using the row's own primary key as the upsert conflict target --
// the identical dialect-neutral clause config's own store.put uses, so both
// supported dialects (SQLite, PostgreSQL) stay on one code path.
func (s *credentialStore) put(ctx context.Context, r credentialRow) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "provider"},
				{Name: "scope"},
				{Name: "tenant_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{"api_key", "base_url", "updated_at"}),
		}).
		Create(&r).Error
}
