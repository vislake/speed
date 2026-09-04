package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeQueue is a minimal jobs.Queue recording every Enqueue call, for tests
// that need to observe what handleDomainEvent submits without running a
// real jobs.StandaloneQueue worker pool.
type fakeQueue struct {
	tasks []jobs.Task
}

func (q *fakeQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.tasks = append(q.tasks, task)
	return jobs.JobID("fake-job"), nil
}

func (q *fakeQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	return nil, errors.New("fakeQueue.Get is not implemented")
}

func (q *fakeQueue) Cancel(context.Context, jobs.JobID) error { return nil }

var _ jobs.Queue = (*fakeQueue)(nil)

// createTestSubscription creates one active webhook subscription for
// testTenant subscribed to testMapping.PublicType, returning the subscription
// id and its raw signing secret.
func createTestSubscription(t *testing.T, svc *Service, url string) (id, secret string) {
	t.Helper()
	created, err := svc.CreateWebhookSubscription(ctxFor(testTenant), CreateWebhookSubscriptionInput{
		URL:        url,
		EventTypes: []string{testMapping.PublicType},
		CreatedBy:  "user-1",
	})
	if err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}
	return created.ID, created.Secret
}

func TestService_handleDomainEvent_UnmappedType_Ignored(t *testing.T) {
	_, svc := newWebhookTestService(t)
	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: "no.such.mapping"}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil (unmapped types are silently ignored)", err)
	}
}

func TestService_handleDomainEvent_NoTenant_Skipped(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	createTestSubscription(t, svc, "https://example.com/hook")

	if err := svc.handleDomainEvent(context.Background(), pkgcore.Event{Type: testMapping.InternalType}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (no tenant, nothing to fan out to)", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_NoMatchingSubscription_NoOp(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	// No subscription created at all.
	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_InactiveSubscription_NeverFannedOut(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	id, _ := createTestSubscription(t, svc, "https://example.com/hook")
	inactive := false
	if _, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: id, Active: &inactive}); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Errorf("handleDomainEvent = %v, want nil", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (inactive subscription must never be fanned out to)", len(fq.tasks))
	}
}

func TestService_handleDomainEvent_CreatesDeliveryRowAndEnqueuesJob(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Fatalf("handleDomainEvent: %v", err)
	}

	if len(fq.tasks) != 1 {
		t.Fatalf("len(fq.tasks) = %d, want 1", len(fq.tasks))
	}
	task := fq.tasks[0]
	if task.Type != jobTypeWebhookDeliver {
		t.Errorf("task.Type = %q, want %q", task.Type, jobTypeWebhookDeliver)
	}
	if string(task.TenantID) != string(testTenant) {
		t.Errorf("task.TenantID = %q, want %q", task.TenantID, testTenant)
	}
	if task.IdempotencyKey == "" {
		t.Error("task.IdempotencyKey is empty")
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != DeliveryStatusPending {
		t.Errorf("Status = %q, want %q", deliveries[0].Status, DeliveryStatusPending)
	}
	if deliveries[0].EventType != testMapping.PublicType || deliveries[0].EventVersion != testMapping.PublicVersion {
		t.Errorf("EventType/EventVersion = %s/%s, want %s/%s",
			deliveries[0].EventType, deliveries[0].EventVersion, testMapping.PublicType, testMapping.PublicVersion)
	}
}

// TestService_handleDomainEvent_Redelivery_IsIdempotent proves the fan-out
// dedupe: an at-least-once redelivered domain event (the same Type, tenant
// and payload) creates exactly one WebhookDelivery row, never two.
func TestService_handleDomainEvent_Redelivery_IsIdempotent(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	evt := pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (1): %v", err)
	}
	if err := svc.handleDomainEvent(ctxFor(testTenant), evt); err != nil {
		t.Fatalf("handleDomainEvent (2, redelivery): %v", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d after two identical events, want 1", len(deliveries))
	}
}

