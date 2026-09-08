package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
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
	if got.Amount() != gw.amount {
		t.Errorf("Amount = %+v, want %+v -- QueryStatus's freshly re-queried Money must be persisted, not discarded", got.Amount(), gw.amount)
	}
}

// TestPollingService_Poll_ResolvesZeroedAmountRow is the end-to-end
// regression test for "a payment event zeroed to Amount=0 by the pending-
// branch fix keeps Amount=0 forever even after resolving to Succeeded": a
// row inserted with a zero-valued Amount -- exactly the shape event.go's
// normalizeCheckoutSession's ChannelStatusPending branch produces for an
// unsettled checkout.session.completed webhook -- must land with the real,
// freshly re-queried Amount once Poll resolves it, driven through the whole
// Poll call (not just markStatus directly, as
// TestPaymentEventRepository_MarkStatus_OverwritesZeroAmount already proves
// at the repository layer). Poll must not discard QueryStatus's Money
// entirely (`status, _, err := gw.QueryStatus(...)`) and call markStatus
// with no amount to persist at all.
func TestPollingService_Poll_ResolvesZeroedAmountRow(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	evt := newTestPaymentEvent("stripe", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
	evt.SetAmount(Money{}) // the zeroed shape a completed-but-unpaid webhook inserts
	if _, err := events.InsertIfNew(ctx, evt); err != nil {
		t.Fatalf("InsertIfNew: %v", err)
	}

	resolved := Money{Cents: 2900, Currency: "usd"}
	gw := &fakeGateway{status: ChannelStatusSucceeded, amount: resolved}
	svc := newPollingService(events, map[string]PaymentGateway{"stripe": gw}, nil)

	if err := svc.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	got, err := events.Get(ctx, evt.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != string(ChannelStatusSucceeded) {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.Amount() != resolved {
		t.Errorf("Amount = %+v, want %+v -- a real, money-moved payment must not be permanently ledgered as a zero-amount success", got.Amount(), resolved)
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

// TestPollingService_Poll_QueryFailureLogsClassificationNeverRawText is
// the regression for the poll log point's provider-text discipline (see
// pollFailureClass's doc comment in job.go): a QueryStatus failure must
// reach the poll log as a bounded classification only, never as the raw
// error's text -- a gateway error can carry external-provider free text (a
// WeChat/Alipay business-envelope message, a Stripe SDK error), and
// go/observability's redaction layer masks credential shapes and sensitive
// key names, never arbitrary free text under the ubiquitous "error" key,
// so the log point must bound the value itself. Two failure shapes are
// scripted, each carrying a distinctive fragment that must not appear in
// any of the poll log's attributes after the fix: an opaque gateway error
// (the shape every non-typed QueryStatus failure takes -- wechat's wrapped
// vendor refusals, alipay's query-failed envelopes, stripe's SDK errors)
// and a typed billing error carrying the fragment as its unbounded cause
// (the sharp case, since (*apperr.Error).Error() renders "code: cause" --
// the classification must read the error's code, never its text). Before
// the fix, the log's "error" attribute carried the whole error text
// verbatim and both legs failed.
func TestPollingService_Poll_QueryFailureLogsClassificationNeverRawText(t *testing.T) {
	const echoedFragment = "mango-vendor-echo-4c9d7"

	legs := []struct {
		name      string
		queryErr  error
		wantClass string
	}{
		{
			name:      "opaque gateway error",
			queryErr:  errors.New("billing/gateway/wechat: query failed: SYSTEMERROR: " + echoedFragment),
			wantClass: "unclassified", // pollFailureClass's fixed literal for an error no typed class covers
		},
		{
			name:      "typed billing error with unbounded cause",
			queryErr:  ErrChannelReferenceNotFound.WithCause(fmt.Errorf("channel answered: %s", echoedFragment)),
			wantClass: ErrChannelReferenceNotFound.Code,
		},
	}

	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			events := NewPaymentEventRepository(newTestDB(t))

			evt := newTestPaymentEvent("wechat", "evt_1", ChannelStatusPending, time.Now().Add(-time.Hour))
			if _, err := events.InsertIfNew(pkgcore.WithTenant(context.Background(), "tenant-a"), evt); err != nil {
				t.Fatalf("InsertIfNew: %v", err)
			}

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			ctx := observability.WithLogger(pkgcore.WithTenant(context.Background(), "tenant-a"), logger)

			gw := &fakeGateway{queryErr: leg.queryErr}
			svc := newPollingService(events, map[string]PaymentGateway{"wechat": gw}, nil)

			if err := svc.Poll(ctx); err != nil {
				t.Fatalf("Poll: %v, want nil -- a single row's query error must not fail the pass", err)
			}
			if gw.queryCall != 1 {
				t.Fatalf("QueryStatus called %d times, want 1", gw.queryCall)
			}

			logged := logBuf.String()
			if strings.Contains(logged, echoedFragment) {
				t.Fatalf("poll log carries the provider error text's fragment %q in its attributes; log = %q", echoedFragment, logged)
			}
			if want := "failure_class=" + leg.wantClass; !strings.Contains(logged, want) {
				t.Fatalf("poll log lacks the bounded classification %q; log = %q", want, logged)
			}
		})
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

func TestPollingService_EnqueuePoll_EnqueuesWithWindowScopedIdempotencyKey(t *testing.T) {
	events := NewPaymentEventRepository(newTestDB(t))
	q := &fakeQueue{}
	svc := newPollingService(events, nil, q)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	// Pin the service's clock so the window the enqueue lands under is
	// deterministic (poll_window_test.go covers the window semantics
	// against a real queue; this test pins the key's shape).
	enqueuedAt := time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	svc.now = func() time.Time { return enqueuedAt }

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
	want := pollIdempotencyKey("tenant-a", pollWindowStart(enqueuedAt))
	if task.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q (the key must name the enqueue's own poll window)", task.IdempotencyKey, want)
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
