package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// deadLetterPendingDelivery turns the freshly created pending delivery row
// createPendingDelivery produced into the dead-lettered state a redelivery
// exists for, the way a real retries-exhausted run would have left it:
// Status dead_letter, LastError set, no live job anywhere.
func deadLetterPendingDelivery(t *testing.T, svc *Service, subID string) WebhookDelivery {
	t.Helper()
	delivery := createPendingDelivery(t, svc, subID)
	delivery.Status = DeliveryStatusDeadLetter
	delivery.LastError = "retries exhausted"
	delivery.Attempts = 6
	if err := svc.deliveryRepo.Update(ctxFor(testTenant), &delivery); err != nil {
		t.Fatalf("deliveryRepo.Update: %v", err)
	}
	row, err := svc.deliveryRepo.FindByID(ctxFor(testTenant), delivery.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	return *row
}

// enqueuedDeliveryTask returns the one task the fake queue captured, with
// its payload decoded -- the webhookDeliveryJobPayload shape enqueueDelivery
// and RedeliverWebhookDelivery share.
func enqueuedDeliveryTask(t *testing.T, fq *fakeQueue) (jobs.Task, webhookDeliveryJobPayload) {
	t.Helper()
	if len(fq.tasks) != 1 {
		t.Fatalf("len(fq.tasks) = %d, want 1", len(fq.tasks))
	}
	var p webhookDeliveryJobPayload
	if err := json.Unmarshal(fq.tasks[0].Payload, &p); err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	return fq.tasks[0], p
}

// TestService_RedeliverWebhookDelivery_ReenqueuesAndDelivers pins the
// redelivery's full round trip: a dead-lettered delivery is re-enqueued as
// a fresh job under a fresh cycle key, the row flips back to pending, and
// running that job through the ordinary delivery path really delivers the
// stored Payload to the subscription's receiver -- the same receiver the
// original fan-out targeted, identified by the same HeaderWebhookID.
func TestService_RedeliverWebhookDelivery_ReenqueuesAndDelivers(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq), WithWebhookHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	dead := deadLetterPendingDelivery(t, svc, subID)

	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); err != nil {
		t.Fatalf("RedeliverWebhookDelivery: %v", err)
	}

	task, payload := enqueuedDeliveryTask(t, fq)
	if payload.SubscriptionID != subID || payload.DeliveryID != dead.ID {
		t.Errorf("enqueued job payload = (%q, %q), want (%q, %q)", payload.SubscriptionID, payload.DeliveryID, subID, dead.ID)
	}
	if task.TenantID != testTenant {
		t.Errorf("enqueued job tenant = %q, want %q", task.TenantID, testTenant)
	}
	wantKey := redeliveryIdempotencyKey(dead.ID, dead.Attempts)
	if task.IdempotencyKey != wantKey {
		t.Errorf("redelivery idempotency key = %q, want %q (delivery id plus the cycle's Attempts)", task.IdempotencyKey, wantKey)
	}

	// The row reads pending again -- a delivery awaiting its next attempt --
	// while its history stays intact for the new cycle's attempts to extend.
	row, err := svc.deliveryRepo.FindByID(ctxFor(testTenant), dead.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if row.Status != DeliveryStatusPending {
		t.Errorf("status after redelivery = %q, want %q", row.Status, DeliveryStatusPending)
	}
	if row.Attempts != dead.Attempts {
		t.Errorf("Attempts = %d, want %d (the new cycle starts from the old cycle's count)", row.Attempts, dead.Attempts)
	}

	// Run the re-enqueued job through the ordinary delivery handler: the
	// stored payload goes out, signed, to the subscription's receiver.
	if _, jobErr := svc.handleDeliveryJob(ctxFor(testTenant), deliveryJob(dead.ID, subID)); jobErr != nil {
		t.Fatalf("handleDeliveryJob: %v", jobErr)
	}
	if gotBody == nil {
		t.Fatal("the receiver never got the redelivered request")
	}
	if string(gotBody) != string(dead.Payload) {
		t.Errorf("redelivered body = %s, want the stored payload %s", gotBody, dead.Payload)
	}
	if gotHeaders.Get(HeaderWebhookID) != dead.ID {
		t.Errorf("%s = %q, want %q", HeaderWebhookID, gotHeaders.Get(HeaderWebhookID), dead.ID)
	}
	if gotHeaders.Get(HeaderWebhookSignature) == "" {
		t.Error("the redelivered request carries no signature header")
	}

	deliveries, err := svc.ListRecentWebhookDeliveries(ctxFor(testTenant), subID, 10)
	if err != nil {
		t.Fatalf("ListRecentWebhookDeliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Status != DeliveryStatusDelivered {
		t.Fatalf("deliveries = %+v, want exactly one Delivered row (still the same occurrence)", deliveries)
	}
}

func TestService_RedeliverWebhookDelivery_NotFound_Refused(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))

	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), "no-such-delivery"); !apperr.HasCode(err, ErrWebhookDeliveryNotFound.Code) {
		t.Errorf("redeliver of an unknown id = %v, want ErrWebhookDeliveryNotFound", err)
	}

	// Cross-tenant: the collapsed not-found, indistinguishable by design.
	_, otherSvc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, otherSvc, "https://example.com/hook")
	dead := deadLetterPendingDelivery(t, otherSvc, subID)
	const otherTenant pkgcore.TenantID = "tenant-2"
	if err := otherSvc.RedeliverWebhookDelivery(ctxFor(otherTenant), dead.ID); !apperr.HasCode(err, ErrWebhookDeliveryNotFound.Code) {
		t.Errorf("cross-tenant redeliver = %v, want ErrWebhookDeliveryNotFound", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (a refused redelivery enqueues nothing)", len(fq.tasks))
	}
}

