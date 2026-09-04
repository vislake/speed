package admin

import (
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestTenant_IsNotTenantScoped proves Tenant is genuinely platform data:
// dbkit's tenant-isolation plugin must not affect it at all, regardless of
// what tenant (if any) is in the calling context -- see D3's own doc
// comment in tenant_service.go for why this table must never be scoped to
// one tenant.
func TestTenant_IsNotTenantScoped(t *testing.T) {
	db := testutil.NewDB(t)
	seq := 0

	createFn := func(db *gorm.DB) error {
		seq++
		return db.Create(&Tenant{
			TenantID:  newTestID(t, "tenant", seq),
			Status:    TenantStatusActive,
			CreatedAt: time.Now().UTC(),
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Model(&Tenant{}).Count(&n).Error
		return n, err
	}
	tenancytest.AssertNotTenantScoped(t, db, Tenant{}, createFn, findFn)
}

// TestImpersonationGrant_IsNotTenantScoped proves ImpersonationGrant is
// genuinely platform data: TargetTenantID is a DATA column naming which
// tenant a grant is scoped to, never a filter dbkit's isolation plugin
// should apply to the table itself -- see model.go's own doc comment.
func TestImpersonationGrant_IsNotTenantScoped(t *testing.T) {
	db := testutil.NewDB(t)
	seq := 0

	createFn := func(db *gorm.DB) error {
		seq++
		now := time.Now().UTC()
		return db.Create(&ImpersonationGrant{
			ID:             newTestID(t, "grant", seq),
			AdminUserID:    "admin-1",
			TargetUserID:   "user-1",
			TargetTenantID: "tenant-1",
			Reason:         "support ticket",
			CreatedAt:      now,
			ExpiresAt:      now.Add(defaultGrantTTL),
		}).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var n int64
		err := db.Model(&ImpersonationGrant{}).Count(&n).Error
		return n, err
	}
	tenancytest.AssertNotTenantScoped(t, db, ImpersonationGrant{}, createFn, findFn)
}

// TestImpersonationGrant_Active pins Active's fail-closed semantics: ended
// beats not-yet-expired, and expiry alone (with no explicit end) is also
// inactive.
func TestImpersonationGrant_Active(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("neither ended nor expired is active", func(t *testing.T) {
		g := ImpersonationGrant{ExpiresAt: now.Add(time.Hour)}
		if !g.Active(now) {
			t.Fatal("Active() = false, want true")
		}
	})
	t.Run("expired with no explicit end is inactive", func(t *testing.T) {
		g := ImpersonationGrant{ExpiresAt: now.Add(-time.Minute)}
		if g.Active(now) {
			t.Fatal("Active() = true, want false (expired)")
		}
	})
	t.Run("explicitly ended before its natural expiry is inactive", func(t *testing.T) {
		endedAt := now.Add(-time.Minute)
		g := ImpersonationGrant{ExpiresAt: now.Add(time.Hour), EndedAt: &endedAt}
		if g.Active(now) {
			t.Fatal("Active() = true, want false (ended)")
		}
	})
}

// newTestID returns a small, distinct id for a test's own throwaway rows.
func newTestID(t *testing.T, prefix string, seq int) string {
	t.Helper()
	return prefix + "-" + strconv.Itoa(seq)
}
