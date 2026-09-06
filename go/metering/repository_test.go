package metering

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	inserted, err := insertOutboxRecord(ctx, db, rec)
	if err != nil || !inserted {
		t.Fatalf("insertOutboxRecord = inserted:%v err:%v, want inserted:true err:nil", inserted, err)
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

// TestInsertOutboxRecord_DuplicateKey_IsANoOpNotAnError pins the
// dialect-independent half of the outbox idempotent-retry fix: a second
// insert for the same (tenant_id, idempotency_key) reports inserted ==
// false with NO error -- the insert runs as ON CONFLICT DO NOTHING, so
// the transaction is never left in the aborted state that would break
// Enqueue's read-back recovery (and the caller's own transaction) on
// PostgreSQL, where a statement error aborts the whole transaction. The
// pre-fix function returned gorm.ErrDuplicatedKey here; the PostgreSQL
// half of the regression, where the aborted transaction is actually
// observable, lives in the integration tier
// (TestPostgres_Enqueue_IdempotentRetry_InsideOneCallerTransaction).
func TestInsertOutboxRecord_DuplicateKey_IsANoOpNotAnError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	inserted, err := insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-1", "tenant-a", "idem-dup"))
	if err != nil || !inserted {
		t.Fatalf("insertOutboxRecord (first) = inserted:%v err:%v, want inserted:true err:nil", inserted, err)
	}
	inserted, err = insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-2", "tenant-a", "idem-dup"))
	if err != nil {
		t.Fatalf("insertOutboxRecord (duplicate) = %v, want inserted:false err:nil (no-op, not a unique-violation error)", err)
	}
	if inserted {
		t.Error("insertOutboxRecord (duplicate) = inserted:true, want inserted:false")
	}

	rows, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "rec-1" {
		t.Fatalf("outbox holds %d row(s) after the duplicate insert, want exactly the first row (no duplicate)", len(rows))
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

	if _, err := insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-a", "tenant-a", "idem-shared")); err != nil {
		t.Fatalf("insertOutboxRecord(tenant-a): %v", err)
	}
	if _, err := insertOutboxRecord(ctx, db, newTestOutboxRecord("rec-b", "tenant-b", "idem-shared")); err != nil {
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
		if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
			t.Fatalf("insertOutboxRecord(%d): %v", i, err)
		}
	}
	delivered := newTestOutboxRecord("delivered-1", "tenant-a", "idem-delivered")
	delivered.Status = outboxStatusDelivered
	if _, err := insertOutboxRecord(ctx, db, delivered); err != nil {
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

// TestClaimPendingOutboxRecords_LeastFailedFirst pins the claim query's
// anti-starvation ordering: among pending rows, never-failed rows
// (Attempts 0) are claimed before already-failed ones, whatever their
// age, so a pile of permanently failing rows at the head of the queue
// (older, high-Attempts) can never occupy a whole batch ahead of a fresh
// row. Within one Attempts tier the oldest row still goes first -- the
// FIFO order the sibling test above pins -- so the two orderings
// disagree only exactly where the starvation hazard lives.
func TestClaimPendingOutboxRecords_LeastFailedFirst(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// An older row that has already failed three times...
	failed := newTestOutboxRecord("failed-3", "tenant-a", "idem-failed-3")
	failed.Attempts = 3
	failed.CreatedAt = time.Now().Add(-time.Hour)
	if _, err := insertOutboxRecord(ctx, db, failed); err != nil {
		t.Fatalf("insertOutboxRecord(failed): %v", err)
	}
	// ...and a brand-new row that has never been attempted.
	fresh := newTestOutboxRecord("fresh-0", "tenant-a", "idem-fresh-0")
	fresh.CreatedAt = time.Now()
	if _, err := insertOutboxRecord(ctx, db, fresh); err != nil {
		t.Fatalf("insertOutboxRecord(fresh): %v", err)
	}

	got, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claimPendingOutboxRecords returned %d rows, want 2", len(got))
	}
	if got[0].ID != "fresh-0" || got[1].ID != "failed-3" {
		t.Errorf("claimPendingOutboxRecords order = [%s, %s], want never-failed-first [fresh-0, failed-3]", got[0].ID, got[1].ID)
	}
}

func TestMarkOutboxDelivered(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-1", "tenant-a", "idem-1")
	if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
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
	if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
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

// TestTruncateError_DoesNotSplitAMultiByteRune pins the UTF-8 boundary
// finding: truncateError cut on byte 500 unconditionally, so a cause
// whose 500th byte fell inside a multi-byte rune stored an invalid-UTF-8
// tail -- a value PostgreSQL rejects on write with SQLSTATE 22021, taking
// the failure-record write (markOutboxAttemptFailed) down with it. The
// truncation must end on a rune boundary, whatever the byte offset of the
// cut.
func TestTruncateError_DoesNotSplitAMultiByteRune(t *testing.T) {
	// A 4-byte rune (U+1F600), placed so the 500-byte cut lands at each of
	// the three possible offsets inside it.
	smile := "\U0001F600"
	for _, prefixLen := range []int{maxLastErrorLength - 3, maxLastErrorLength - 2, maxLastErrorLength - 1} {
		prefix := strings.Repeat("a", prefixLen)
		cause := prefix + smile + strings.Repeat("b", 50)
		got := truncateError(cause)
		if !utf8.ValidString(got) {
			t.Errorf("truncateError(cause with a rune straddling byte %d) = invalid UTF-8: %q", maxLastErrorLength, got)
			continue
		}
		if len(got) > maxLastErrorLength {
			t.Errorf("truncateError length = %d, want at most %d", len(got), maxLastErrorLength)
		}
		if got != prefix {
			t.Errorf("truncateError = %q, want the cut to drop the whole straddling rune: %q", got, prefix)
		}
	}
}

// TestMarkOutboxAttemptFailed_LongMultiByteCause_StoredValueStaysValidUTF8
// pins the finding at the write path itself -- the stored value is the
// assertion target, exactly as PostgreSQL would validate it on its way
// into the column: a cause whose byte-truncation used to split a rune is
// stored whole-rune-truncated, valid UTF-8 and within the column's byte
// bound.
func TestMarkOutboxAttemptFailed_LongMultiByteCause_StoredValueStaysValidUTF8(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-1", "tenant-a", "idem-1")
	if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
		t.Fatalf("insertOutboxRecord: %v", err)
	}

	// 200 three-byte runes: comfortably over the 500-byte column bound.
	cause := strings.Repeat("界", 200)
	if err := markOutboxAttemptFailed(ctx, db, rec.ID, cause); err != nil {
		t.Fatalf("markOutboxAttemptFailed: %v", err)
	}

	pending, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending rows = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if !utf8.ValidString(got.LastError) {
		t.Errorf("stored LastError = invalid UTF-8: %q (PostgreSQL would refuse this write with SQLSTATE 22021)", got.LastError)
	}
	if len(got.LastError) > maxLastErrorLength {
		t.Errorf("stored LastError length = %d, want at most %d", len(got.LastError), maxLastErrorLength)
	}
	if !strings.HasPrefix(cause, got.LastError) {
		t.Errorf("stored LastError = %q, want a prefix of the cause (truncated, never rewritten)", got.LastError)
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
