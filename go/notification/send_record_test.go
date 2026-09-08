package notification

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestSendRecord_AssertNotTenantScoped is the mandatory isolation assertion
// for a platform-domain table: the outbound-delivery log must stay readable
// across tenants -- a retry worker must find the succeeded record a
// previous attempt left, whatever tenant context the retry carries -- so
// send_records must be visible to any query whatever tenant, or no tenant,
// is in the context. The suite's createFn returns a distinct id, tenant and
// idempotency key on every call, as it requires: the UNIQUE
// (tenant_id, idempotency_key) index would otherwise turn the suite's
// second create into a duplicate-key failure before the visibility question
// was ever asked.
func TestSendRecord_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)

	seq := 0
	tenancytest.AssertNotTenantScoped(t, db, SendRecord{},
		func(db *gorm.DB) error {
			seq++
			return db.Create(&SendRecord{
				ID:              fmt.Sprintf("sr-%03d", seq),
				TenantID:        "",
				TypeKey:         "clinic.appointment_reminder",
				Channel:         ChannelInApp,
				RecipientClass:  RecipientClassUser,
				RecipientUserID: "user-x",
				IdempotencyKey:  fmt.Sprintf("key-%03d", seq),
				Status:          SendRecordStatusSucceeded,
			}).Error
		},
		func(db *gorm.DB) (int64, error) {
			var n int64
			err := db.Model(&SendRecord{}).Count(&n).Error
			return n, err
		},
	)
}

// insertSendRecordFixture writes one send_records row straight through the
// plain *gorm.DB -- the sanctioned data path for platform data, exactly as
// send_record.go's own repository uses it. The row carries tenantID so the
// scoped-lookup tests below can place records under distinct tenants.
func insertSendRecordFixture(t *testing.T, db *gorm.DB, rec *SendRecord) {
	t.Helper()
	if err := db.Create(rec).Error; err != nil {
		t.Fatalf("insert send record fixture %s: %v", rec.ID, err)
	}
}

func testSendRecord(t *testing.T, id, tenantID, key string) *SendRecord {
	t.Helper()
	return &SendRecord{
		ID:              id,
		TenantID:        tenantID,
		TypeKey:         fixtureTypeAppointment,
		Channel:         ChannelEmail,
		RecipientClass:  RecipientClassUser,
		RecipientUserID: "user-7",
		Status:          SendRecordStatusSucceeded,
		IdempotencyKey:  key,
	}
}

// TestSendRecordRepository_ByTenantAndKey_EmptyTable_ReturnsNil pins the
// absent case of the replay probe: a delivery key nobody has ever recorded
// is (nil, nil) -- the "never attempted" answer that lets the delivery job
// proceed.
func TestSendRecordRepository_ByTenantAndKey_EmptyTable_ReturnsNil(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	got, err := repo.ByTenantAndKey(tenantCtx("tenant-acme"), "tenant-acme", "never-seen-key")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got != nil {
		t.Errorf("ByTenantAndKey on an empty table = %+v, want nil", got)
	}
}

// TestSendRecordRepository_ByTenantAndKey_MatchingRow_ReturnsIt pins the
// replay probe's hit case: a record under the (tenant, key) pair is
// returned with every column intact.
func TestSendRecordRepository_ByTenantAndKey_MatchingRow_ReturnsIt(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	want := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	want.Error = "transport refused"
	want.DurationMs = 42
	insertSendRecordFixture(t, db, want)

	got, err := repo.ByTenantAndKey(tenantCtx("tenant-acme"), "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got == nil {
		t.Fatal("ByTenantAndKey on a matching row = nil, want the record")
	}
	if got.ID != want.ID || got.Status != want.Status || got.Error != want.Error ||
		got.DurationMs != want.DurationMs || got.Channel != want.Channel ||
		got.RecipientClass != want.RecipientClass {
		t.Errorf("ByTenantAndKey = %+v, want %+v", got, want)
	}
}