// TestService_RedeliverWebhookDelivery_OnlyDeadLetter_Refused pins the state
// gate: only a dead-lettered delivery may be redelivered. Every other state
// has a live job of its own or already reached the receiver, so each is
// refused with ErrWebhookDeliveryNotDeadLetter and nothing is enqueued.
func TestService_RedeliverWebhookDelivery_OnlyDeadLetter_Refused(t *testing.T) {
	for _, state := range []string{DeliveryStatusPending, DeliveryStatusFailed, DeliveryStatusDelivered} {
		t.Run(state, func(t *testing.T) {
			fq := &fakeQueue{}
			_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
			subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

			delivery := createPendingDelivery(t, svc, subID)
			delivery.Status = state
			if state == DeliveryStatusDelivered {
				now := time.Now()
				delivery.DeliveredAt = &now
			}
			if err := svc.deliveryRepo.Update(ctxFor(testTenant), &delivery); err != nil {
				t.Fatalf("deliveryRepo.Update: %v", err)
			}

			err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), delivery.ID)
			if !apperr.HasCode(err, ErrWebhookDeliveryNotDeadLetter.Code) {
				t.Errorf("redeliver of a %q delivery = %v, want ErrWebhookDeliveryNotDeadLetter", state, err)
			}
			if len(fq.tasks) != 0 {
				t.Errorf("len(fq.tasks) = %d, want 0 (a refused redelivery enqueues nothing)", len(fq.tasks))
			}
		})
	}
}

func TestService_RedeliverWebhookDelivery_SubscriptionDeleted_Refused(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	dead := deadLetterPendingDelivery(t, svc, subID)

	if err := svc.DeleteWebhookSubscription(ctxFor(testTenant), subID); err != nil {
		t.Fatalf("DeleteWebhookSubscription: %v", err)
	}
	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); !apperr.HasCode(err, ErrWebhookSubscriptionNotFound.Code) {
		t.Errorf("redeliver after the subscription's deletion = %v, want ErrWebhookSubscriptionNotFound", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (nothing enqueued for a deleted subscription)", len(fq.tasks))
	}
}

func TestService_RedeliverWebhookDelivery_SubscriptionInactive_Refused(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	dead := deadLetterPendingDelivery(t, svc, subID)

	inactive := false
	if _, err := svc.UpdateWebhookSubscription(ctxFor(testTenant), UpdateWebhookSubscriptionInput{ID: subID, Active: &inactive}); err != nil {
		t.Fatalf("UpdateWebhookSubscription: %v", err)
	}
	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); !apperr.HasCode(err, ErrWebhookSubscriptionInactive.Code) {
		t.Errorf("redeliver to a paused subscription = %v, want ErrWebhookSubscriptionInactive", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (nothing enqueued for a paused subscription)", len(fq.tasks))
	}
}

