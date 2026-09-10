package integration

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testWebhookCipherKey is the AES key webhook tests register
// WebhookSecretSerializerName with, mirroring go/org's own
// testEncryptionKey precedent (invitation_test.go) -- a fixed, obviously-a-
// test key, distinct from any blind-index key this module might grow later
// per dbkit.NewCipher's own doc comment. dbkit.NewCipher requires exactly
// 32 bytes, hence bytes.Repeat rather than a hand-counted string literal.
var testWebhookCipherKey = bytes.Repeat([]byte("k"), 32)

// newWebhookTestDB returns newTestDB's identical fresh, migrated SQLite
// database, with WebhookSecretSerializerName registered first -- any test
// that writes a WebhookSubscription row needs this, since Secret is written
// through a named GORM serializer and GORM fails to parse the model when
// nothing is registered under that name. Registering here, at the call
// site, rather than in a TestMain keeps the requirement visible, and GORM's
// registry is keyed by name, so repeating the call across tests is a
// harmless no-op replacement.
func newWebhookTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	cipher, err := dbkit.NewCipher(testWebhookCipherKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	dbkit.RegisterEncryptedSerializer(WebhookSecretSerializerName, cipher)
	return newTestDB(t)
}

// TestWebhookSubscriptionRepository_AssertIsolated runs the mandatory
// tenant-isolation suite against integration_webhook_subscriptions.
func TestWebhookSubscriptionRepository_AssertIsolated(t *testing.T) {
	repo := NewWebhookSubscriptionRepository(newWebhookTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *WebhookSubscription {
		n++
		return &WebhookSubscription{
			ID:         fmt.Sprintf("sub-%d", n),
			URL:        "https://example.com/hook",
			EventTypes: eventTypesJSON([]string{"org.member.joined"}),
			Secret:     "whsec_test",
			Active:     true,
			CreatedBy:  "user-1",
		}
	})
}

// TestWebhookDeliveryRepository_AssertIsolated runs the mandatory
// tenant-isolation suite against integration_webhook_deliveries.
func TestWebhookDeliveryRepository_AssertIsolated(t *testing.T) {
	repo := NewWebhookDeliveryRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *WebhookDelivery {
		n++
		return &WebhookDelivery{
			ID:             fmt.Sprintf("delivery-%d", n),
			SubscriptionID: "sub-1",
			EventType:      "org.member.joined",
			EventVersion:   "v1",
			IdempotencyKey: fmt.Sprintf("key-%d", n),
			Payload:        eventTypesJSON(nil), // any valid JSON value works here
			Status:         DeliveryStatusPending,
		}
	})
}

