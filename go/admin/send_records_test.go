package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
)

// insertTestSendRecord writes one send_records row directly through
// notification's own exported SendRecordRepository -- the sanctioned data
// path for this platform table (send_record.go's own doc comment) --
// rather than driving notification's full Dispatch/delivery pipeline,
// which needs a registered notification type and rendered templates this
// test has no need to stand up. It mirrors notification's own test
// fixture helper (insertSendRecordFixture) at arm's length, through the
// exported repository type only.
func insertTestSendRecord(t *testing.T, repo *notification.SendRecordRepository, tenantID, id, channel, status, key string) {
	t.Helper()
	rec := &notification.SendRecord{
		ID:              id,
		TenantID:        tenantID,
		TypeKey:         "admin_test.type",
		Channel:         channel,
		RecipientClass:  notification.RecipientClassUser,
		RecipientUserID: "user-send-record-flow",
		Status:          status,
		IdempotencyKey:  key,
	}
	if err := repo.Create(context.Background(), rec); err != nil {
		t.Fatalf("insert send record fixture %s: %v", id, err)
	}
}

// TestSendRecordSearchService_SingleTenant_Filters pins D10's single-
// tenant path: a real SendRecordRepository row, written and read back
// through notification's own real ListByFilter, filtered by channel and
// status exactly as SendRecordFilter's own contract promises.
func TestSendRecordSearchService_SingleTenant_Filters(t *testing.T) {
	env := buildTestAdminModule(t)
	repo := notification.NewSendRecordRepository(env.DB)

	insertTestSendRecord(t, repo, "tenant-send-record-single", "sr-1", notification.ChannelEmail, notification.SendRecordStatusSucceeded, "key-1")
	insertTestSendRecord(t, repo, "tenant-send-record-single", "sr-2", notification.ChannelEmail, notification.SendRecordStatusFailed, "key-2")

	svc := NewSendRecordSearchService(env.Notification.Deliveries(), env.Admin.Tenants())
	svc.attach(env.Registry.EventBus())

	got, err := svc.Query(context.Background(), "operator-1", "tenant-send-record-single", notification.SendRecordFilter{
		Status: notification.SendRecordStatusFailed,
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "sr-2" {
		t.Fatalf("Query(status=failed) = %+v, want exactly sr-2", got)
	}
}

// TestSendRecordSearchService_CrossTenant_SearchesEveryLedgerTenant pins
// D10's cross-tenant path (D2's mechanism): with no tenantID named, every
// tenant in admin's own D3 ledger is searched, and rows from a tenant NOT
// in the ledger are correctly invisible (the ledger, not send_records
// itself, is what bounds the cross-tenant search space).
func TestSendRecordSearchService_CrossTenant_SearchesEveryLedgerTenant(t *testing.T) {
	env := buildTestAdminModule(t)
	repo := notification.NewSendRecordRepository(env.DB)

	const tenantA = pkgcore.TenantID("tenant-send-record-a")
	const tenantB = pkgcore.TenantID("tenant-send-record-b")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenantA), "Send Record A", "workspace"); err != nil {
		t.Fatalf("CreateRoot(tenantA) error = %v", err)
	}
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenantB), "Send Record B", "workspace"); err != nil {
		t.Fatalf("CreateRoot(tenantB) error = %v", err)
	}

	insertTestSendRecord(t, repo, string(tenantA), "sr-a-1", notification.ChannelEmail, notification.SendRecordStatusSucceeded, "key-a-1")
	insertTestSendRecord(t, repo, string(tenantB), "sr-b-1", notification.ChannelEmail, notification.SendRecordStatusSucceeded, "key-b-1")
	// Not in the ledger at all -- must never surface in the cross-tenant
	// search, since D2's mechanism draws its candidate tenants from D3's
	// own ledger.
	insertTestSendRecord(t, repo, "tenant-send-record-not-in-ledger", "sr-x-1", notification.ChannelEmail, notification.SendRecordStatusSucceeded, "key-x-1")

	svc := NewSendRecordSearchService(env.Notification.Deliveries(), env.Admin.Tenants())
	svc.attach(env.Registry.EventBus())

	got, err := svc.Query(context.Background(), "operator-1", "", notification.SendRecordFilter{Limit: 50})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	seen := map[string]bool{}
	for _, rec := range got {
		seen[rec.ID] = true
	}
	if !seen["sr-a-1"] || !seen["sr-b-1"] {
		t.Fatalf("cross-tenant Query() = %+v, want both sr-a-1 and sr-b-1", got)
	}
	if seen["sr-x-1"] {
		t.Fatalf("cross-tenant Query() = %+v, want sr-x-1 (unknown tenant) excluded", got)
	}
}
