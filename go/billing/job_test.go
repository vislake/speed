package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeGateway is a minimal billing.PaymentGateway double for job_test.go's
// own PollingService tests -- it never touches a network, only reports
// whatever QueryStatus fixture the test set up.
type fakeGateway struct {
	status    ChannelStatus
	amount    Money
	queryErr  error
	queryCall int
}

func (g *fakeGateway) CreateCharge(context.Context, ChargeRequest) (ChargeHandle, error) {
	return ChargeHandle{}, errors.New("fakeGateway: CreateCharge not implemented")
}

func (g *fakeGateway) VerifyWebhook(context.Context, map[string][]string, []byte) (NormalizedEvent, error) {
	return NormalizedEvent{}, errors.New("fakeGateway: VerifyWebhook not implemented")
}

func (g *fakeGateway) QueryStatus(context.Context, ChannelReference) (ChannelStatus, Money, error) {
	g.queryCall++
	if g.queryErr != nil {
		return "", Money{}, g.queryErr
	}
	return g.status, g.amount, nil
}

var _ PaymentGateway = (*fakeGateway)(nil)

func TestPollingService_Poll_ResolvesStuckPendingRow(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{status: ChannelStatusSucceeded, amount: Money{Cents: 100, Currency: "usd"}}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gw.queryCall != 1 {
		t.Errorf("QueryStatus called %d times, want 1", gw.queryCall)
	}

	got, err := events.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
}

func TestPollingService_Poll_SkipsRowsNotYetStuck(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now())
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{status: ChannelStatusSucceeded}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gw.queryCall != 0 {
		t.Errorf("QueryStatus called %d times, want 0 -- the row is too fresh to be stuck", gw.queryCall)
	}
}

// TestPollingService_Poll_UnwiredChannelIsSkippedNotFatal proves a row
// whose Channel has no wired PaymentGateway does not fail the whole pass --
// Poll's own doc comment on why this is a configuration gap, not a data
// error.
func TestPollingService_Poll_UnwiredChannelIsSkippedNotFatal(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	unwired := newTestPaymentEvent("alipay", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, unwired); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}
	wired := newTestPaymentEvent("stripe", "evt_2", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, wired); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{status: ChannelStatusSucceeded}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gw.queryCall != 1 {
		t.Errorf("QueryStatus called %d times, want 1 (only the wired channel's row)", gw.queryCall)
	}

	gotWired, err := events.Get(ctx, wired.ID)
	if err != nil {
		t.Fatalf("Get(wired): %v", err)
	}
	if gotWired.Status != string(ChannelStatusSucceeded) {
		t.Errorf("wired row Status = %q, want succeeded", gotWired.Status)
	}
	gotUnwired, err := events.Get(ctx, unwired.ID)
	if err != nil {
		t.Fatalf("Get(unwired): %v", err)
	}
	if gotUnwired.Status != string(ChannelStatusPending) {
		t.Errorf("unwired row Status = %q, want it left untouched (pending)", gotUnwired.Status)
	}
}

// TestPollingService_Poll_QueryErrorIsSkippedNotFatal mirrors the unwired-
// channel case: one row's QueryStatus failing must not block the rest of
// the batch.
func TestPollingService_Poll_QueryErrorIsSkippedNotFatal(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{queryErr: errors.New("channel unreachable")}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v, want nil -- a single row's query error must not fail the pass", err)
	}

	got, err := events.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusPending) {
		t.Errorf("Status = %q, want left untouched (pending)", got.Status)
	}
}

func TestPollingService_Poll_StillPendingLeavesRowUntouched(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{status: ChannelStatusPending}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gw.queryCall != 1 {
		t.Errorf("QueryStatus called %d times, want 1", gw.queryCall)
	}
}

// fakeQueue is a minimal jobs.Queue double recording every Enqueue call.
type fakeQueue struct {
	tasks []jobs.Task
	err   error
}

func (q *fakeQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	if q.err != nil {
		return "", q.err
	}
	q.tasks = append(q.tasks, task)
	return jobs.JobID("job-1"), nil
}

func (q *fakeQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	return nil, errors.New("fakeQueue: Get not implemented")
}

func (q *fakeQueue) Cancel(context.Context, jobs.JobID) error {
	return errors.New("fakeQueue: Cancel not implemented")
}

var _ jobs.Queue = (*fakeQueue)(nil)

func TestPollingService_EnqueuePoll_NoQueueWired(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	svc := newPollingService(events, nil, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if err := svc.EnqueuePoll(ctx); err == nil {
		t.Error("EnqueuePoll with no queue wired = nil error, want an error")
	}
}

func TestPollingService_EnqueuePoll_NoTenant(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	q := &fakeQueue{}
	svc := newPollingService(events, nil, q)

	if err := svc.EnqueuePoll(context.Background()); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("EnqueuePoll with no tenant in ctx: err = %v, want it to wrap pkgcore.ErrNoTenant", err)
	}
}

func TestPollingService_EnqueuePoll_EnqueuesWithPerTenantIdempotencyKey(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	q := &fakeQueue{}
	svc := newPollingService(events, nil, q)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("EnqueuePoll: %v", err)
	}
	if len(q.tasks) != 1 {
		t.Fatalf("tasks enqueued = %d, want 1", len(q.tasks))
	}
	task := q.tasks[0]
	if task.Type != taskTypePoll {
		t.Errorf("Type = %q, want %q", task.Type, taskTypePoll)
	}
	if task.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want tenant-a", task.TenantID)
	}
	if task.IdempotencyKey != pollIdempotencyKey("tenant-a") {
		t.Errorf("IdempotencyKey = %q, want %q", task.IdempotencyKey, pollIdempotencyKey("tenant-a"))
	}
}

func TestPollHandler_Handle_DrivesPoll(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	gw := &fakeGateway{status: ChannelStatusSucceeded}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)
	h := pollHandler{svc: svc}

	if got := h.Type(); got != taskTypePoll {
		t.Errorf("Type() = %q, want %q", got, taskTypePoll)
	}

	job := &jobs.Job{TenantID: "tenant-a"}
	if _, err := h.Handle(ctx, job, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gw.queryCall != 1 {
		t.Errorf("QueryStatus called %d times, want 1", gw.queryCall)
	}
}

func TestPollHandler_Handle_RejectsNonEmptyPayload(t *testing.T) {
	svc := newPollingService(NewPaymentEventRepository(newTestDB(t)), nil, nil)
	h := pollHandler{svc: svc}

	job := &jobs.Job{TenantID: "tenant-a", Payload: []byte(`{"unexpected":true}`)}
	if _, err := h.Handle(context.Background(), job, nil); err == nil {
		t.Error("Handle with a non-empty payload = nil error, want an error")
	}
}
