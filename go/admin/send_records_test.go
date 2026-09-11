package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/go/admin/api"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
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

// TestSendRecordSearchService_SingleTenant_Filters pins the single-tenant
// path: a real SendRecordRepository row, written and read back
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

// TestSendRecordSearchService_SingleTenant_ReadIsAuditedSystemContext
// pins the single-tenant path's audited wrapper: even the NAMED-tenant
// path of the send-record search must take tenancy.WithSystemContext's
// audited wrapper, exactly like AuditService.Query's own single-tenant
// read -- a platform operator reading one tenant's send records is still
// a cross-tenant read of platform data, and it must leave the same
// tenancy.system_context.entered audit trail (who read which tenant's
// records, under which declared purpose) every other admin cross-tenant
// read does.
func TestSendRecordSearchService_SingleTenant_ReadIsAuditedSystemContext(t *testing.T) {
	env := buildTestAdminModule(t)
	repo := notification.NewSendRecordRepository(env.DB)

	const tenant = "tenant-send-record-single-audited"
	insertTestSendRecord(t, repo, tenant, "sr-1", notification.ChannelEmail, notification.SendRecordStatusSucceeded, "key-1")

	var entered []tenancy.SystemContextEnteredEvent
	env.Registry.EventBus().Subscribe(tenancy.EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		var e tenancy.SystemContextEnteredEvent
		if err := pkgcore.DecodeEventPayload(evt.Payload, &e); err != nil {
			return err
		}
		entered = append(entered, e)
		return nil
	})

	svc := NewSendRecordSearchService(env.Notification.Deliveries(), env.Admin.Tenants())
	svc.attach(env.Registry.EventBus())

	got, err := svc.Query(context.Background(), "operator-audited-1", tenant, notification.SendRecordFilter{Limit: 50})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != "sr-1" {
		t.Fatalf("Query(tenantId=%s) = %+v, want exactly sr-1", tenant, got)
	}
	if len(entered) != 1 {
		t.Fatalf("single-tenant Query() published %d tenancy.system_context.entered events, want exactly 1 -- the read must take the audited D2 wrapper, never a direct untrailed read", len(entered))
	}
	if entered[0].Actor != "operator-audited-1" || entered[0].Purpose != SystemPurposeAdminCrossTenant {
		t.Fatalf("system-context event = %+v, want Actor operator-audited-1 under %s", entered[0], SystemPurposeAdminCrossTenant)
	}
}

// TestSendRecordSearchService_CrossTenant_SearchesEveryLedgerTenant pins
// the cross-tenant path: with no tenantID named, every tenant in admin's
// own ledger is searched, and rows from a tenant NOT in the ledger are
// correctly invisible (the ledger, not send_records itself, is what
// bounds the cross-tenant search space).
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
	// search, since the search draws its candidate tenants from admin's
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

// TestHandler_AdminListSendRecords_FiltersLimitAndWireShape_OverHTTP
// drives the send-record search through its real route: a status
// filter narrows to the one failed external-contact delivery (whose wire
// shape carries the contact id and the error text, with no recipient
// user id), a channel+status+limit+offset combination narrows to the
// single matching row (with the recipient user id and idempotency key
// present), and an unfiltered read answers the frozen default limit of
// 50 records -- never the whole table -- from a 57-row fixture.
func TestHandler_AdminListSendRecords_FiltersLimitAndWireShape_OverHTTP(t *testing.T) {
	env := buildTestAdminModule(t)
	repo := notification.NewSendRecordRepository(env.DB)

	const tenant = "tenant-send-record-http"
	for i := 0; i < 55; i++ {
		insertTestSendRecord(t, repo, tenant, fmt.Sprintf("sr-http-%02d", i), notification.ChannelEmail, notification.SendRecordStatusSucceeded, fmt.Sprintf("key-http-%02d", i))
	}
	ext := &notification.SendRecord{
		ID:             "sr-http-ext",
		TenantID:       tenant,
		TypeKey:        "admin_test.type",
		Channel:        notification.ChannelEmail,
		RecipientClass: notification.RecipientClassExternal,
		ContactID:      "contact-http-1",
		Status:         notification.SendRecordStatusFailed,
		Error:          "provider refused",
		IdempotencyKey: "key-http-ext",
	}
	if err := repo.Create(context.Background(), ext); err != nil {
		t.Fatalf("insert external send record fixture: %v", err)
	}
	insertTestSendRecord(t, repo, tenant, "sr-http-sms", notification.ChannelSMS, notification.SendRecordStatusSkipped, "key-http-sms")

	req := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		return r.WithContext(authn.WithPrincipal(r.Context(), authn.Principal{UserID: "operator-send-record-http"}))
	}

	// The failed external delivery, filtered by status alone.
	w := httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/notifications/send-records?tenantId="+tenant+"&status=failed"))
	if w.Code != http.StatusOK {
		t.Fatalf("status-filter status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var failed api.AdminListSendRecordsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &failed); err != nil {
		t.Fatalf("decode status-filter response: %v", err)
	}
	if len(failed.Records) != 1 || failed.Records[0].ID != "sr-http-ext" {
		t.Fatalf("status=failed = %+v, want exactly the one failed external delivery", failed.Records)
	}
	rec := failed.Records[0]
	if rec.ContactID == nil || *rec.ContactID != "contact-http-1" {
		t.Fatalf("failed record ContactID = %v, want contact-http-1 on the wire", rec.ContactID)
	}
	if rec.Error == nil || *rec.Error != "provider refused" {
		t.Fatalf("failed record Error = %v, want the recorded error text on the wire", rec.Error)
	}
	if rec.RecipientUserID != nil {
		t.Fatalf("failed record RecipientUserID = %v, want absent -- an external-contact delivery names no user", rec.RecipientUserID)
	}

	// Channel+status+limit+offset: the one skipped SMS row.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/notifications/send-records?tenantId="+tenant+"&channel=sms&status=skipped&limit=3&offset=0"))
	if w.Code != http.StatusOK {
		t.Fatalf("channel-filter status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var sms api.AdminListSendRecordsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sms); err != nil {
		t.Fatalf("decode channel-filter response: %v", err)
	}
	if len(sms.Records) != 1 || sms.Records[0].ID != "sr-http-sms" {
		t.Fatalf("channel=sms&status=skipped = %+v, want exactly the skipped SMS row", sms.Records)
	}
	smsRec := sms.Records[0]
	if smsRec.RecipientUserID == nil || *smsRec.RecipientUserID != "user-send-record-flow" {
		t.Fatalf("SMS record RecipientUserID = %v, want the recipient user on the wire", smsRec.RecipientUserID)
	}
	if smsRec.IdempotencyKey == nil || *smsRec.IdempotencyKey != "key-http-sms" {
		t.Fatalf("SMS record IdempotencyKey = %v, want key-http-sms on the wire", smsRec.IdempotencyKey)
	}
	if smsRec.Error != nil || smsRec.ContactID != nil {
		t.Fatalf("SMS record = %+v, want no error and no contact on a skipped user delivery", smsRec)
	}

	// Unfiltered: the default limit of 50 caps the 57-row fixture.
	w = httptest.NewRecorder()
	env.Admin.handler.ServeHTTP(w, req("/api/v1/admin/notifications/send-records?tenantId="+tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("unfiltered status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
	var all api.AdminListSendRecordsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode unfiltered response: %v", err)
	}
	if len(all.Records) != 50 {
		t.Fatalf("unfiltered search = %d records, want the default limit of 50 from a 57-row table", len(all.Records))
	}
}
