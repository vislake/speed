package config

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// store is the configs table accessor. It is deliberately a thin row
// accessor over a plain *gorm.DB -- no dbkit.Repository[T], because row is
// platform data, not tenant data, and Repository[T] requires TenantScoped
// (see model.go's own doc comment for the full reasoning). Scope and
// system-context rules live in the Service above it; the store itself does
// not know what a Scope is beyond the column it maps to, which keeps the
// guard in exactly one place.
type store struct {
	db *gorm.DB
}

// get returns the row for one exact (key, scope, tenantID) triple, or
// (nil, nil) when no such row exists. tenantID is empty for ScopeSystem
// rows, the empty-string sentinel those rows carry in the tenant_id
// column.
func (s *store) get(ctx context.Context, scope Scope, tenantID string, key string) (*row, error) {
	var out row
	err := s.db.WithContext(ctx).
		Where("scope = ? AND tenant_id = ? AND key = ?", string(scope), tenantID, key).
		First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// put inserts or updates the row for one exact (key, scope, tenantID)
// triple, using the row's own primary key as the upsert conflict target.
// The dialect-neutral clause keeps the two deployment dialects (SQLite,
// PostgreSQL) on one code path; both support INSERT ... ON CONFLICT.
func (s *store) put(ctx context.Context, r row) error {
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "key"},
				{Name: "scope"},
				{Name: "tenant_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_by", "updated_at"}),
		}).
		Create(&r).Error
}

// changedSince returns every row whose updated_at is not older than since.
// It is the poller's query (see (*Service).Refresh): rows are returned
// whole -- including tenant_id -- so the poller can invalidate exactly the
// cache entries the changed rows belong to without decrypting anything
// (Sensitive values stay sealed in the value column; the poller only needs
// the row's address, never its content).
//
// The comparison is inclusive (>=) on purpose. The poller's watermark
// advances to the newest updated_at it has seen; under a strict ">", a row
// written at the exact same clock instant as the watermark -- after the
// poll's query ran but before the watermark advanced -- would sit at the
// boundary forever, unreachable by every later poll. The inclusive form
// re-reads the boundary rows on every sweep; they are few (rows sharing
// one timestamp), the invalidation is idempotent, and the hole in the
// anti-loss net closes. What remains is documented in AGENTS.md: the
// watermark assumes writers' clocks are not behind the reader's, a
// multi-writer clock-skew caveat the event bus itself is not subject to.
func (s *store) changedSince(ctx context.Context, since time.Time) ([]row, error) {
	var out []row
	err := s.db.WithContext(ctx).
		Where("updated_at >= ?", since).
		Order("updated_at").
		Find(&out).Error
	return out, err
}