// TestSendRecordRepository_ByTenantAndKey_IsScopedPerTenant pins the
// scoped-uniqueness semantics of the (tenant_id, idempotency_key) pair:
// the same key under a different tenant is a different delivery -- one
// tenant's succeeded record must never satisfy another tenant's replay
// probe, or a retry under the wrong tenant would silently skip a send it
// was responsible for.
func TestSendRecordRepository_ByTenantAndKey_IsScopedPerTenant(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-0001", "tenant-acme", "shared-key"))

	got, err := repo.ByTenantAndKey(tenantCtx("tenant-bright"), "tenant-bright", "shared-key")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got != nil {
		t.Errorf("ByTenantAndKey under another tenant = %+v, want nil -- the key is scoped to its tenant", got)
	}
}

// TestSendRecordRepository_Create_DuplicatePairWithinTenant_Refused pins
// the database-level dedupe the delivery pipeline's at-most-once property
// rests on: two Create calls with the same (tenant, idempotency key) cannot
// both land. The second surfaces as the unique-index violation -- the
// failure mode that makes concurrent double-enqueues converge on one
// record instead of two.
func TestSendRecordRepository_Create_DuplicatePairWithinTenant_Refused(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	rec1 := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	rec2 := testSendRecord(t, "sr-0002", "tenant-acme", "delivery-key-1")
	if err := repo.Create(tenantCtx("tenant-acme"), rec1); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	if err := repo.Create(tenantCtx("tenant-acme"), rec2); err == nil {
		t.Fatal("Create(duplicate tenant+key) succeeded, want the unique-index violation")
	}

	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("send_records holds %d rows after a duplicate Create, want 1", n)
	}
}

// TestSendRecordRepository_Save_UpsertsInPlace pins the retry-overwrite
// contract: Save with an id that already exists updates the row in place --
// the failed attempt's status and message replace the earlier attempt's,
// the row count stays at one, and the updated_at clock moves.
func TestSendRecordRepository_Save_UpsertsInPlace(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	rec := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	ctx := tenantCtx("tenant-acme")
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec.Status = SendRecordStatusFailed
	rec.Error = "smtp 550 relay denied"
	rec.DurationMs = 310
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got == nil {
		t.Fatal("ByTenantAndKey after Save = nil, want the updated record")
	}
	if got.Status != SendRecordStatusFailed || got.Error != "smtp 550 relay denied" ||
		got.DurationMs != 310 {
		t.Errorf("after Save = %+v, want the failed attempt's fields", got)
	}
	if !got.UpdatedAt.After(rec.CreatedAt) {
		t.Errorf("updated_at %v did not move past created_at %v", got.UpdatedAt, rec.CreatedAt)
	}

	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("send_records holds %d rows after a Save, want 1 (upsert in place)", n)
	}
}

// TestSendRecordRepository_Save_InsertsWhenAbsent pins Save's insert leg:
// a delivery that failed before any record existed -- the first attempt's
// record write itself failed, say -- lands through the same call a
// successful attempt would have used.
func TestSendRecordRepository_Save_InsertsWhenAbsent(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	rec := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	rec.Status = SendRecordStatusFailed
	if err := repo.Save(tenantCtx("tenant-acme"), rec); err != nil {
		t.Fatalf("Save on an absent id: %v", err)
	}

	got, err := repo.ByTenantAndKey(tenantCtx("tenant-acme"), "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got == nil || got.Status != SendRecordStatusFailed {
		t.Errorf("ByTenantAndKey after Save-on-absent = %+v, want the failed record", got)
	}
}

