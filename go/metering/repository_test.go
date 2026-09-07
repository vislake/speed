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
	now := time.Now()
	return &OutboxRecord{
		ID:             id,
		TenantID:       tenantID,
		Feature:        "ai.generation",
		Quantity:       1,
		IdempotencyKey: idempotencyKey,
		OccurredAt:     now,
		Status:         outboxStatusPending,
		// RetryAfter starts at CreatedAt, exactly as Enqueue schedules a
		// never-failed row (see outbox.go); a test that backdates
		// CreatedAt afterwards must backdate RetryAfter with it.
		RetryAfter: &now,
		CreatedAt:  now,
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

// TestClaimPendingOutboxRecords_OrderedByRetrySchedule pins the claim
// query's schedule semantics -- the ordering migration 0005 introduced to
// replace the attempts-class ordering (reviewer finding P1-metering-10):
// only pending rows whose retry_after has arrived are claimable, and they
// come back oldest-scheduled first. A row that failed once and whose
// re-claim window has opened is claimed before a never-failed row
// enqueued after it (its schedule slot is older), while a row still
// inside its window -- however long it has waited -- is not claimable at
// all. RetryAfter is CreatedAt for never-failed rows, so the sibling test
// above's FIFO among fresh rows is the same order the schedule produces
// for them.
func TestClaimPendingOutboxRecords_OrderedByRetrySchedule(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now()

	backdate := func(rec *OutboxRecord, at time.Time) *OutboxRecord {
		rec.CreatedAt = at
		rec.RetryAfter = &at
		return rec
	}

	// A row that failed once an hour ago, whose re-claim window (failure
	// time plus the retry delay) has long since opened...
	onceFailed := backdate(newTestOutboxRecord("failed-once", "tenant-a", "idem-failed-once"), now.Add(-time.Hour))
	onceFailed.Attempts = 1
	if _, err := insertOutboxRecord(ctx, db, onceFailed); err != nil {
		t.Fatalf("insertOutboxRecord(onceFailed): %v", err)
	}
	// ...a brand-new never-failed row...
	fresh := backdate(newTestOutboxRecord("fresh-0", "tenant-a", "idem-fresh-0"), now)
	if _, err := insertOutboxRecord(ctx, db, fresh); err != nil {
		t.Fatalf("insertOutboxRecord(fresh): %v", err)
	}
	// ...and a row still inside its re-claim window: its retry_after is
	// an hour away, so it must not be claimable at all this call.
	inWindow := backdate(newTestOutboxRecord("in-window", "tenant-a", "idem-in-window"), now.Add(-time.Hour))
	inWindow.Attempts = 5
	inWindow.RetryAfter = ptrTime(now.Add(time.Hour))
	if _, err := insertOutboxRecord(ctx, db, inWindow); err != nil {
		t.Fatalf("insertOutboxRecord(inWindow): %v", err)
	}

	got, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("claimPendingOutboxRecords returned %d rows, want 2 (the in-window row is not claimable)", len(got))
	}
	if got[0].ID != "failed-once" || got[1].ID != "fresh-0" {
		t.Errorf("claimPendingOutboxRecords order = [%s, %s], want schedule order [failed-once, fresh-0] (the once-failed row's re-claim slot is older than the fresh row's birth)", got[0].ID, got[1].ID)
	}
}