func TestService_handleDeliveryJob_Success_SignsAndDelivers(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, withHTTPClient(srv.Client()))
	subID, secret := createTestSubscription(t, svc, srv.URL)

	delivery := createPendingDelivery(t, svc, subID)

	result, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob: %v", err)
	}
	_ = result

	if gotBody == nil {
		t.Fatal("the webhook receiver never got a request")
	}
	if string(gotBody) != string(delivery.Payload) {
		t.Errorf("body = %s, want %s", gotBody, delivery.Payload)
	}
	if gotHeaders.Get(HeaderWebhookID) != delivery.ID {
		t.Errorf("%s = %q, want %q", HeaderWebhookID, gotHeaders.Get(HeaderWebhookID), delivery.ID)
	}
	ts, err := strconv.ParseInt(gotHeaders.Get(HeaderWebhookTimestamp), 10, 64)
	if err != nil {
		t.Fatalf("parse %s: %v", HeaderWebhookTimestamp, err)
	}
	wantSig := signWebhookPayload(secret, ts, gotBody)
	if gotHeaders.Get(HeaderWebhookSignature) != wantSig {
		t.Errorf("%s = %q, want %q", HeaderWebhookSignature, gotHeaders.Get(HeaderWebhookSignature), wantSig)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDelivered {
		t.Fatalf("deliveries = %+v, want exactly one Delivered row", deliveries)
	}
	if deliveries[0].DeliveredAt == nil {
		t.Error("DeliveredAt is nil after a successful delivery")
	}
	if deliveries[0].LastStatusCode == nil || *deliveries[0].LastStatusCode != http.StatusOK {
		t.Errorf("LastStatusCode = %v, want 200", deliveries[0].LastStatusCode)
	}
}

func TestService_handleDeliveryJob_ReceiverError_MarksFailedAndRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, withHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	delivery := createPendingDelivery(t, svc, subID)

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err == nil {
		t.Fatal("handleDeliveryJob = nil error, want a retryable failure for a 500 response")
	}

	deliveries, listErr := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if listErr != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", listErr)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusFailed {
		t.Fatalf("deliveries = %+v, want exactly one Failed row", deliveries)
	}
	if deliveries[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", deliveries[0].Attempts)
	}
	if deliveries[0].LastError == "" {
		t.Error("LastError is empty after a failed attempt")
	}
}

func TestService_handleDeliveryJob_SubscriptionDeleted_TerminatesWithoutRetry(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), subID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a deleted subscription is terminal, not retried)", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

// TestService_handleDeliveryJob_SubscriptionMarkDeleted_DeliveryRowUnaffected
// is this round's own proof for the mark-delete adoption: a WebhookDelivery
// enqueued before its subscription was mark-deleted settles exactly as
// TestService_handleDeliveryJob_SubscriptionDeleted_TerminatesWithoutRetry
// already proves for the identical scenario (that test is unaffected by
// this round precisely because it exercises the ordinary Service.
// DeleteWebhookSubscription call, which is now a mark-delete) -- and this
// test additionally reaches under the Service to confirm WHY: the
// subscription row still physically exists, mark-deleted, and the delivery
// row -- an id reference only, per this module's own no-cross-table-FK
// discipline (webhook_model.go's WebhookDelivery.SubscriptionID doc
// comment) -- is completely untouched by the subscription's own
// soft-delete columns, since WebhookDelivery does not implement
// dbkit.SoftDeletable at all.
func TestService_handleDeliveryJob_SubscriptionMarkDeleted_DeliveryRowUnaffected(t *testing.T) {
	m, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), subID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}

	// The subscription row is mark-deleted, not gone.
	var subCount int64
	if err := dbkit.WithTenantSession(ctxFor(testTenant), m.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Model(&WebhookSubscription{}).
			Where("id = ? AND deleted_at IS NOT NULL", subID).
			Count(&subCount).Error
	}); err != nil {
		t.Fatalf("counting the mark-deleted subscription row: %v", err)
	}
	if subCount != 1 {
		t.Fatalf("mark-deleted subscription row count = %d, want exactly 1", subCount)
	}

	// The delivery row is untouched: no deleted_at/deleted_by columns exist
	// on WebhookDelivery at all, and its own fields are exactly what
	// createPendingDelivery produced.
	var deliveryRow WebhookDelivery
	if err := dbkit.WithTenantSession(ctxFor(testTenant), m.db, func(tx *gorm.DB) error {
		return tx.Unscoped().Where("id = ?", delivery.ID).First(&deliveryRow).Error
	}); err != nil {
		t.Fatalf("reading the delivery row back: %v", err)
	}
	if deliveryRow.Status != DeliveryStatusPending {
		t.Errorf("delivery Status = %q, want unchanged %q", deliveryRow.Status, DeliveryStatusPending)
	}
	if deliveryRow.SubscriptionID != subID {
		t.Errorf("delivery SubscriptionID = %q, want unchanged %q", deliveryRow.SubscriptionID, subID)
	}

	// The already-enqueued job still settles terminal without retrying,
	// exactly as it did before this round -- handleDeliveryJob's own
	// FindByID lookup on the now mark-deleted subscription is hidden from
	// it by dbkit's soft-delete auto-scope plugin exactly as a physical
	// DELETE always hid it before.
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID)); err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a mark-deleted subscription is terminal, not retried)", err)
	}
	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

