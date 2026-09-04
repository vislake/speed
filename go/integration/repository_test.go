package integration

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/integration/internal/testutil"
	"github.com/vislake/speed/go/integration/migrations"
)

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// TestAPIKeyRepository_AssertIsolated runs the mandatory tenant-isolation
// suite (root CLAUDE.md's "every new repository must run
// tenancytest.AssertIsolated") against integration_api_keys: APIKey is
// tenant data, so a key created under one tenant must never be readable,
// updatable or listable from another.
func TestAPIKeyRepository_AssertIsolated(t *testing.T) {
	repo := NewAPIKeyRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *APIKey {
		n++
		_, prefix, hash, err := newAPIKeyToken()
		if err != nil {
			t.Fatalf("newAPIKeyToken: %v", err)
		}
		return &APIKey{
			ID:        fmt.Sprintf("key-%d", n),
			Prefix:    prefix,
			Hash:      hash,
			Scopes:    scopesJSON([]string{"notes:read"}),
			CreatedBy: "user-1",
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}
	})
}
