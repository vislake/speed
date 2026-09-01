package testutil

import "github.com/vislake/speed/go/pkgcore"

// Widget is dbkit's minimal tenant-scoped fixture model. It exists solely
// to exercise dbkit's tenant-isolation GORM plugin, Repository[T], and
// migration aggregation in tests; it carries no meaning outside them.
//
// Its schema lives in migrations/{postgres,sqlite}/0001_create_widgets.sql,
// kept portable between both dialects. Its primary key is (tenant_id, id)
// with tenant_id leftmost, per the multi-tenant isolation standard every
// tenant-scoped table follows.
type Widget struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	Name     string `gorm:"size:255;not null"`
	Value    int    `gorm:"not null;default:0"`
}

// GetTenantID returns the tenant Widget belongs to. It gives Widget the
// GetTenantID() pkgcore.TenantID method that dbkit's tenant-scoping
// contract requires of every tenant-scoped model, so Widget can stand in
// for a real one wherever that contract is exercised in tests.
func (w Widget) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(w.TenantID)
}