// ptrTime returns a pointer to t, for seeding nullable timestamp columns.
func ptrTime(t time.Time) *time.Time { return &t }

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

	retryAfter := time.Now().Add(time.Hour)
	if err := markOutboxAttemptFailed(ctx, db, "rec-1", "boom", retryAfter); err != nil {
		t.Fatalf("markOutboxAttemptFailed: %v", err)
	}

	var pending []OutboxRecord
	if err := db.WithContext(ctx).Where("status = ?", outboxStatusPending).Find(&pending).Error; err != nil {
		t.Fatalf("find pending rows: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending rows = %d, want 1 (still pending, per Dispatcher's indefinite-retry contract)", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", pending[0].Attempts)
	}
	if pending[0].LastError != "boom" {
		t.Errorf("LastError = %q, want %q", pending[0].LastError, "boom")
	}
	if pending[0].RetryAfter == nil || !pending[0].RetryAfter.Equal(retryAfter) {
		t.Errorf("RetryAfter = %v, want %v (the failed row is scheduled for its next claim, not left claimable immediately)", pending[0].RetryAfter, retryAfter)
	}
	// The row is not claimable until its retry_after arrives: the claim
	// query must refuse it, whatever the queue behind it holds.
	claimable, err := claimPendingOutboxRecords(ctx, db, 10)
	if err != nil {
		t.Fatalf("claimPendingOutboxRecords: %v", err)
	}
	if len(claimable) != 0 {
		t.Errorf("claimPendingOutboxRecords returned %d rows, want 0 (the row is inside its re-claim window)", len(claimable))
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

// TestTruncateError_ShortInvalidUTF8_IsSanitized is the reviewer finding
// P3-metering-15 regression in its unit form: truncateError's short-value
// fast path returned cause untouched whenever it fit the column, so a
// SHORT cause carrying an invalid byte sequence was stored raw -- and
// PostgreSQL refuses exactly that on the failure-record write
// (SQLSTATE 22021), taking the record of a failure down with the failure
// it recorded. The column safety must hold at ANY length, not only past
// the truncation point: invalid bytes are rendered as the Unicode
// replacement character (strings.ToValidUTF8), never passed through and
// never silently dropped. The fixed shape mirrors go/sharing's
// truncateAccessLogValue and go/authn's truncateClientField -- the third
// instance of the rune-safe helper this codebase now carries in three
// modules.
func TestTruncateError_ShortInvalidUTF8_IsSanitized(t *testing.T) {
	// A short value (well under the 500-byte bound) whose middle byte is
	// an invalid UTF-8 sequence -- what a caller-supplied error string
	// with raw bytes in it can look like at any length.
	cause := "upstream said: ok\xff\xfe then failed"
	if len(cause) >= maxLastErrorLength {
		t.Fatalf("test cause is %d bytes, want a SHORT cause (under %d) so the pre-fix fast path is what returns it", len(cause), maxLastErrorLength)
	}
	got := truncateError(cause)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateError(short invalid-UTF-8 cause) = invalid UTF-8: %q (PostgreSQL would refuse this write with SQLSTATE 22021)", got)
	}
	if got == cause {
		t.Errorf("truncateError(short invalid-UTF-8 cause) passed the raw bytes through unchanged: %q", got)
	}
	if !strings.Contains(got, "\uFFFD") {
		t.Errorf("truncateError = %q, want the invalid bytes rendered as the replacement character", got)
	}
	if strings.Contains(got, "\xff") || strings.Contains(got, "\xfe") {
		t.Errorf("truncateError = %q, want no raw invalid bytes left in it", got)
	}
}

// TestMarkOutboxAttemptFailed_ShortInvalidUTF8Cause_StoredValueIsSanitized
// pins the same finding at the write path itself, mirroring the existing
// long-cause stored-value test: a short cause carrying invalid bytes is
// stored sanitized -- the stored value is the assertion target, exactly
// as PostgreSQL would validate it on its way into the column.
func TestMarkOutboxAttemptFailed_ShortInvalidUTF8Cause_StoredValueIsSanitized(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	rec := newTestOutboxRecord("rec-invalid", "tenant-a", "idem-invalid")
	if _, err := insertOutboxRecord(ctx, db, rec); err != nil {
		t.Fatalf("insertOutboxRecord: %v", err)
	}

	cause := "provider replied with raw bytes \xff\xfe and then died"
	if err := markOutboxAttemptFailed(ctx, db, rec.ID, cause, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("markOutboxAttemptFailed: %v", err)
	}

	var pending []OutboxRecord
	if err := db.WithContext(ctx).Where("id = ?", rec.ID).Find(&pending).Error; err != nil {
		t.Fatalf("find row by id: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("rows = %d, want 1", len(pending))
	}
	if !utf8.ValidString(pending[0].LastError) {
		t.Errorf("stored LastError = invalid UTF-8: %q (PostgreSQL would refuse this write with SQLSTATE 22021)", pending[0].LastError)
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
	if err := markOutboxAttemptFailed(ctx, db, rec.ID, cause, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("markOutboxAttemptFailed: %v", err)
	}

	// Read the row back directly rather than through the claim query: the
	// row now sits inside its re-claim window, which is exactly what the
	// claim query must refuse to return.
	var pending []OutboxRecord
	if err := db.WithContext(ctx).Where("id = ?", rec.ID).Find(&pending).Error; err != nil {
		t.Fatalf("find row by id: %v", err)
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