// TestSendRecordRepository_SaveGuarded_RefusesToDowngradeSucceededRow pins
// SaveGuarded's refusal leg -- the statement-level never-downgrade-succeeded
// guard every write to an existing send record runs under (delivery.go's
// settle doc): a write that says failed over a row that says succeeded is
// refused -- (false, nil), zero rows affected -- and the row survives
// byte-identical with updated_at unmoved, exactly as settle's dropped
// writes must leave it.
func TestSendRecordRepository_SaveGuarded_RefusesToDowngradeSucceededRow(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)
	ctx := tenantCtx("tenant-acme")

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1"))
	before, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}

	loser := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	loser.Status = SendRecordStatusFailed
	loser.Error = "smtp 550 relay denied"
	loser.DurationMs = 310

	landed, err := repo.SaveGuarded(ctx, loser)
	if err != nil {
		t.Fatalf("SaveGuarded: %v", err)
	}
	if landed {
		t.Fatal("SaveGuarded downgraded a succeeded row, want the refusal")
	}

	after, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("SaveGuarded's refusal rewrote the row: before = %+v, after = %+v", before, after)
	}
	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("send_records holds %d rows after a refused SaveGuarded, want 1 (no insert fallback)", n)
	}
}

// TestSendRecordRepository_SaveGuarded_LandsWhenTheGuardAllows pins the two
// allowed shapes of a guarded write -- the in-place retry overwrite a failed
// attempt makes over its own earlier failed row, and the upgrade a succeeded
// attempt makes over a failed row (a retry that finally sent, settling over
// the record its own earlier failures left). Both land with (true, nil) and
// the row rewritten -- updated_at moves -- exactly as the plain Save they
// replaced would have written.
func TestSendRecordRepository_SaveGuarded_LandsWhenTheGuardAllows(t *testing.T) {
	cases := []struct {
		name   string
		seed   string
		write  string
		errMsg string
	}{
		{
			name:   "failed over failed",
			seed:   SendRecordStatusFailed,
			write:  SendRecordStatusFailed,
			errMsg: "a later failure message",
		},
		{
			name:  "upgrade over failed",
			seed:  SendRecordStatusFailed,
			write: SendRecordStatusSucceeded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			repo := NewSendRecordRepository(db)
			ctx := tenantCtx("tenant-acme")

			seeded := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
			seeded.Status = tc.seed
			seeded.Error = "the earlier attempt"
			insertSendRecordFixture(t, db, seeded)

			writer := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
			writer.Status = tc.write
			writer.Error = tc.errMsg

			landed, err := repo.SaveGuarded(ctx, writer)
			if err != nil {
				t.Fatalf("SaveGuarded: %v", err)
			}
			if !landed {
				t.Fatalf("SaveGuarded over a %s row returned not-landed, want the write to land", tc.seed)
			}

			got, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
			if err != nil {
				t.Fatalf("ByTenantAndKey: %v", err)
			}
			if got == nil {
				t.Fatal("ByTenantAndKey after SaveGuarded = nil, want the row")
			}
			if got.Status != tc.write || got.Error != tc.errMsg {
				t.Errorf("after SaveGuarded = %+v, want status %s with message %q", got, tc.write, tc.errMsg)
			}
			if !got.UpdatedAt.After(seeded.CreatedAt) {
				t.Errorf("updated_at %v did not move past the seed's created_at %v", got.UpdatedAt, seeded.CreatedAt)
			}
		})
	}
}

// TestSendRecordRepository_SaveGuarded_AllowsASucceededResettle pins the
// guard's one symmetric allowance: a write that says succeeded over a row
// that already says succeeded lands. The guard refuses only a different
// terminal status over succeeded; the double-send window's two successful
// attempts both re-settle the same key, and the second one must still land
// (the metrics of a real delivery follow the write that records it).
func TestSendRecordRepository_SaveGuarded_AllowsASucceededResettle(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)
	ctx := tenantCtx("tenant-acme")

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1"))

	replay := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	landed, err := repo.SaveGuarded(ctx, replay)
	if err != nil {
		t.Fatalf("SaveGuarded: %v", err)
	}
	if !landed {
		t.Fatal("SaveGuarded refused a succeeded re-settle of a succeeded row, want the write to land")
	}
	got, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got == nil || got.Status != SendRecordStatusSucceeded {
		t.Errorf("after SaveGuarded = %+v, want the succeeded row", got)
	}
}

