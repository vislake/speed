package notification

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/notification/internal/testutil"
	"github.com/vislake/speed/go/notification/migrations"
)

// newTestDB returns a fresh, migrated SQLite database for one test, through
// the module's own migration files -- see internal/testutil's doc comment on
// why that makes every test here a from-zero migration proof too.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// tenantCtx wraps ctx's background with the tenant a repository call runs
// under. The tenant never travels any other way -- no header, no argument,
// no field on the record; the repository reads it from the context alone.
func tenantCtx(tenant string) context.Context {
	return pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenant))
}

// testMessage returns a fully populated inbox row with id as its primary key.
// Its TenantModel is deliberately left empty: dbkit.Repository.Create
// overwrites TenantID from the context's tenant, and the AssertIsolated
// suite below pins that behaviour; no test in this file needs to set the
// field by hand.
func testMessage(id string) *InboxMessage {
	return &InboxMessage{
		ID:              id,
		RecipientUserID: "user-7",
		TypeKey:         "note.shared",
		Title:           "Note 42 was shared with you",
		Body:            "Lin opened Note 42 and shared it with the whole clinic.",
	}
}

// TestRepository_AssertIsolated runs the tenant-data isolation suite every
// tenant-scoped repository in this codebase is required to pass
// (docs/internal/04-data-and-tenancy.md): two tenants' rows created side by
// side, each tenant's FindByID/List/Update/Delete seeing only its own, a
// forged TenantID overwritten by the context's tenant, and a tenant-less
// context failing closed. The closure returns a distinct id on every call,
// as the suite requires, via a plain counter.
func TestRepository_AssertIsolated(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	seq := 0
	tenancytest.AssertIsolated[InboxMessage](t, repo.Repository, func(_ pkgcore.TenantID) *InboxMessage {
		seq++
		return testMessage(fmt.Sprintf("inbox-%06d", seq))
	})
}

// TestRepository_Create_RoundTripsEveryColumn writes one message carrying a
// value in every column -- non-empty group and link, template parameters,
// an expiry, a dedupe key -- and reads it back field by field. The empty
// TenantModel doubles as an assertion that the read-back row reports the
// context's tenant, not an empty string.
func TestRepository_Create_RoundTripsEveryColumn(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	key := "delivery-note-42"
	expiry := time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)
	params := datatypes.JSON(`{"note_id":"note-42","shared_by":"user-3"}`)
	msg := &InboxMessage{
		ID:              "inbox-000001",
		RecipientUserID: "user-7",
		TypeKey:         "note.shared",
		Group:           "collaboration",
		Title:           "Note 42 was shared with you",
		Body:            "Lin opened Note 42 and shared it with the whole clinic.",
		Params:          params,
		Link:            "/notes/note-42",
		DedupeKey:       &key,
		ExpiryAt:        &expiry,
	}
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	if got.TenantID != "tenant-acme" {
		t.Errorf("TenantID = %q, want the context tenant %q", got.TenantID, "tenant-acme")
	}
	if got.RecipientUserID != "user-7" {
		t.Errorf("RecipientUserID = %q, want %q", got.RecipientUserID, "user-7")
	}
	if got.TypeKey != "note.shared" {
		t.Errorf("TypeKey = %q, want %q", got.TypeKey, "note.shared")
	}
	if got.Group != "collaboration" {
		t.Errorf("Group = %q, want %q", got.Group, "collaboration")
	}
	if got.Title != "Note 42 was shared with you" {
		t.Errorf("Title = %q, want %q", got.Title, "Note 42 was shared with you")
	}
	if got.Body != "Lin opened Note 42 and shared it with the whole clinic." {
		t.Errorf("Body = %q, want the message body", got.Body)
	}
	if string(got.Params) != string(params) {
		t.Errorf("Params = %s, want %s", got.Params, params)
	}
	if got.Link != "/notes/note-42" {
		t.Errorf("Link = %q, want %q", got.Link, "/notes/note-42")
	}
	if got.DedupeKey == nil || *got.DedupeKey != key {
		t.Errorf("DedupeKey = %v, want %q", got.DedupeKey, key)
	}
	if got.ExpiryAt == nil || !got.ExpiryAt.Equal(expiry) {
		t.Errorf("ExpiryAt = %v, want %v", got.ExpiryAt, expiry)
	}
	if got.ReadAt != nil {
		t.Errorf("ReadAt = %v, want nil for a freshly delivered message", got.ReadAt)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; autoCreateTime did not run")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero; autoUpdateTime did not run")
	}
}

