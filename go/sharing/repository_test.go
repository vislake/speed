package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/sharing/internal/testutil"
	"github.com/vislake/speed/go/sharing/migrations"
)

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

func newTestShare(id string, now time.Time) *Share {
	expires := now.Add(24 * time.Hour)
	return &Share{
		ID:          id,
		ResourceRef: "storage:object-" + id,
		TokenHash:   hashShareToken("token-" + id),
		ExpiresAt:   &expires,
	}
}

// --- ShareRepository -------------------------------------------------

func TestShareRepository_AssertIsolated(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Share {
		return newTestShare(uuid.NewString(), now)
	})
}

func TestShareRepository_ByTokenHash_FindsWithinTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctxA, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.byTokenHash(ctxA, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(own tenant): %v", err)
	}
	if got.ID != share.ID {
		t.Errorf("byTokenHash(own tenant) = %+v, want ID %q", got, share.ID)
	}

	if _, otherErr := repo.byTokenHash(ctxB, share.TokenHash); !errors.Is(otherErr, ErrNotAccessible) {
		t.Errorf("byTokenHash(other tenant) error = %v, want ErrNotAccessible", otherErr)
	}
}

func TestShareRepository_ByTokenHash_UnknownHashReportsNotAccessible(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	if _, err := repo.byTokenHash(ctx, "does-not-exist"); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("byTokenHash(unknown hash) error = %v, want ErrNotAccessible", err)
	}
}

func TestShareRepository_TryRecordView_GuardsOnCurrentState(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	won, err := repo.tryRecordView(ctx, share, now)
	if err != nil {
		t.Fatalf("tryRecordView (first): %v", err)
	}
	if !won {
		t.Fatalf("tryRecordView (first) = false, want true")
	}

	// Re-read so this copy's ViewCount matches the row's real state (1):
	// the equality guard on view_count would otherwise ALSO refuse a
	// second attempt, which would not isolate what this test wants to
	// prove -- that the max_views ceiling itself refuses it.
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 1 {
		t.Fatalf("fresh.ViewCount = %d, want 1", fresh.ViewCount)
	}

	won, err = repo.tryRecordView(ctx, fresh, now)
	if err != nil {
		t.Fatalf("tryRecordView (second, exhausted): %v", err)
	}
	if won {
		t.Errorf("tryRecordView (second, exhausted) = true, want false -- max_views already reached")
	}
}

func TestShareRepository_TryRecordView_StaleViewCountLosesTheRace(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate another caller having already recorded a view: the row's
	// real view_count is now 1, but this copy of share still says 0.
	other, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if racerWon, racerErr := repo.tryRecordView(ctx, other, now); racerErr != nil || !racerWon {
		t.Fatalf("tryRecordView (racer): won=%v err=%v", racerWon, racerErr)
	}

	won, err := repo.tryRecordView(ctx, share, now)
	if err != nil {
		t.Fatalf("tryRecordView (stale copy): %v", err)
	}
	if won {
		t.Errorf("tryRecordView (stale copy) = true, want false -- the row's view_count has moved on")
	}
}

func TestShareRepository_ListExpiredOrExhausted(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	live := newTestShare("live", now)
	if err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create(live): %v", err)
	}

	expired := newTestShare("expired", now)
	past := now.Add(-time.Hour)
	expired.ExpiresAt = &past
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("Create(expired): %v", err)
	}

	exhausted := newTestShare("exhausted", now)
	zero := 0
	exhausted.MaxViews = &zero
	if err := repo.Create(ctx, exhausted); err != nil {
		t.Fatalf("Create(exhausted): %v", err)
	}

	alreadyRevoked := newTestShare("already-revoked", now)
	alreadyRevoked.ExpiresAt = &past
	revokedAt := now
	alreadyRevoked.RevokedAt = &revokedAt
	if err := repo.Create(ctx, alreadyRevoked); err != nil {
		t.Fatalf("Create(alreadyRevoked): %v", err)
	}

	got, err := repo.listExpiredOrExhausted(ctx, now)
	if err != nil {
		t.Fatalf("listExpiredOrExhausted: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["expired"] {
		t.Errorf("listExpiredOrExhausted missed the expired share")
	}
	if !ids["exhausted"] {
		t.Errorf("listExpiredOrExhausted missed the view-exhausted share")
	}
	if ids["live"] {
		t.Errorf("listExpiredOrExhausted wrongly included the live share")
	}
	if ids["already-revoked"] {
		t.Errorf("listExpiredOrExhausted wrongly included an already-revoked share")
	}
}

// --- shareTokenIndex / createWithTokenIndex / tenantForTokenHash --------