func TestService_RedeliverWebhookDelivery_NoQueueWired_IsAPlainError(t *testing.T) {
	_, svc := newWebhookTestService(t)
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")
	dead := deadLetterPendingDelivery(t, svc, subID)

	err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID)
	if err == nil {
		t.Fatal("RedeliverWebhookDelivery with no queue wired = nil error, want the plain no-queue error")
	}
	if !strings.Contains(err.Error(), "no queue wired") {
		t.Errorf("error = %q, want it to name the missing queue wiring", err)
	}
	// The row must still be dead-lettered -- an unwired queue changes
	// nothing about the delivery.
	row, findErr := svc.deliveryRepo.FindByID(ctxFor(testTenant), dead.ID)
	if findErr != nil {
		t.Fatalf("FindByID: %v", findErr)
	}
	if row.Status != DeliveryStatusDeadLetter {
		t.Errorf("status after a refused redelivery = %q, want %q unchanged", row.Status, DeliveryStatusDeadLetter)
	}
}

// deliveringQueue is a jobs.Queue whose Enqueue runs the enqueued delivery
// job synchronously -- through the service's own handleDeliveryJob, against
// the queue's inner recorder -- BEFORE returning. It forces the exact
// interleaving the guarded post-enqueue flip exists for: the worker picks
// the redelivered job up the instant Enqueue returns, so the row is
// already settled by the time RedeliverWebhookDelivery's flip executes.
type deliveringQueue struct {
	inner jobs.Queue
	svc   *Service
}

func (q *deliveringQueue) Enqueue(ctx context.Context, task jobs.Task, opts ...jobs.EnqueueOption) (jobs.JobID, error) {
	var p webhookDeliveryJobPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		return "", err
	}
	if _, err := q.svc.handleDeliveryJob(ctx, deliveryJob(p.DeliveryID, p.SubscriptionID)); err != nil {
		return "", err
	}
	return q.inner.Enqueue(ctx, task, opts...)
}

func (q *deliveringQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	return nil, errors.New("deliveringQueue.Get is not implemented")
}

func (q *deliveringQueue) Cancel(context.Context, jobs.JobID) error { return nil }

var _ jobs.Queue = (*deliveringQueue)(nil)

// TestService_RedeliverWebhookDelivery_JobSettlingBeforeFlip_NoStalePending
// is the regression test for the flip's guard: the redelivered job can be
// picked up and settle the row (here: deliver it) between the enqueue and
// RedeliverWebhookDelivery's post-enqueue status flip, and the flip must
// not write its pending state back over that settled outcome. Before the
// flip became the guarded single-column update markPending performs, the
// flip was a full-row save of the dead-lettered read, and this test failed
// against it with the row resurrected to pending -- a delivered webhook
// whose log row claimed it was still awaiting delivery.
func TestService_RedeliverWebhookDelivery_JobSettlingBeforeFlip_NoStalePending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, svc := newWebhookTestService(t, WithWebhookHTTPClient(srv.Client()))
	subID, _ := createTestSubscription(t, svc, srv.URL)
	dead := deadLetterPendingDelivery(t, svc, subID)

	inner := &fakeQueue{}
	prevQueue := svc.queue
	svc.queue = &deliveringQueue{inner: inner, svc: svc}
	defer func() { svc.queue = prevQueue }()

	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); err != nil {
		t.Fatalf("RedeliverWebhookDelivery: %v", err)
	}

	row, err := svc.deliveryRepo.FindByID(ctxFor(testTenant), dead.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if row.Status != DeliveryStatusDelivered {
		t.Errorf("status = %q, want %q -- the post-enqueue flip resurrected pending over the job's delivered settlement", row.Status, DeliveryStatusDelivered)
	}
	if row.Attempts != dead.Attempts+1 {
		t.Errorf("Attempts = %d, want %d (the delivered run's own count)", row.Attempts, dead.Attempts+1)
	}
	if len(inner.tasks) != 1 {
		t.Errorf("len(inner.tasks) = %d, want 1 (the job was still enqueued)", len(inner.tasks))
	}
}