// TestSendRecordRepository_SaveGuarded_AbsentId_IsARefusalNotACreate pins
// SaveGuarded's no-insert contract: unlike Save, a guarded write over an id
// no row carries is a refusal -- (false, nil) -- not a create. settle reaches
// SaveGuarded only with an id its own probe read off an existing row, and
// send_records has no delete path, so in settle's hands a refusal and a
// missing row are the same already-delivered answer.
func TestSendRecordRepository_SaveGuarded_AbsentId_IsARefusalNotACreate(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	rec := testSendRecord(t, "sr-never", "tenant-acme", "delivery-key-1")
	rec.Status = SendRecordStatusFailed
	landed, err := repo.SaveGuarded(tenantCtx("tenant-acme"), rec)
	if err != nil {
		t.Fatalf("SaveGuarded: %v", err)
	}
	if landed {
		t.Fatal("SaveGuarded on an absent id landed, want the refusal")
	}
	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("send_records holds %d rows after a SaveGuarded on an absent id, want 0", n)
	}
}

// TestSendRecordRepository_SaveGuarded_ConcurrentRefusalsKeepTheSucceededRow
// pins the refusal under true concurrency: sixteen failed writes over one
// already-succeeded row, all holding the adopted id and all racing to land
// at once -- the double-send window's late failed settles (delivery.go's
// settle doc) -- must every one be refused by the guard in its own
// statement, leaving the succeeded row byte-identical. A probe-then-write
// guard would let whichever write lands after the winner's commit through;
// a guard evaluated by the write itself cannot.
func TestSendRecordRepository_SaveGuarded_ConcurrentRefusalsKeepTheSucceededRow(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)
	ctx := tenantCtx("tenant-acme")

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1"))
	before, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var refusals, landings int
	var writeErrs []error
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			loser := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
			loser.Status = SendRecordStatusFailed
			loser.Error = "smtp 550 relay denied"
			landed, writeErr := repo.SaveGuarded(ctx, loser)
			mu.Lock()
			defer mu.Unlock()
			if writeErr != nil {
				writeErrs = append(writeErrs, writeErr)
				return
			}
			if landed {
				landings++
			} else {
				refusals++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(writeErrs) != 0 {
		t.Fatalf("SaveGuarded failed %d times: %v", len(writeErrs), writeErrs)
	}
	if refusals != 16 || landings != 0 {
		t.Errorf("16 concurrent failed writes over a succeeded row: %d refusals, %d landings, want 16 refusals", refusals, landings)
	}
	after, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("concurrent SaveGuarded refusals rewrote the row: before = %+v, after = %+v", before, after)
	}
	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("send_records holds %d rows after the concurrent refusals, want 1", n)
	}
}