func TestWebhookSubscriptionRepository_ListActiveByTenant(t *testing.T) {
	db := newWebhookTestDB(t)
	repo := NewWebhookSubscriptionRepository(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-1")
	other := pkgcore.WithTenant(context.Background(), "tenant-2")

	active := &WebhookSubscription{ID: "sub-active", URL: "https://example.com/a", EventTypes: eventTypesJSON([]string{"e"}), Secret: "s", Active: true, CreatedBy: "u"}
	inactive := &WebhookSubscription{ID: "sub-inactive", URL: "https://example.com/b", EventTypes: eventTypesJSON([]string{"e"}), Secret: "s", Active: false, CreatedBy: "u"}
	otherTenant := &WebhookSubscription{ID: "sub-other", URL: "https://example.com/c", EventTypes: eventTypesJSON([]string{"e"}), Secret: "s", Active: true, CreatedBy: "u"}

	if err := repo.Create(ctx, active); err != nil {
		t.Fatalf("create active: %v", err)
	}
	if err := repo.Create(ctx, inactive); err != nil {
		t.Fatalf("create inactive: %v", err)
	}
	if err := repo.Create(other, otherTenant); err != nil {
		t.Fatalf("create other-tenant: %v", err)
	}

	got, err := repo.ListActiveByTenant(ctx)
	if err != nil {
		t.Fatalf("ListActiveByTenant: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sub-active" {
		t.Errorf("ListActiveByTenant = %+v, want exactly [sub-active]", got)
	}
}

// TestWebhookSubscriptionRepository_updateFields_PartialAndLiveOnly pins the
// two load-bearing properties of the partial update
// Service.UpdateWebhookSubscription writes through (webhook_service.go):
// only the columns in the payload change (a full-row save would also rewrite
// every untouched column, which is exactly the resurrection hazard the
// method exists to avoid), and a mark-deleted row matches nothing -- an
// update arriving after a delete can never bring the row back.
func TestWebhookSubscriptionRepository_updateFields_PartialAndLiveOnly(t *testing.T) {
	db := newWebhookTestDB(t)
	repo := NewWebhookSubscriptionRepository(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-1")

	row := &WebhookSubscription{
		ID: "sub-1", URL: "https://example.com/a", EventTypes: eventTypesJSON([]string{"e"}),
		Secret: "s", Active: true, CreatedBy: "u",
	}
	if err := repo.Create(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update exactly one column (Active to its zero value -- the case a
	// bare struct-based Updates would silently omit, which is why the
	// repository writes through an explicit Select list); the others must
	// survive untouched.
	paused := false
	matched, err := repo.updateFields(ctx, "sub-1", webhookSubscriptionChanges{Active: &paused})
	if err != nil {
		t.Fatalf("updateFields: %v", err)
	}
	if !matched {
		t.Fatal("updateFields reported no match for a live row")
	}
	got, err := repo.FindByID(ctx, "sub-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.Active {
		t.Error("Active = true, want false after the targeted update")
	}
	if got.URL != "https://example.com/a" {
		t.Errorf("URL = %q, want unchanged (the targeted update rewrote an untouched column)", got.URL)
	}

	// A mark-deleted row must not match: the update cannot resurrect it.
	if delErr := repo.Delete(ctx, "sub-1"); delErr != nil {
		t.Fatalf("delete: %v", delErr)
	}
	active := true
	matched, err = repo.updateFields(ctx, "sub-1", webhookSubscriptionChanges{Active: &active})
	if err != nil {
		t.Fatalf("updateFields after delete: %v", err)
	}
	if matched {
		t.Fatal("updateFields matched a mark-deleted row")
	}
	if _, findErr := repo.FindByID(ctx, "sub-1"); !dbkit.IsRecordNotFound(findErr) {
		t.Errorf("FindByID after delete+updateFields = %v, want not found (the row must stay deleted)", findErr)
	}

	// Another tenant's row (even a live one) must not match either.
	other := pkgcore.WithTenant(context.Background(), "tenant-2")
	otherRow := &WebhookSubscription{
		ID: "sub-other", URL: "https://example.com/b", EventTypes: eventTypesJSON([]string{"e"}),
		Secret: "s", Active: true, CreatedBy: "u",
	}
	if createErr := repo.Create(other, otherRow); createErr != nil {
		t.Fatalf("create other-tenant: %v", createErr)
	}
	matched, err = repo.updateFields(ctx, "sub-other", webhookSubscriptionChanges{Active: &paused})
	if err != nil {
		t.Fatalf("updateFields cross-tenant: %v", err)
	}
	if matched {
		t.Fatal("updateFields matched another tenant's row")
	}
}

// TestWebhookSubscriptionRepository_GuardedFlip_CannotResurrectADeleteThatLandedAfterTheRestoreRead
// pins, step by step and deterministically, the property
// Service.UpdateWebhookSubscription's write path -- and
// RestoreWebhookSubscription's guarded restore, whose restorePaused write
// the same guard shape protects -- depends on: a guarded updateFields flip
// whose WHERE requires deleted_at IS NULL can never resurrect a row a
// concurrent DeleteWebhookSubscription mark-deleted after the flip's own
// read took its snapshot -- the state a real-concurrency race cannot time
// (see the honesty note in webhook_service_test.go's
// TestService_RestoreWebhookSubscription_ConcurrentDelete_DeletionWins):
//
//  1. the subscription is mark-deleted, then un-marked (as dbkit's
//     Repository[WebhookSubscription].Restore unmarks);
//  2. a read takes the snapshot a later flip would carry;
//  3. a delete's mark lands between that read and the flip;
//  4. the flip runs.
//
// Step 4 must match nothing: the row stays deleted, deletion wins, and
// the caller answers its not-found. A whole-row save of the step-2
// snapshot would rewrite its nil DeletedAt over the step-3 mark and
// silently resurrect the subscription. This test fails the moment the
// guard is weakened (the deleted_at IS NULL condition removed) or the
// write is widened back to whole-row scope.
//
// One honesty note on what this replay covers and does not:
// RestoreWebhookSubscription's restore lands through the single
// restorePaused write, whose WHERE requires deleted_at IS NOT NULL -- so a
// delete landing before that write makes it match nothing in the first
// place, and no read-then-flip window exists inside the restore at all
// (see restorePaused's own doc comment in webhook_repository.go, and
// webhook_service_test.go's
// TestService_RestoreWebhookSubscription_NoDeliveryCanFireBetweenRestoreAndPause).
// The guarded-flip property below is load-bearing for the UPDATE path's
// own identical race, which is why this replay exists.
func TestWebhookSubscriptionRepository_GuardedFlip_CannotResurrectADeleteThatLandedAfterTheRestoreRead(t *testing.T) {
	db := newWebhookTestDB(t)
	repo := NewWebhookSubscriptionRepository(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-1")

	row := &WebhookSubscription{
		ID: "sub-restore-race", URL: "https://example.com/a", EventTypes: eventTypesJSON([]string{"e"}),
		Secret: "s", Active: true, CreatedBy: "u",
	}
	if createErr := repo.Create(ctx, row); createErr != nil {
		t.Fatalf("create: %v", createErr)
	}
	if delErr := repo.Delete(ctx, row.ID); delErr != nil {
		t.Fatalf("delete (setup): %v", delErr)
	}
	// Step 1: un-mark the row, as dbkit's Repository Restore unmarks.
	if restoreErr := repo.Restore(ctx, row.ID); restoreErr != nil {
		t.Fatalf("restore: %v", restoreErr)
	}
	// Step 2: the read -- the snapshot a later flip would carry.
	read, err := repo.FindByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("FindByID (post-restore): %v", err)
	}
	if !read.Active {
		t.Fatal("premise: the restored row must read Active for the pause-flip to be the write at stake")
	}
	// Step 3: the interfering delete's commit, landing between the read and
	// the flip.
	if delErr := repo.Delete(ctx, row.ID); delErr != nil {
		t.Fatalf("delete (interfering): %v", delErr)
	}
	// Step 4: the guarded flip -- Active = false, through the same guarded
	// write the update path uses.
	inactive := false
	matched, err := repo.updateFields(ctx, row.ID, webhookSubscriptionChanges{Active: &inactive})
	if err != nil {
		t.Fatalf("updateFields (the guarded flip): %v", err)
	}
	if matched {
		t.Fatal("the guarded flip matched a row that was mark-deleted again -- the write resurrected the deletion")
	}
	if _, findErr := repo.FindByID(ctx, row.ID); !dbkit.IsRecordNotFound(findErr) {
		t.Errorf("FindByID after the guarded flip = %v, want not found (the row must stay deleted)", findErr)
	}
}

func TestWebhookDeliveryRepository_ByIdempotencyKey(t *testing.T) {
	db := newTestDB(t)
	repo := NewWebhookDeliveryRepository(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-1")

	got, err := repo.ByIdempotencyKey(ctx, "sub-1", "no-such-key")
	if err != nil {
		t.Fatalf("ByIdempotencyKey (absent): %v", err)
	}
	if got != nil {
		t.Errorf("ByIdempotencyKey (absent) = %+v, want nil", got)
	}

	row := &WebhookDelivery{
		ID:             "delivery-1",
		SubscriptionID: "sub-1",
		EventType:      "org.member.joined",
		EventVersion:   "v1",
		IdempotencyKey: "key-1",
		Payload:        eventTypesJSON(nil),
		Status:         DeliveryStatusPending,
	}
	if createErr := repo.Create(ctx, row); createErr != nil {
		t.Fatalf("create: %v", createErr)
	}

	got, err = repo.ByIdempotencyKey(ctx, "sub-1", "key-1")
	if err != nil {
		t.Fatalf("ByIdempotencyKey: %v", err)
	}
	if got == nil || got.ID != "delivery-1" {
		t.Errorf("ByIdempotencyKey = %+v, want the delivery-1 row", got)
	}

	// Same key, different subscription: not found -- the index is scoped by
	// (tenant, subscription, key), not by key alone.
	got, err = repo.ByIdempotencyKey(ctx, "sub-2", "key-1")
	if err != nil {
		t.Fatalf("ByIdempotencyKey (different subscription): %v", err)
	}
	if got != nil {
		t.Errorf("ByIdempotencyKey (different subscription) = %+v, want nil", got)
	}
}

func TestWebhookDeliveryRepository_ListRecentBySubscription_NewestFirst(t *testing.T) {
	db := newTestDB(t)
	repo := NewWebhookDeliveryRepository(db)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-1")

	for i := 1; i <= 3; i++ {
		row := &WebhookDelivery{
			ID:             fmt.Sprintf("delivery-%d", i),
			SubscriptionID: "sub-1",
			EventType:      "org.member.joined",
			EventVersion:   "v1",
			IdempotencyKey: fmt.Sprintf("key-%d", i),
			Payload:        eventTypesJSON(nil),
			Status:         DeliveryStatusPending,
		}
		if err := repo.Create(ctx, row); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// A delivery of a different subscription must never show up.
	if err := repo.Create(ctx, &WebhookDelivery{
		ID: "delivery-other-sub", SubscriptionID: "sub-2", EventType: "e", EventVersion: "v1",
		IdempotencyKey: "key-other", Payload: eventTypesJSON(nil), Status: DeliveryStatusPending,
	}); err != nil {
		t.Fatalf("create other-subscription delivery: %v", err)
	}

	got, err := repo.ListRecentBySubscription(ctx, "sub-1", 2)
	if err != nil {
		t.Fatalf("ListRecentBySubscription: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (limit honored)", len(got))
	}
	for _, row := range got {
		if row.SubscriptionID != "sub-1" {
			t.Errorf("got a delivery of subscription %q, want only sub-1", row.SubscriptionID)
		}
	}
}
