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

// messageAt returns a fresh inbox row for recipient with its CreatedAt
// pinned to at, so ordering tests do not depend on the clock's resolution.
// gorm's autoCreateTime fills CreatedAt only when it is zero, so a pinned
// value survives Create.
func messageAt(id, recipient, group string, at time.Time) *InboxMessage {
	msg := testMessage(id)
	msg.RecipientUserID = recipient
	msg.Group = group
	msg.CreatedAt = at
	return msg
}

// TestRepository_ListForRecipient_OwnRowsOnly_NewestFirst pins the two
// scoping axes and the ordering of the list operation: the page holds only
// recipientUserID's own rows in the tenant of ctx, newest first (created_at
// DESC with id DESC as the tiebreak). Another recipient's newer row and the
// same recipient's row in another tenant must both stay out of the page.
func TestRepository_ListForRecipient_OwnRowsOnly_NewestFirst(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	t1 := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)
	rows := []*InboxMessage{
		messageAt("inbox-a1", "user-7", "", t1),
		messageAt("inbox-a2", "user-7", "", t2),
		messageAt("inbox-a3", "user-7", "", t3),
		messageAt("inbox-a4", "user-9", "", t3.Add(time.Hour)), // newer, but user-9's
	}
	for _, row := range rows {
		if err := repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	if err := repo.Create(tenantCtx("tenant-bright"), messageAt("inbox-b1", "user-7", "", t3.Add(2*time.Hour))); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	got, err := repo.ListForRecipient(ctx, "user-7", "", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient: %v", err)
	}
	want := []string{"inbox-a3", "inbox-a2", "inbox-a1"}
	if len(got) != len(want) {
		t.Fatalf("ListForRecipient returned %d rows %v, want %d (%v)", len(got), idsOf(got), len(want), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("row %d = %q, want %q (newest first)", i, got[i].ID, id)
		}
	}
}

