package notification

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// TestSendRecord_AssertNotTenantScoped is the mandatory isolation assertion
// for a platform-domain table (docs/internal/04-data-and-tenancy.md's
// data-domain table): the outbound-delivery log must stay readable across
// tenants -- a retry worker must find the succeeded record a previous
// attempt left, whatever tenant context the retry carries -- so send_records
// must be visible to any query whatever tenant, or no tenant, is in the
// context. The suite's createFn returns a distinct id, tenant and
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
