package testutil

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// FakeTenantLister is a compliance.TenantLister returning a fixed slice
// of tenants, for tests of RetentionService.SweepAllTenants.
type FakeTenantLister struct {
	Tenants []pkgcore.TenantID
	// Err, when non-nil, is returned by ListTenants instead of Tenants.
	Err error
}

// ListTenants implements compliance.TenantLister.
func (l FakeTenantLister) ListTenants(_ context.Context) ([]pkgcore.TenantID, error) {
	if l.Err != nil {
		return nil, l.Err
	}
	return l.Tenants, nil
}
