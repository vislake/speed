package integration

import (
	"context"
	"errors"
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

// --- apiKeyHashIndex / createWithHashIndex / tenantForHash / byHash -----

// newTestAPIKeyRow builds an unsaved APIKey with a freshly generated raw
// key/hash pair, returning both the row and the raw key that hashes to it.
func newTestAPIKeyRow(t *testing.T, id string) (*APIKey, string) {
	t.Helper()
	raw, prefix, hash, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}
	return &APIKey{
		ID:        id,
		Prefix:    prefix,
		Hash:      hash,
		Scopes:    scopesJSON(nil),
		CreatedBy: "user-1",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, raw
}

// TestAPIKeyHashIndex_AssertNotTenantScoped proves apiKeyHashIndex is
// genuinely platform data, not tenant data: a row written with no tenant in
// context is visible under an arbitrary one, and a row written under one
// tenant is visible under another -- the exact opposite of AssertIsolated,
// and the correct property for a table dbkit's tenant-scope GORM plugin must
// never engage on (model.go's apiKeyHashIndex doc comment explains why).
func TestAPIKeyHashIndex_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	i := 0
	tenancytest.AssertNotTenantScoped(t, db, apiKeyHashIndex{},
		func(tx *gorm.DB) error {
			i++
			return tx.Create(&apiKeyHashIndex{
				Hash:     fmt.Sprintf("probe-hash-%d", i),
				TenantID: "irrelevant",
			}).Error
		},
		func(tx *gorm.DB) (int64, error) {
			var n int64
			err := tx.Model(&apiKeyHashIndex{}).Count(&n).Error
			return n, err
		},
	)
}

// TestAPIKeyRepository_CreateWithHashIndex_WritesBothRowsUnderTheSameTenant
// proves the transactional write: both the APIKey row (found through the
// ordinary tenant-scoped byHash) and the apiKeyHashIndex row (found through
// the narrow, tenant-less tenantForHash) land from one createWithHashIndex
// call, agreeing on the same tenant.
func TestAPIKeyRepository_CreateWithHashIndex_WritesBothRowsUnderTheSameTenant(t *testing.T) {
	repo := NewAPIKeyRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	row, _ := newTestAPIKeyRow(t, "key-1")
	if err := repo.createWithHashIndex(ctx, row); err != nil {
		t.Fatalf("createWithHashIndex: %v", err)
	}

	got, err := repo.byHash(ctx, row.Hash)
	if err != nil {
		t.Fatalf("byHash: %v", err)
	}
	if got.ID != row.ID {
		t.Errorf("byHash = %+v, want ID %q", got, row.ID)
	}

	tenant, err := repo.tenantForHash(context.Background(), row.Hash)
	if err != nil {
		t.Fatalf("tenantForHash: %v", err)
	}
	if tenant != "tenant-a" {
		t.Errorf("tenantForHash = %q, want %q", tenant, "tenant-a")
	}
}

// TestAPIKeyRepository_TenantForHash_UnknownHashReportsAuthenticationFailed
// mirrors sharing's identical TestShareRepository_TenantForTokenHash_
// UnknownHashReportsNotAccessible.
func TestAPIKeyRepository_TenantForHash_UnknownHashReportsAuthenticationFailed(t *testing.T) {
	repo := NewAPIKeyRepository(newTestDB(t))
	if _, err := repo.tenantForHash(context.Background(), "does-not-exist"); !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("tenantForHash(unknown hash) error = %v, want ErrAuthenticationFailed", err)
	}
}

// TestAPIKeyRepository_TenantForHash_NeedsNoTenantInContext is the direct
// proof of this method's whole reason to exist: a genuinely anonymous
// caller -- context.Background(), nothing attached to it at all -- can still
// resolve a tenant from a hash. A regression that silently made this method
// call pkgcore.MustTenantFromContext (or route through
// dbkit.WithTenantSession, which does the same) would fail this test with
// pkgcore.ErrNoTenant instead of a real answer.
func TestAPIKeyRepository_TenantForHash_NeedsNoTenantInContext(t *testing.T) {
	repo := NewAPIKeyRepository(newTestDB(t))
	row, _ := newTestAPIKeyRow(t, "key-1")
	if err := repo.createWithHashIndex(pkgcore.WithTenant(context.Background(), "tenant-b"), row); err != nil {
		t.Fatalf("createWithHashIndex: %v", err)
	}

	tenant, err := repo.tenantForHash(context.Background(), row.Hash)
	if err != nil {
		t.Fatalf("tenantForHash(no tenant in ctx) error = %v, want success", err)
	}
	if tenant != "tenant-b" {
		t.Errorf("tenantForHash = %q, want %q", tenant, "tenant-b")
	}
}

// TestAPIKeyRepository_ByHash_CrossTenantReportsAuthenticationFailed proves
// byHash's own tenant scoping: a key created under tenant-a is not found by
// byHash under tenant-b's context, even though the hash itself is genuine
// and tenantForHash would correctly resolve it to tenant-a.
func TestAPIKeyRepository_ByHash_CrossTenantReportsAuthenticationFailed(t *testing.T) {
	repo := NewAPIKeyRepository(newTestDB(t))
	row, _ := newTestAPIKeyRow(t, "key-1")
	if err := repo.createWithHashIndex(pkgcore.WithTenant(context.Background(), "tenant-a"), row); err != nil {
		t.Fatalf("createWithHashIndex: %v", err)
	}

	wrongTenantCtx := pkgcore.WithTenant(context.Background(), "tenant-b")
	if _, err := repo.byHash(wrongTenantCtx, row.Hash); !errors.Is(err, ErrAuthenticationFailed) {
		t.Errorf("byHash(cross-tenant) error = %v, want ErrAuthenticationFailed", err)
	}
}