// idsOf is the string form of a row slice's ids, for failure messages.
func idsOf(rows []InboxMessage) []string {
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

// TestRepository_ListForRecipient_SameCreatedAt_IdDescTiebreak pins the
// tiebreak half of the ordering contract: two rows written in the same
// instant (the same created_at value) list deterministically by id DESC, so
// a page boundary can never split two same-instant rows ambiguously.
func TestRepository_ListForRecipient_SameCreatedAt_IdDescTiebreak(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	same := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"inbox-1", "inbox-2"} {
		if err := repo.Create(ctx, messageAt(id, "user-7", "", same)); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	got, err := repo.ListForRecipient(ctx, "user-7", "", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient: %v", err)
	}
	if len(got) != 2 || got[0].ID != "inbox-2" || got[1].ID != "inbox-1" {
		t.Errorf("ListForRecipient = %v, want [inbox-2 inbox-1]: same created_at must break ties by id DESC", idsOf(got))
	}
}

// TestRepository_ListForRecipient_ExpiredRowsStillListed pins the spec's
// line between listing and counting: expiry governs only the unread
// predicate (UnreadCount), never list membership -- an unread message whose
// expiry passed is still a row of the recipient's inbox, still listed, its
// expiry_at carried on the row so the rendering side drops the unread
// affordance itself.
func TestRepository_ListForRecipient_ExpiredRowsStillListed(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	expired := time.Now().UTC().Add(-time.Hour)
	msg := messageAt("inbox-expired", "user-7", "", time.Now().UTC())
	msg.ExpiryAt = &expired
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.ListForRecipient(ctx, "user-7", "", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient: %v", err)
	}
	if len(got) != 1 || got[0].ID != "inbox-expired" {
		t.Fatalf("ListForRecipient = %v, want the expired row still listed", idsOf(got))
	}
	if got[0].ExpiryAt == nil {
		t.Error("ExpiryAt is nil on the listed row; the response cannot show why it should render as expired")
	}
}

// TestRepository_ListForRecipient_GroupFilterAndPaging drives the listing's
// group restriction and its paging: group "" lists every group, a named
// group only its own rows, an unknown group an empty list (never an error),
// and limit/offset page the filtered result in the same newest-first order.
func TestRepository_ListForRecipient_GroupFilterAndPaging(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	rows := []*InboxMessage{
		messageAt("inbox-c1", "user-7", "collaboration", base.Add(1*time.Hour)),
		messageAt("inbox-c2", "user-7", "collaboration", base.Add(2*time.Hour)),
		messageAt("inbox-b1", "user-7", "billing", base.Add(3*time.Hour)),
		messageAt("inbox-x1", "user-7", "", base.Add(4*time.Hour)),
	}
	for _, row := range rows {
		if err := repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}

	collab, err := repo.ListForRecipient(ctx, "user-7", "collaboration", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient(collaboration): %v", err)
	}
	if want := []string{"inbox-c2", "inbox-c1"}; len(collab) != 2 || collab[0].ID != want[0] || collab[1].ID != want[1] {
		t.Errorf("collaboration page = %v, want %v", idsOf(collab), want)
	}

	unknown, err := repo.ListForRecipient(ctx, "user-7", "no.such.group", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient(unknown group): %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown group returned %d rows, want an empty list", len(unknown))
	}

	page1, err := repo.ListForRecipient(ctx, "user-7", "", 2, 0)
	if err != nil {
		t.Fatalf("ListForRecipient(page 1): %v", err)
	}
	page2, err := repo.ListForRecipient(ctx, "user-7", "", 2, 2)
	if err != nil {
		t.Fatalf("ListForRecipient(page 2): %v", err)
	}
	if want1, want2 := []string{"inbox-x1", "inbox-b1"}, []string{"inbox-c2", "inbox-c1"}; !equalIDs(page1, want1) || !equalIDs(page2, want2) {
		t.Errorf("pages = %v then %v, want %v then %v", idsOf(page1), idsOf(page2), want1, want2)
	}

	tail, err := repo.ListForRecipient(ctx, "user-7", "", 50, 8)
	if err != nil {
		t.Fatalf("ListForRecipient(past the end): %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("offset past the end returned %d rows, want an empty list", len(tail))
	}
}

// equalIDs reports whether rows' ids equal want, in order.
func equalIDs(rows []InboxMessage, want []string) bool {
	got := idsOf(rows)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestRepository_ListForRecipient_OtherTenant_Invisible completes the
// listing's isolation: the page is scoped by the tenant of ctx, so the same
// recipient's rows under another tenant never leak into it.
func TestRepository_ListForRecipient_OtherTenant_Invisible(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	if err := repo.Create(tenantCtx("tenant-acme"), messageAt("inbox-acme-1", "user-7", "", base)); err != nil {
		t.Fatalf("Create(acme): %v", err)
	}
	if err := repo.Create(tenantCtx("tenant-bright"), messageAt("inbox-bright-1", "user-7", "", base.Add(time.Hour))); err != nil {
		t.Fatalf("Create(bright): %v", err)
	}

	for _, tc := range []struct{ tenant, wantID string }{
		{"tenant-acme", "inbox-acme-1"},
		{"tenant-bright", "inbox-bright-1"},
	} {
		got, err := repo.ListForRecipient(tenantCtx(tc.tenant), "user-7", "", 50, 0)
		if err != nil {
			t.Fatalf("ListForRecipient(%s): %v", tc.tenant, err)
		}
		if len(got) != 1 || got[0].ID != tc.wantID {
			t.Errorf("ListForRecipient(%s) = %v, want only %s", tc.tenant, idsOf(got), tc.wantID)
		}
	}
}

// TestRepository_UnreadCount_AppliesTheUnreadPredicate pins the spec's
// unread predicate exactly: read_at still nil AND expiry_at -- when set --
// still in the future, judged against the server clock at query time.
// Read rows, expired-but-unread rows, rows of another recipient and rows of
// another tenant all stay out of the count; rows in the count vanish from it
// once marked read.
func TestRepository_UnreadCount_AppliesTheUnreadPredicate(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	now := time.Now().UTC()
	build := func(id string, markRead, expired bool) *InboxMessage {
		msg := messageAt(id, "user-7", "", now)
		if expired {
			past := now.Add(-time.Hour)
			msg.ExpiryAt = &past
		}
		if markRead {
			msg.ReadAt = &now
		}
		return msg
	}
	unreadFresh := build("inbox-unread", false, false)
	unreadExpired := build("inbox-unread-expired", false, true)
	readFresh := build("inbox-read", true, false)
	readExpired := build("inbox-read-expired", true, true)
	for _, row := range []*InboxMessage{unreadFresh, unreadExpired, readFresh, readExpired} {
		if err := repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	otherRecipient := messageAt("inbox-user9", "user-9", "", now)
	if err := repo.Create(ctx, otherRecipient); err != nil {
		t.Fatalf("Create(other recipient): %v", err)
	}
	if err := repo.Create(tenantCtx("tenant-bright"), messageAt("inbox-bright", "user-7", "", now)); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	got, err := repo.UnreadCount(ctx, "user-7")
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if got != 1 {
		t.Errorf("UnreadCount = %d, want 1: only the unread, unexpired row counts", got)
	}
}

// TestRepository_MarkRead_FlipsAndIsIdempotent drives the single mark-read
// contract: the first call stamps read_at, the second answers nil without
// touching the row, so the first read's timestamp survives as the read
// time.
func TestRepository_MarkRead_FlipsAndIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	msg := testMessage("inbox-000001")
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := time.Now().UTC()

	if err := repo.MarkRead(ctx, "user-7", msg.ID); err != nil {
		t.Fatalf("MarkRead(first): %v", err)
	}
	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.ReadAt == nil || got.ReadAt.Before(before) {
		t.Fatalf("ReadAt after first mark = %v, want a stamp at or after %v", got.ReadAt, before)
	}
	firstReadAt := *got.ReadAt

	if markErr := repo.MarkRead(ctx, "user-7", msg.ID); markErr != nil {
		t.Fatalf("MarkRead(second): %v", markErr)
	}
	again, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID after second mark: %v", err)
	}
	if again.ReadAt == nil || !again.ReadAt.Equal(firstReadAt) {
		t.Errorf("ReadAt after the idempotent second mark = %v, want the first stamp %v untouched", again.ReadAt, firstReadAt)
	}
}

// TestRepository_MarkRead_UnknownForeignAndOtherTenant_OneRefusal pins the
// collapse ErrMessageNotFound documents: an id that is unknown, that
// belongs to another recipient of the same tenant, or that belongs to
// another tenant altogether is the same refusal carrying the id -- one
// recipient can never learn whether another recipient's message id exists
// by probing it.
func TestRepository_MarkRead_UnknownForeignAndOtherTenant_OneRefusal(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)

	acmeMsg := testMessage("inbox-acme-1") // user-7's, in tenant-acme
	if err := repo.Create(tenantCtx("tenant-acme"), acmeMsg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	user9Msg := testMessage("inbox-acme-user9")
	user9Msg.RecipientUserID = "user-9"
	if err := repo.Create(tenantCtx("tenant-acme"), user9Msg); err != nil {
		t.Fatalf("Create(user-9's): %v", err)
	}
	brightMsg := testMessage("inbox-bright-1")
	if err := repo.Create(tenantCtx("tenant-bright"), brightMsg); err != nil {
		t.Fatalf("Create(tenant-bright): %v", err)
	}

	probe := func(name string, ctx context.Context, recipient, id string) {
		t.Helper()
		err := repo.MarkRead(ctx, recipient, id)
		if err == nil {
			t.Fatalf("%s: MarkRead succeeded, want ErrMessageNotFound", name)
		}
		appErr, ok := apperr.As(err)
		if !ok {
			t.Fatalf("%s: error %v is not an *apperr.Error", name, err)
		}
		if appErr.Code != ErrMessageNotFound.Code {
			t.Errorf("%s: code = %q, want %q", name, appErr.Code, ErrMessageNotFound.Code)
		}
		if appErr.Params["message_id"] != id {
			t.Errorf("%s: message_id param = %v, want %q", name, appErr.Params["message_id"], id)
		}
	}
	probe("unknown id", tenantCtx("tenant-acme"), "user-7", "inbox-nope")
	probe("other recipient's id", tenantCtx("tenant-acme"), "user-7", user9Msg.ID)
	probe("other tenant's id", tenantCtx("tenant-acme"), "user-7", brightMsg.ID)
	probe("right id, right recipient, other tenant", tenantCtx("tenant-bright"), "user-7", acmeMsg.ID)
}

// TestRepository_MarkRead_ExpiredRow_StillMarkable pins the other half of
// the expiry line: marking read is not gated on expiry either -- an unread
// message whose expiry passed is still the caller's own row, and marking it
// read is what keeps the unread predicate monotone.
func TestRepository_MarkRead_ExpiredRow_StillMarkable(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	expired := time.Now().UTC().Add(-time.Hour)
	msg := messageAt("inbox-expired", "user-7", "", expired)
	msg.ExpiryAt = &expired
	if err := repo.Create(ctx, msg); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.MarkRead(ctx, "user-7", msg.ID); err != nil {
		t.Fatalf("MarkRead(expired row): %v", err)
	}
	got, err := repo.FindByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.ReadAt == nil {
		t.Error("ReadAt is nil; the expired row was not marked read")
	}
}

// TestRepository_ReadAll_FlipsOnlyTheCallerOwnsUnreadAndCounts drives the
// mark-all-read contract: every row the recipient still holds unread --
// expired or not -- flips in one call, the answer is how many flipped, a
// recipient with nothing unread gets (0, nil), and neither another
// recipient's rows nor another tenant's rows move.
func TestRepository_ReadAll_FlipsOnlyTheCallerOwnsUnreadAndCounts(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-acme")

	now := time.Now().UTC()
	build := func(id string, markRead, expired bool) *InboxMessage {
		msg := messageAt(id, "user-7", "", now)
		if expired {
			past := now.Add(-time.Hour)
			msg.ExpiryAt = &past
		}
		if markRead {
			msg.ReadAt = &now
		}
		return msg
	}
	unreadFresh := build("inbox-unread", false, false)
	unreadExpired := build("inbox-unread-expired", false, true)
	readFresh := build("inbox-read", true, false)
	for _, row := range []*InboxMessage{unreadFresh, unreadExpired, readFresh} {
		if err := repo.Create(ctx, row); err != nil {
			t.Fatalf("Create(%s): %v", row.ID, err)
		}
	}
	otherRecipient := messageAt("inbox-user9-unread", "user-9", "", now)
	if err := repo.Create(ctx, otherRecipient); err != nil {
		t.Fatalf("Create(other recipient): %v", err)
	}
	if err := repo.Create(tenantCtx("tenant-bright"), messageAt("inbox-bright-unread", "user-7", "", now)); err != nil {
		t.Fatalf("Create(other tenant): %v", err)
	}

	flipped, err := repo.ReadAll(ctx, "user-7")
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if flipped != 2 {
		t.Errorf("ReadAll flipped %d rows, want 2: the unread rows, expired one included, and nothing already read", flipped)
	}

	got, err := repo.UnreadCount(ctx, "user-7")
	if err != nil {
		t.Fatalf("UnreadCount after ReadAll: %v", err)
	}
	if got != 0 {
		t.Errorf("UnreadCount after ReadAll = %d, want 0", got)
	}

	again, err := repo.ReadAll(ctx, "user-7")
	if err != nil {
		t.Fatalf("ReadAll(second): %v", err)
	}
	if again != 0 {
		t.Errorf("ReadAll(second) flipped %d rows, want 0", again)
	}

	user9Rows, err := repo.ListForRecipient(ctx, "user-9", "", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient(user-9): %v", err)
	}
	if len(user9Rows) != 1 || user9Rows[0].ReadAt != nil {
		t.Errorf("user-9's unread row was touched: %+v", user9Rows)
	}
	brightRows, err := repo.ListForRecipient(tenantCtx("tenant-bright"), "user-7", "", 50, 0)
	if err != nil {
		t.Fatalf("ListForRecipient(bright): %v", err)
	}
	if len(brightRows) != 1 || brightRows[0].ReadAt != nil {
		t.Errorf("tenant-bright's unread row was touched: %+v", brightRows)
	}
}