// TestSendRecordRepository_SaveGuarded_ConcurrentSettleRaceConvergesSucceeded
// pins the race the guarded write exists to settle: sixteen succeeded and
// sixteen failed settles racing over one row that a first, failed attempt
// already left. Whichever interleaving the writers land in, the outcome is
// deterministic -- the row ends succeeded. A failed write lands only while
// the row still says failed, and the moment any succeeded write lands the
// row says succeeded forever (every later failed write is refused by its own
// statement), so the last write to land is always a succeeded one. A
// probe-then-write guard, whose failed writes can land after the winner's
// commit, would end this same race on a failed row.
func TestSendRecordRepository_SaveGuarded_ConcurrentSettleRaceConvergesSucceeded(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)
	ctx := tenantCtx("tenant-acme")

	seeded := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	seeded.Status = SendRecordStatusFailed
	seeded.Error = "an earlier attempt's failure"
	insertSendRecordFixture(t, db, seeded)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var writeErrs []error
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(failed bool) {
			defer wg.Done()
			<-start
			rec := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
			if failed {
				rec.Status = SendRecordStatusFailed
				rec.Error = "smtp 550 relay denied"
			}
			_, writeErr := repo.SaveGuarded(ctx, rec)
			if writeErr != nil {
				mu.Lock()
				writeErrs = append(writeErrs, writeErr)
				mu.Unlock()
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()

	if len(writeErrs) != 0 {
		t.Fatalf("SaveGuarded failed %d times: %v", len(writeErrs), writeErrs)
	}
	got, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey: %v", err)
	}
	if got == nil || got.Status != SendRecordStatusSucceeded {
		t.Errorf("the concurrent settle race ended on %+v, want the row succeeded -- no failed write may outlive a succeeded one", got)
	}
	var n int64
	if err := db.Model(&SendRecord{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("send_records holds %d rows after the race, want 1", n)
	}
}

// TestSendRecordRepository_ListByFilter_NoTenant_Refused pins D10's one
// error: a filter with no TenantID is refused before any query runs, the
// same forgotten-tenant-filter refusal ByTenantAndKey's own hand-written
// WHERE clause exists to make impossible to skip by accident.
func TestSendRecordRepository_ListByFilter_NoTenant_Refused(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	_, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{Limit: 50})
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrSendRecordTenantRequired.Code {
		t.Fatalf("ListByFilter with no TenantID error = %v, want %s", err, ErrSendRecordTenantRequired.Code)
	}
}