// TestService_RedeliverWebhookDelivery_CycleKeyedIdempotency pins the two
// protections that keep each manual redelivery one clean operation:
//
//   - A redelivery is a state TRANSITION (dead-lettered -> pending, with a
//     fresh job scheduled), exactly like Revoke's -- so a second call while
//     the row is no longer dead-lettered is refused with
//     ErrWebhookDeliveryNotDeadLetter rather than silently scheduling a
//     duplicate send. The only double-enqueue a same-cycle repeat could
//     produce is two calls racing from the same dead-lettered read, and
//     those share one idempotency key (delivery id plus the row's Attempts
//     at that instant -- the round-trip test pins the exact key), which the
//     queue dedupes onto one job.
//   - A LATER cycle is always redeliverable: once the new cycle's own
//     horizon exhausts and the row dead-letters again with its Attempts
//     advanced, the next redelivery derives a NEW key and gets its own job.
//     jobs' idempotency is unconditional per key on StandaloneQueue, so a
//     cycle-fixed key would let the first-ever redelivery swallow every
//     later one forever (see redeliveryIdempotencyKey's doc comment).
func TestService_RedeliverWebhookDelivery_CycleKeyedIdempotency(t *testing.T) {
	fq := &fakeQueue{}
	_, svc := newWebhookTestService(t, WithWebhookQueue(fq))
	subID, _ := createTestSubscription(t, svc, "https://example.com/hook")

	dead := deadLetterPendingDelivery(t, svc, subID)
	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); err != nil {
		t.Fatalf("first redelivery: %v", err)
	}
	if len(fq.tasks) != 1 {
		t.Fatalf("len(fq.tasks) = %d, want 1", len(fq.tasks))
	}
	firstKey := fq.tasks[0].IdempotencyKey
	if want := redeliveryIdempotencyKey(dead.ID, dead.Attempts); firstKey != want {
		t.Errorf("first-cycle key = %q, want %q", firstKey, want)
	}

	// A same-cycle repeat while the row is already pending is refused -- the
	// state flip is the double-click protection.
	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); !apperr.HasCode(err, ErrWebhookDeliveryNotDeadLetter.Code) {
		t.Errorf("same-cycle redelivery = %v, want ErrWebhookDeliveryNotDeadLetter", err)
	}
	if len(fq.tasks) != 1 {
		t.Errorf("len(fq.tasks) = %d, want 1 (a refused repeat enqueues nothing)", len(fq.tasks))
	}

	// The first cycle's job runs and fails again; its retry horizon exhausts
	// and the row dead-letters once more, Attempts advanced. The next
	// redelivery is a NEW cycle and must derive a NEW key -- nothing about
	// the previous cycle's job may stand in its way.
	row, err := svc.deliveryRepo.FindByID(ctxFor(testTenant), dead.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	row.Status = DeliveryStatusDeadLetter
	row.Attempts = dead.Attempts + 7
	row.LastError = "retries exhausted again"
	if err := svc.deliveryRepo.Update(ctxFor(testTenant), row); err != nil {
		t.Fatalf("deliveryRepo.Update: %v", err)
	}
	if err := svc.RedeliverWebhookDelivery(ctxFor(testTenant), dead.ID); err != nil {
		t.Fatalf("later-cycle redelivery: %v", err)
	}
	if len(fq.tasks) != 2 {
		t.Fatalf("len(fq.tasks) = %d, want 2", len(fq.tasks))
	}
	if fq.tasks[1].IdempotencyKey == firstKey {
		t.Errorf("later-cycle key equals the first cycle's: %q (the queue would swallow every later redelivery)", firstKey)
	}
	if want := redeliveryIdempotencyKey(dead.ID, dead.Attempts+7); fq.tasks[1].IdempotencyKey != want {
		t.Errorf("later-cycle key = %q, want %q (delivery id plus the new cycle's Attempts)", fq.tasks[1].IdempotencyKey, want)
	}
}