func TestService_handleDeliveryJob_SubscriptionInactive_TerminatesWithoutRetry(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	inactive := false
	if _, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: subID, Active: &inactive}); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}

	_, err := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(delivery.ID, subID))
	if err != nil {
		t.Fatalf("handleDeliveryJob = %v, want nil (a paused subscription is terminal, not retried)", err)
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
}

func TestService_handleDeliveryJob_AlreadyDelivered_NoOp(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, withHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	delivery := createPendingDelivery(t, svc, subID)

	job := deliveryJob(delivery.ID, subID)
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), job); err != nil {
		t.Fatalf("first handleDeliveryJob: %v", err)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("requestCount after the first attempt = %d, want 1", got)
	}

	// A retried job for an already-succeeded delivery must not re-send.
	if _, err := svc.handleDeliveryJob(ctxFor(testTenant), job); err != nil {
		t.Fatalf("second handleDeliveryJob: %v", err)
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("requestCount after the second (replayed) attempt = %d, want still 1 -- an already-delivered delivery must not be re-sent", got)
	}
}

func TestService_onWebhookDeliveryDeadLetter_MarksDeadLetter(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	delivery := createPendingDelivery(t, svc, subID)

	svc.onWebhookDeliveryDeadLetter(ctxFor(testTenant), deliveryJob(delivery.ID, subID), errors.New("exhausted"))

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDeadLetter {
		t.Fatalf("deliveries = %+v, want exactly one DeadLetter row", deliveries)
	}
	if deliveries[0].LastError != "exhausted" {
		t.Errorf("LastError = %q, want %q", deliveries[0].LastError, "exhausted")
	}
}

func TestDeriveWebhookDeliveryKey_DeterministicAndDistinct(t *testing.T) {
	body := []byte(`{"a":1}`)
	k1 := deriveWebhookDeliveryKey("sub-1", "type.a", "v1", body)
	k2 := deriveWebhookDeliveryKey("sub-1", "type.a", "v1", body)
	if k1 != k2 {
		t.Error("deriveWebhookDeliveryKey is not deterministic for identical inputs")
	}
	if k3 := deriveWebhookDeliveryKey("sub-2", "type.a", "v1", body); k3 == k1 {
		t.Error("a different subscription id produced the same key")
	}
	if k4 := deriveWebhookDeliveryKey("sub-1", "type.b", "v1", body); k4 == k1 {
		t.Error("a different public type produced the same key")
	}
}

// createPendingDelivery enqueues one real delivery for subID by publishing
// testMapping's InternalType through handleDomainEvent (over a discarding
// fake queue, so the row is created without a real job being submitted),
// and returns the created row read back from the repository.
func createPendingDelivery(t *testing.T, svc *Service, subID string) WebhookDelivery {
	t.Helper()
	prevQueue := svc.queue
	svc.queue = &fakeQueue{}
	defer func() { svc.queue = prevQueue }()

	if err := svc.handleDomainEvent(ctxFor(testTenant), pkgcore.Event{Type: testMapping.InternalType, TenantID: testTenant}); err != nil {
		t.Fatalf("handleDomainEvent: %v", err)
	}
	rows, err := svc.deliveryRepo.ListRecentBySubscription(ctxFor(testTenant), subID, 1)
	if err != nil {
		t.Fatalf("ListRecentBySubscription: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	return rows[0]
}

// deliveryJob builds the jobs.Job handleDeliveryJob and
// onWebhookDeliveryDeadLetter expect: TenantID set (mirroring what a real
// worker rebuilds before calling Handle) and Payload the encoded
// webhookDeliveryJobPayload.
func deliveryJob(deliveryID, subscriptionID string) *jobs.Job {
	payload, _ := json.Marshal(webhookDeliveryJobPayload{SubscriptionID: subscriptionID, DeliveryID: deliveryID})
	return &jobs.Job{
		ID:       "job-1",
		Type:     jobTypeWebhookDeliver,
		TenantID: testTenant,
		Payload:  payload,
	}
}
