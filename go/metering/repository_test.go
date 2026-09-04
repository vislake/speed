package metering

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/metering/internal/testutil"
	"github.com/vislake/speed/go/metering/migrations"
)

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// --- SummaryRepository -----------------------------------------------------

func TestSummaryRepository_CreateAndFindByID(t *testing.T) {
	repo := NewSummaryRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	summary := &UsageSummary{
		ID:          summaryID("ai.generation", start),
		Feature:     "ai.generation",
		PeriodStart: start,
		PeriodEnd:   end,
		Quantity:    3,
	}
	if err := repo.Create(ctx, summary); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, summary.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Feature != "ai.generation" || got.Quantity != 3 {
		t.Errorf("FindByID = %+v, want Feature=ai.generation Quantity=3", got)
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("FindByID.TenantID = %q, want %q (set by Repository[T].Create from ctx)", got.TenantID, "tenant-a")
	}
}

// TestSummaryRepository_AssertIsolated runs the mandatory tenant-isolation
// suite against metering_usage_summaries. UsageSummary is tenant data
// (docs/internal/04-data-and-tenancy.md), so AssertIsolated -- not
// AssertNotTenantScoped -- is the correct half of the pair: one tenant's
// usage summary must never be readable, updatable or deletable from
// another tenant.
func TestSummaryRepository_AssertIsolated(t *testing.T) {
	repo := NewSummaryRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *UsageSummary {
		n++
		start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		feature := fmt.Sprintf("feature-%d", n)
		return &UsageSummary{
			ID:          summaryID(feature, start),
			Feature:     feature,
			PeriodStart: start,
			PeriodEnd:   start.AddDate(0, 1, 0),
			Quantity:    float64(n),
		}
	})
}

// --- Outbox: plain *gorm.DB functions ---------------------------------------

func newTestOutboxRecord(id, tenantID, idempotencyKey string) *OutboxRecord {
	return &OutboxRecord{
		ID:             id,
		TenantID:       tenantID,
		Feature:        "ai.generation",
		Quantity:       1,
		IdempotencyKey: idempotencyKey,
		OccurredAt:     time.Now(),
		Status:         outboxStatusPending,
		CreatedAt:      time.Now(),
	}
}

func TestInsertOutboxRecord_AndFindByIdempotencyKey(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-1", "tenant-a", "idem-1")
	if err := insertOutboxRecord(ctx, db, rec); err != nil {
		t.Fatalf("insertOutboxRecord: %v", err)
	}

	got, found, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-1")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if !found {
		t.Fatal("findOutboxByIdempotencyKey: found = false, want true")
	}
	if got.ID != "rec-1" {
		t.Errorf("findOutboxByIdempotencyKey.ID = %q, want %q", got.ID, "rec-1")
	}
}

func TestFindOutboxByIdempotencyKey_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, found, err := findOutboxByIdempotencyKey(context.Background(), db, "tenant-a", "does-not-exist")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey: %v", err)
	}
	if found {
		t.Error("findOutboxByIdempotencyKey: found = true, want false")
	}
}

// TestFindOutboxByIdempotencyKey_ScopedByTenant proves the lookup key is
// (tenant_id, idempotency_key) together, not idempotency_key alone --
// two different tenants may reuse the same idempotency key value without
// colliding.
func TestFindOutboxByIdempotencyKey_ScopedByTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-a", "tenant-a", "idem-shared")); err != nil {
		t.Fatalf("insertOutboxRecord(tenant-a): %v", err)
	}
	if err := insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-b", "tenant-b", "idem-shared")); err != nil {
		t.Fatalf("insertOutboxRecord(tenant-b): %v", err)
	}

	gotA, _, err := findOutboxByIdempotencyKey(ctx, db, "tenant-a", "idem-shared")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey(tenant-a): %v", err)
	}
	if gotA.ID != "rec-a" {
		t.Errorf("findOutboxByIdempotencyKey(tenant-a).ID = %q, want %q", gotA.ID, "rec-a")
	}
	gotB, _, err := findOutboxByIdempotencyKey(ctx, db, "tenant-b", "idem-shared")
	if err != nil {
		t.Fatalf("findOutboxByIdempotencyKey(tenant-b): %v", err)
	}
	if gotB.ID != "rec-b" {
		t.Errorf("findOutboxByIdempotencyKey(tenant-b).ID = %q, want %q", gotB.ID, "rec-b")
	}
}

