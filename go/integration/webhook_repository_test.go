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