// TestSendRecordRepository_ListByFilter_IsScopedPerTenant pins D10's
// cross-tenant read discipline: a filter naming one tenant never returns
// another tenant's records, even with every other field left at its zero
// value.
func TestSendRecordRepository_ListByFilter_IsScopedPerTenant(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-acme-1", "tenant-acme", "key-1"))
	insertSendRecordFixture(t, db, testSendRecord(t, "sr-bright-1", "tenant-bright", "key-2"))

	got, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme", Limit: 50})
	if err != nil {
		t.Fatalf("ListByFilter: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sr-acme-1" {
		t.Fatalf("ListByFilter(tenant-acme) = %+v, want exactly the acme record", got)
	}
}

// TestSendRecordRepository_ListByFilter_ChannelAndStatus_Match pins D10's
// second and third filter dimensions: a channel or status filter narrows
// the result to exactly the matching rows, and combining both is an AND,
// not an OR.
func TestSendRecordRepository_ListByFilter_ChannelAndStatus_Match(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	emailSucceeded := testSendRecord(t, "sr-1", "tenant-acme", "key-1")
	emailSucceeded.Channel = ChannelEmail
	emailSucceeded.Status = SendRecordStatusSucceeded
	insertSendRecordFixture(t, db, emailSucceeded)

	emailFailed := testSendRecord(t, "sr-2", "tenant-acme", "key-2")
	emailFailed.Channel = ChannelEmail
	emailFailed.Status = SendRecordStatusFailed
	insertSendRecordFixture(t, db, emailFailed)

	smsSucceeded := testSendRecord(t, "sr-3", "tenant-acme", "key-3")
	smsSucceeded.Channel = ChannelSMS
	smsSucceeded.Status = SendRecordStatusSucceeded
	insertSendRecordFixture(t, db, smsSucceeded)

	got, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{
		TenantID: "tenant-acme",
		Channel:  ChannelEmail,
		Status:   SendRecordStatusFailed,
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("ListByFilter: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sr-2" {
		t.Fatalf("ListByFilter(channel=email, status=failed) = %+v, want exactly sr-2", got)
	}
}

// TestSendRecordRepository_ListByFilter_TimeRange_ExcludesOutsideRecords
// pins D10's fourth filter dimension: From/To bound created_at, excluding
// a record on either side of the window while keeping one inside it.
func TestSendRecordRepository_ListByFilter_TimeRange_ExcludesOutsideRecords(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	old := testSendRecord(t, "sr-old", "tenant-acme", "key-old")
	insertSendRecordFixture(t, db, old)
	if err := db.Model(&SendRecord{}).Where("id = ?", "sr-old").
		Update("created_at", time.Now().Add(-48*time.Hour)).Error; err != nil {
		t.Fatalf("backdate sr-old: %v", err)
	}

	inWindow := testSendRecord(t, "sr-in-window", "tenant-acme", "key-in-window")
	insertSendRecordFixture(t, db, inWindow)

	future := testSendRecord(t, "sr-future", "tenant-acme", "key-future")
	insertSendRecordFixture(t, db, future)
	if err := db.Model(&SendRecord{}).Where("id = ?", "sr-future").
		Update("created_at", time.Now().Add(48*time.Hour)).Error; err != nil {
		t.Fatalf("postdate sr-future: %v", err)
	}

	got, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{
		TenantID: "tenant-acme",
		From:     time.Now().Add(-24 * time.Hour),
		To:       time.Now().Add(24 * time.Hour),
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("ListByFilter: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sr-in-window" {
		t.Fatalf("ListByFilter(time-bounded) = %+v, want exactly sr-in-window", got)
	}
}

// TestSendRecordRepository_ListByFilter_NewestFirstWithLimitAndOffset pins
// D10's paging contract: results are ordered created_at DESC, id DESC
// (ListForRecipient's identical stable-paging tiebreak), and Limit/Offset
// page through them exactly as given, with no clamping inside the
// repository.
func TestSendRecordRepository_ListByFilter_NewestFirstWithLimitAndOffset(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"sr-1", "sr-2", "sr-3"} {
		rec := testSendRecord(t, id, "tenant-acme", "key-"+id)
		insertSendRecordFixture(t, db, rec)
		if err := db.Model(&SendRecord{}).Where("id = ?", id).
			Update("created_at", base.Add(time.Duration(i)*time.Minute)).Error; err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}

	all, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme", Limit: 50})
	if err != nil {
		t.Fatalf("ListByFilter: %v", err)
	}
	if len(all) != 3 || all[0].ID != "sr-3" || all[1].ID != "sr-2" || all[2].ID != "sr-1" {
		t.Fatalf("ListByFilter order = %+v, want newest first (sr-3, sr-2, sr-1)", all)
	}

	page, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("ListByFilter(paged): %v", err)
	}
	if len(page) != 1 || page[0].ID != "sr-2" {
		t.Fatalf("ListByFilter(limit=1, offset=1) = %+v, want exactly sr-2", page)
	}
}

// TestSendRecordRepository_SaveGuarded_PreservesCreatedAt pins created_at's
// survival of an in-place guarded rewrite -- the retry-overwrite shape
// delivery.go's settle runs on a key that already has a record. The first
// write for a key inserts through Save, where gorm's autoCreateTime stamps
// created_at; a later attempt builds a FRESH record (sendRecordFor leaves
// CreatedAt at the zero value) and settles it by adopting only the existing
// row's id, so the guarded UPDATE's rec.CreatedAt is still the zero value.
// Select("*") would then write that zero into the row's created_at column --
// collapsing the delivery's creation instant to year 1 -- and the record
// would silently vanish from the time-bounded operator search ListByFilter's
// From/To ranges implement (its doc names D10's "did this delivery actually
// go out" query). The guarded write must never touch the create-only
// created_at column.
func TestSendRecordRepository_SaveGuarded_PreservesCreatedAt(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)
	ctx := tenantCtx("tenant-acme")

	first := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	first.Status = SendRecordStatusFailed
	first.Error = "smtp: connection refused"
	if err := repo.Save(ctx, first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	seeded, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey after the first Save: %v", err)
	}
	if seeded == nil {
		t.Fatal("the first Save left no row, want the failed record")
	}
	createdAt := seeded.CreatedAt
	if createdAt.IsZero() {
		t.Fatal("the first write left created_at at the zero value, test premise broken")
	}

	// The retried attempt's record is built fresh (CreatedAt still the zero
	// value) and carries the existing row's id -- settle's adopt copies only
	// the id, nothing else.
	retry := testSendRecord(t, "sr-0001", "tenant-acme", "delivery-key-1")
	retry.Status = SendRecordStatusSucceeded
	landed, err := repo.SaveGuarded(ctx, retry)
	if err != nil {
		t.Fatalf("SaveGuarded: %v", err)
	}
	if !landed {
		t.Fatal("SaveGuarded over the failed row returned not-landed, want the upgrade to land")
	}

	got, err := repo.ByTenantAndKey(ctx, "tenant-acme", "delivery-key-1")
	if err != nil {
		t.Fatalf("ByTenantAndKey after SaveGuarded: %v", err)
	}
	if got == nil {
		t.Fatal("ByTenantAndKey after SaveGuarded = nil, want the row")
	}
	if got.Status != SendRecordStatusSucceeded {
		t.Errorf("after SaveGuarded = %+v, want the retry's succeeded status", got)
	}
	if !got.UpdatedAt.After(createdAt) {
		t.Errorf("updated_at %v did not move past the first attempt's created_at %v", got.UpdatedAt, createdAt)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("created_at after the guarded re-settle = %v (zero = %v), want the first attempt's %v unchanged", got.CreatedAt, got.CreatedAt.IsZero(), createdAt)
	}

	// The audit-visibility consequence: the same record must still answer the
	// time-bounded query around its first attempt's instant. A row whose
	// created_at collapsed to year 1 falls out of any From/To range and the
	// operator's search would report the delivery never went out.
	page, err := repo.ListByFilter(ctx, SendRecordFilter{
		TenantID: "tenant-acme",
		From:     createdAt.Add(-time.Second),
		To:       createdAt.Add(time.Second),
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("ListByFilter(time-bounded): %v", err)
	}
	if len(page) != 1 || page[0].ID != "sr-0001" {
		t.Errorf("ListByFilter around the first attempt's instant = %+v, want the retried delivery's one record (a zeroed created_at is invisible to From/To)", page)
	}
}

// TestSendRecordRepository_ListByFilter_ValidatesLimitAndOffset pins the
// paging parameters' honest handling: a zero Limit is gorm's "no limit" --
// an unbounded read the caller never asked for -- and a negative Offset is
// meaningless, so both are refused with the filter's coded error before any
// query runs, never silently served as an unlimited dump or a nonsense
// page. A valid page still answers exactly as before.
func TestSendRecordRepository_ListByFilter_ValidatesLimitAndOffset(t *testing.T) {
	db := newTestDB(t)
	repo := NewSendRecordRepository(db)

	insertSendRecordFixture(t, db, testSendRecord(t, "sr-1", "tenant-acme", "key-1"))
	insertSendRecordFixture(t, db, testSendRecord(t, "sr-2", "tenant-acme", "key-2"))

	_, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme"})
	if err == nil {
		t.Fatal("ListByFilter with a zero Limit succeeded, want the coded refusal (a zero limit is an unbounded read in gorm)")
	}
	assertCode(t, err, "notification.send_record_filter_invalid")

	_, err = repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme", Limit: 50, Offset: -1})
	if err == nil {
		t.Fatal("ListByFilter with a negative Offset succeeded, want the coded refusal")
	}
	assertCode(t, err, "notification.send_record_filter_invalid")

	got, err := repo.ListByFilter(tenantCtx("tenant-acme"), SendRecordFilter{TenantID: "tenant-acme", Limit: 50})
	if err != nil {
		t.Fatalf("ListByFilter(valid page): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListByFilter(valid page) = %d rows, want the 2 inserted records", len(got))
	}
}