func TestClaimPendingOutboxRecords(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		rec := newTestOutboxRecord(fmt.Sprintf("pending-%d", i), "tenant-a", fmt.Sprintf("idem-%d", i))
		rec.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		if err := insertOutboxRecord(ctx, db, rec); err != nil {
			t.Fatalf("insertOutboxRecord(%d): %v", i, err)
		}
	}
	delivered := newTestOutboxRecord("delivered-1", "tenant-a", "idem-delivered")
	delivered.Status = outboxStatusDelivered
	if err := insertOutboxRecord(ctx, db, delivered); err != nil {
		t.Fatalf("insertOutboxRecord(delivered): %v", err)
	}

	got, err := claimPendingOutboxRecords(ctx, db, 2)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claimPendingOutboxRecords returned %d rows, want 2 (limit)", len(got))
	}
	if got[0].ID != "pending-0" || got[1].ID != "pending-1" {
		t.Errorf("claimPendingOutboxRecords order = [%s, %s], want oldest-first [pending-0, pending-1]", got[0].ID, got[1].ID)
	}
}

func TestMarkOutboxDelivered(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-1", "tenant-a", "idem-1")
	if err := insertOutboxRecord(ctx, db, rec); err != nil {
		t.Fatalf("insertOutboxRecord: %v", err)
	}

	deliveredAt := time.Now()
	if err := markOutboxDelivered(ctx, db, "rec-1", deliveredAt); err != nil {
		t.Fatalf("markOutboxDelivered: %v", err)
	}

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("claimPendingOutboxRecords after delivery = %d rows, want 0", len(pending))
	}
}

func TestMarkOutboxDelivered_UnknownID_IsNoOp(t *testing.T) {
	db := newTestDB(t)
	if err := markOutboxDelivered(context.Background(), db, "does-not-exist", time.Now()); err != nil {
		t.Errorf("markOutboxDelivered(unknown id) = %v, want nil (no-op)", err)
	}
}

func TestMarkOutboxAttemptFailed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-1", "tenant-a", "idem-1")
	if err := insertOutboxRecord(ctx, db, rec); err != nil {
		t.Fatalf("insertOutboxRecord: %v", err)
	}

	if err := markOutboxAttemptFailed(ctx, db, "rec-1", "boom"); err != nil {
		t.Fatalf("markOutboxAttemptFailed: %v", err)
	}

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("claimPendingOutboxRecords = %d rows, want 1 (still pending, per Dispatcher's indefinite-retry contract)", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", pending[0].Attempts)
	}
	if pending[0].LastError != "boom" {
		t.Errorf("LastError = %q, want %q", pending[0].LastError, "boom")
	}
}

func TestTruncateError(t *testing.T) {
	short := "boom"
	if got := truncateError(short); got != short {
		t.Errorf("truncateError(short) = %q, want unchanged %q", got, short)
	}

	long := make([]byte, maxLastErrorLength+50)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateError(string(long))
	if len(got) != maxLastErrorLength {
		t.Errorf("truncateError(long) length = %d, want %d", len(got), maxLastErrorLength)
	}
}

// TestOutbox_AssertNotTenantScoped proves metering_outbox_records is
// platform data (model.go's OutboxRecord doc comment): the tenant-scoping
// plugin must never filter it, and a row is visible regardless of which
// (or no) tenant is current -- Dispatcher's cross-tenant claim query
// depends on exactly this property.
func TestOutbox_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	n := 0
	createFn := func(db *gorm.DB) error {
		n++
		// The tenant_id value here is written verbatim -- unlike a
		// TenantScoped model, OutboxRecord's tenant_id is never forced to
		// ctx's tenant by the plugin, since the plugin does not look at
		// OutboxRecord at all.
		return db.Create(newTestOutboxRecord(
			fmt.Sprintf("scope-%d", n),
			"tenant-x",
			fmt.Sprintf("idem-scope-%d", n),
		)).Error
	}
	findFn := func(db *gorm.DB) (int64, error) {
		var count int64
		err := db.Model(&OutboxRecord{}).Count(&count).Error
		return count, err
	}
	tenancytest.AssertNotTenantScoped(t, db, OutboxRecord{}, createFn, findFn)
}