// TestShareTokenIndex_AssertNotTenantScoped proves shareTokenIndex is
// genuinely platform data, not tenant data: a row written with no tenant
// in context is visible under an arbitrary one, and a row written under one
// tenant is visible under another -- the exact opposite of AssertIsolated,
// and the correct property for a table dbkit's tenant-scope GORM plugin
// must never engage on (model.go's shareTokenIndex doc comment explains
// why).
func TestShareTokenIndex_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	i := 0
	tenancytest.AssertNotTenantScoped(t, db, shareTokenIndex{},
		func(tx *gorm.DB) error {
			i++
			return tx.Create(&shareTokenIndex{
				TokenHash: hashShareToken("probe-token-" + uuid.NewString()),
				TenantID:  "irrelevant",
			}).Error
		},
		func(tx *gorm.DB) (int64, error) {
			var n int64
			err := tx.Model(&shareTokenIndex{}).Count(&n).Error
			return n, err
		},
	)
}

func TestShareRepository_CreateWithTokenIndex_WritesBothRowsUnderTheSameTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.createWithTokenIndex(ctx, share); err != nil {
		t.Fatalf("createWithTokenIndex: %v", err)
	}

	// The ordinary tenant-scoped lookup finds the Share row, under the
	// tenant createWithTokenIndex resolved from ctx.
	got, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if got.ID != share.ID {
		t.Errorf("byTokenHash = %+v, want ID %q", got, share.ID)
	}

	// The narrow, tenant-less lookup resolves the same tenant from the same
	// hash, with no tenant anywhere in ctx.
	tenant, err := repo.tenantForTokenHash(context.Background(), share.TokenHash)
	if err != nil {
		t.Fatalf("tenantForTokenHash: %v", err)
	}
	if tenant != "tenant-a" {
		t.Errorf("tenantForTokenHash = %q, want %q", tenant, "tenant-a")
	}
}

func TestShareRepository_TenantForTokenHash_UnknownHashReportsNotAccessible(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	if _, err := repo.tenantForTokenHash(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("tenantForTokenHash(unknown hash) error = %v, want ErrNotAccessible", err)
	}
}

// TestShareRepository_TenantForTokenHash_NeedsNoTenantInContext is the
// direct proof of this method's whole reason to exist: a genuinely
// anonymous caller -- context.Background(), nothing attached to it at all
// -- can still resolve a tenant from a token hash. A regression that
// silently made this method call pkgcore.MustTenantFromContext (or route
// through dbkit.WithTenantSession, which does the same) would fail this
// test with pkgcore.ErrNoTenant instead of a real answer.
func TestShareRepository_TenantForTokenHash_NeedsNoTenantInContext(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	share := newTestShare("share-1", now)
	if err := repo.createWithTokenIndex(pkgcore.WithTenant(context.Background(), "tenant-b"), share); err != nil {
		t.Fatalf("createWithTokenIndex: %v", err)
	}

	tenant, err := repo.tenantForTokenHash(context.Background(), share.TokenHash)
	if err != nil {
		t.Fatalf("tenantForTokenHash(no tenant in ctx) error = %v, want success", err)
	}
	if tenant != "tenant-b" {
		t.Errorf("tenantForTokenHash = %q, want %q", tenant, "tenant-b")
	}
}

// --- AccessLogRepository -----------------------------------------------

func newTestAccessLogEntry(id, shareID string, now time.Time) *AccessLogEntry {
	return &AccessLogEntry{
		ID:         id,
		ShareID:    shareID,
		OccurredAt: now,
		Outcome:    AccessOutcomeGranted,
	}
}

func TestAccessLogRepository_AssertIsolated(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	i := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *AccessLogEntry {
		i++
		return newTestAccessLogEntry(uuid.NewString(), "share-x", now.Add(time.Duration(i)*time.Second))
	})
}

func TestAccessLogRepository_ListByShare_ScopedToTenantAndShare(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	entry1 := newTestAccessLogEntry("log-1", "share-1", now)
	entry2 := newTestAccessLogEntry("log-2", "share-1", now.Add(time.Second))
	otherShare := newTestAccessLogEntry("log-3", "share-2", now)
	if err := repo.Create(ctxA, entry1); err != nil {
		t.Fatalf("Create(entry1): %v", err)
	}
	if err := repo.Create(ctxA, entry2); err != nil {
		t.Fatalf("Create(entry2): %v", err)
	}
	if err := repo.Create(ctxA, otherShare); err != nil {
		t.Fatalf("Create(otherShare): %v", err)
	}
	crossTenant := newTestAccessLogEntry("log-4", "share-1", now)
	if err := repo.Create(ctxB, crossTenant); err != nil {
		t.Fatalf("Create(crossTenant): %v", err)
	}

	got, err := repo.listByShare(ctxA, "share-1")
	if err != nil {
		t.Fatalf("listByShare: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listByShare returned %d rows, want 2", len(got))
	}
	if got[0].ID != "log-2" || got[1].ID != "log-1" {
		t.Errorf("listByShare order = [%s, %s], want newest first [log-2, log-1]", got[0].ID, got[1].ID)
	}
}