// TestRepository_Create_NullableColumnsAndDefaults_RoundTrip writes the
// minimal message the delivery path produces -- no group, no parameters, no
// link, no dedupe key, nothing read or expired yet -- and pins that each
// nullable column comes back NULL (not an empty value) and each defaulted
// column comes back as its documented empty-string default.
func TestRepository_Create_NullableColumnsAndDefaults_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	msg := testMessage("inbox-000001")
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	if got.Group != "" {
		t.Errorf("Group = %q, want the empty-string default", got.Group)
	}
	if got.Link != "" {
		t.Errorf("Link = %q, want the empty-string default", got.Link)
	}
	if got.Params != nil {
		t.Errorf("Params = %s, want NULL", got.Params)
	}
	if got.DedupeKey != nil {
		t.Errorf("DedupeKey = %v, want NULL", got.DedupeKey)
	}
	if got.ExpiryAt != nil {
		t.Errorf("ExpiryAt = %v, want NULL", got.ExpiryAt)
	}
	if got.ReadAt != nil {
		t.Errorf("ReadAt = %v, want NULL", got.ReadAt)
	}
}

// TestRepository_DedupeKey_SameKeySameTenant_SecondCreateRejected proves the
// unique index turns a redelivered message into a duplicate-key error: the
// producer of a later block recomputes the same dedupe key for a retried
// delivery, and the rejection -- not a silent second row -- is what makes
// redelivery idempotent. The first message must survive untouched.
func TestRepository_DedupeKey_SameKeySameTenant_SecondCreateRejected(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	key := "delivery-note-42"
	first := testMessage("inbox-000001")
	first.DedupeKey = &key
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	second := testMessage("inbox-000002")
	second.DedupeKey = &key
	if err := repo.Create(ctx, second); err == nil {
		t.Fatal("Create(duplicate dedupe key) succeeded, want a duplicate-key error")
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1: the duplicate create must not leave a row behind", len(rows))
	}
	if rows[0].ID != first.ID {
		t.Errorf("surviving row id = %q, want the first message %q", rows[0].ID, first.ID)
	}
}

// TestRepository_DedupeKey_SameKeyOtherTenant_RejectedToo pins the global
// scope of uq_in_app_messages_dedupe_key. Keys are globally unique, not per
// tenant, because the derivation the producer ships folds the tenant in; a
// key that ever collides across tenants is therefore a derivation bug, and
// it must fail loudly here instead of silently swallowing one tenant's
// message.
func TestRepository_DedupeKey_SameKeyOtherTenant_RejectedToo(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	key := "delivery-note-42"
	first := testMessage("inbox-000001")
	first.DedupeKey = &key
	if err := repo.Create(tenantCtx("tenant-acme"), first); err != nil {
		t.Fatalf("Create(tenant-acme): %v", err)
	}

	second := testMessage("inbox-000002")
	second.DedupeKey = &key
	if err := repo.Create(tenantCtx("tenant-bright"), second); err == nil {
		t.Fatal("Create(same key, other tenant) succeeded, want a duplicate-key error")
	}

	rows, err := repo.List(tenantCtx("tenant-bright"))
	if err != nil {
		t.Fatalf("List(tenant-bright): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("tenant-bright List returned %d rows, want 0: the rejected create must not leave a row behind", len(rows))
	}
}

// TestRepository_DedupeKey_Nil_MultipleMessagesAllowed proves the counter
// half of the dedupe contract: a message without a dedupe key is never
// deduplicated. NULLs are distinct under the unique index on both dialects,
// so the delivery path that has no idempotency story to tell (a message
// whose duplicates are all legitimate, say) stays unlimited.
func TestRepository_DedupeKey_Nil_MultipleMessagesAllowed(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	for i := 1; i <= 3; i++ {
		if err := repo.Create(ctx, testMessage(fmt.Sprintf("inbox-%06d", i))); err != nil {
			t.Fatalf("Create(message %d, no dedupe key): %v", i, err)
		}
	}

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(rows))
	}
}

// TestRepository_SameRecipient_TwoTenants_HaveSeparateInboxes proves the
// inbox model of docs/internal/07: one person belongs to several tenants,
// and their rows form one inbox per tenant, isolated by tenant_id. The same
// recipient's message in the other tenant must be invisible to each List --
// a per-tenant roster that leaked across tenants would be an isolation
// defect, and one that mixed two tenants' messages into one list would be a
// correctness one.
func TestRepository_SameRecipient_TwoTenants_HaveSeparateInboxes(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	acmeMsg := testMessage("inbox-acme-1")
	acmeMsg.RecipientUserID = "user-9"
	if err := repo.Create(tenantCtx("tenant-acme"), acmeMsg); err != nil {
		t.Fatalf("Create(tenant-acme): %v", err)
	}
	brightMsg := testMessage("inbox-bright-1")
	brightMsg.RecipientUserID = "user-9"
	if err := repo.Create(tenantCtx("tenant-bright"), brightMsg); err != nil {
		t.Fatalf("Create(tenant-bright): %v", err)
	}

	assertInbox := func(tenant, wantID string) {
		t.Helper()
		rows, err := repo.List(tenantCtx(tenant))
		if err != nil {
			t.Fatalf("List(%s): %v", tenant, err)
		}
		if len(rows) != 1 {
			t.Fatalf("List(%s) returned %d rows, want 1", tenant, len(rows))
		}
		if rows[0].ID != wantID {
			t.Errorf("List(%s) returned %q, want %q", tenant, rows[0].ID, wantID)
		}
		if rows[0].RecipientUserID != "user-9" {
			t.Errorf("List(%s) recipient = %q, want %q", tenant, rows[0].RecipientUserID, "user-9")
		}
	}
	assertInbox("tenant-acme", acmeMsg.ID)
	assertInbox("tenant-bright", brightMsg.ID)
}

// TestRepository_Update_MarksMessageRead drives the read path of the inbox
// UI of a later block through the promoted Update: load the row, stamp
// ReadAt, write it back, and read again. A second write is what the inbox
// list marks read; the assertion that ReadAt was NULL before the update and
// equals the stamp after it is the whole behaviour.
func TestRepository_Update_MarksMessageRead(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	msg := testMessage("inbox-000001")
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.ReadAt != nil {
		t.Fatalf("ReadAt = %v, want nil before the message is opened", got.ReadAt)
	}

	readAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	got.ReadAt = &readAt
	if err = repo.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	again, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID after update: %v", err)
	}
	if again.ReadAt == nil || !again.ReadAt.Equal(readAt) {
		t.Errorf("ReadAt after update = %v, want %v", again.ReadAt, readAt)
	}
}

// TestRepository_FindByID_OtherTenant_ReportsRecordNotFound is the
// repository-level face of the collapse dbkit's ErrRecordNotFound documents:
// another tenant's message id must read exactly like a message that never
// existed -- dbkit.record_not_found, the code the example_test.go flow also
// shows -- never a row and never a distinct, more revealing error.
func TestRepository_FindByID_OtherTenant_ReportsRecordNotFound(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	msg := testMessage("inbox-000001")
	if err := repo.Create(tenantCtx("tenant-acme"), msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := repo.FindByID(tenantCtx("tenant-bright"), msg.ID)
	if err == nil {
		t.Fatal("FindByID(other tenant) succeeded, want dbkit.ErrRecordNotFound")
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("FindByID error %v is not an *apperr.Error", err)
	}
	if appErr.Code != dbkit.ErrRecordNotFound.Code {
		t.Errorf("error code = %q, want %q", appErr.Code, dbkit.ErrRecordNotFound.Code)
	}
}
