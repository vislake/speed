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

	"gorm.io/gorm"

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
// regression test for the zero-amount-stuck defect: a row inserted with a
// zero-valued Amount -- exactly the shape event.go's
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
// any of the poll log's attributes: an opaque gateway error (the shape
// every non-typed QueryStatus failure takes -- wechat's wrapped vendor
// refusals, alipay's query-failed envelopes, stripe's SDK errors) and a
// typed billing error carrying the fragment as its unbounded cause (the
// sharp case, since (*apperr.Error).Error() renders "code: cause" -- the
// classification must read the error's code, never its text). A log
// point that wrote the raw error text verbatim would fail both legs.
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
	// deterministic (the poll-window tests below cover the window
	// semantics against a real queue; this test pins the key's shape).
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

// The tests below pin the window semantics of the poll idempotency key
// (pollIdempotencyKey): enqueues inside one poll window collapse into one
// job (the concurrency protection the key exists for, preserved),
// enqueues in a later window become new jobs and the poll runs again
// (periodicity -- jobs' dedup is permanent on StandaloneQueue, so a
// tenant-only key would give each tenant exactly one poll task per
// database file and a stuck payment would never be polled once its
// one-ever poll had run), and a poll job that dead-letters poisons only
// its own window, never its tenant's later windows. All three run against
// a REAL jobs.StandaloneQueue over a real SQLite database -- the dedupe
// behaviour under test lives in jobs' partial unique index and row
// semantics, which a fake queue cannot exercise.
//
// The window boundary constants below are the implementation's own
// (pollIdempotencyWindowSize, 15 minutes -- DefaultPollStuckAfter, the
// poll's own detection granularity) spelled as local literals so a drift
// between the two would break the later-window test loudly: an enqueue
// pollWindowB apart would then land inside the implementation's own
// window and collapse instead of creating the second job.
//
// EnqueuePoll returns no job id (it is a fire-and-forget schedule point),
// so the tests observe the queue's own database -- the same *gorm.DB the
// queue was started over -- for row counts and ids, plus the registered
// handler's run channel for executions.

// startPollWindowQueue starts a real StandaloneQueue over its own fresh
// billing-migrated database with fast intervals, registering cleanup, and
// returns both the queue and its database.
func startPollWindowQueue(t *testing.T) (*jobs.StandaloneQueue, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	q := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(5*time.Millisecond),
		jobs.WithWorkerCount(1),
		jobs.WithBackoff(5*time.Millisecond, 50*time.Millisecond),
	)
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("StandaloneQueue.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := q.Close(ctx); err != nil {
			t.Errorf("StandaloneQueue.Close() error = %v", err)
		}
	})
	return q, db
}

// pollWindowA and pollWindowB are two points in time in two different
// poll windows (10:15 and 10:30 UTC), windowB exactly one
// pollIdempotencyWindowSize later than windowA.
var (
	pollWindowA = time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	pollWindowB = pollWindowA.Add(15 * time.Minute)
)

// pollRowCount counts the poll-task rows in the queue's own database: one
// per idempotency-key window resolved, whether the row is pending,
// running or already settled.
func pollRowCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Table("jobs").Where("type = ?", taskTypePoll).Count(&n).Error; err != nil {
		t.Fatalf("count poll rows: %v", err)
	}
	return n
}

// firstPollJobID returns the id of the database's only poll-task row.
func firstPollJobID(t *testing.T, db *gorm.DB) jobs.JobID {
	t.Helper()
	if n := pollRowCount(t, db); n != 1 {
		t.Fatalf("poll rows = %d, want exactly 1 before reading the first job id", n)
	}
	var ids []string
	if err := db.Table("jobs").Where("type = ?", taskTypePoll).Pluck("id", &ids).Error; err != nil {
		t.Fatalf("read first poll job id: %v", err)
	}
	return jobs.JobID(ids[0])
}

// waitForPollRun waits until a poll handler run lands on runs and returns
// its job id, failing the test after timeout. runs must be a buffered
// channel the handler fills once per Handle call.
func waitForPollRun(t *testing.T, runs chan jobs.JobID, what string) jobs.JobID {
	t.Helper()
	select {
	case id := <-runs:
		return id
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no poll run within 20s", what)
		return ""
	}
}

// TestEnqueuePoll_SameWindowEnqueuesCollapseIntoOneJob pins the
// same-window collapse: the concurrency protection the poll key exists
// for must survive the windowing -- two enqueues for one tenant inside
// the same poll window collapse into the first job (one row, one run), so
// two scheduler replicas ticking in one window still never poll the
// tenant twice at once.
func TestEnqueuePoll_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	q, db := startPollWindowQueue(t)
	svc := newPollingService(NewPaymentEventRepository(db), nil, q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypePoll, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return pollWindowA }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("first EnqueuePoll: %v", err)
	}
	// A second replica's tick five minutes later -- still inside
	// pollWindowA's window.
	svc.now = func() time.Time { return pollWindowA.Add(5 * time.Minute) }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("second EnqueuePoll: %v", err)
	}

	if n := pollRowCount(t, db); n != 1 {
		t.Fatalf("poll rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row", n)
	}
	first := waitForPollRun(t, runs, "the collapsed poll")
	if first != firstPollJobID(t, db) {
		t.Errorf("poll run job id = %s, want the row's id %s", first, firstPollJobID(t, db))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("poll ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnqueuePoll_LaterWindowEnqueuesNewJobAndRunsAgain pins the
// later-window periodicity: an enqueue in a later window is a NEW job and
// the poll runs again. A tenant-only key would fail this: the later
// enqueue would resolve the first job's id -- the first-ever poll's
// permanent dedupe -- so no second row is ever created and nothing ever
// runs again: the harm the windowing closes, a stuck payment that is
// never actively polled once the payment chain is connected (the single
// run of the tenant's one-ever poll usually finds nothing, since no row
// is stuck yet at DefaultPollStuckAfter past its start).
func TestEnqueuePoll_LaterWindowEnqueuesNewJobAndRunsAgain(t *testing.T) {
	q, db := startPollWindowQueue(t)
	svc := newPollingService(NewPaymentEventRepository(db), nil, q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypePoll, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return pollWindowA }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("first EnqueuePoll: %v", err)
	}
	first := waitForPollRun(t, runs, "window A's poll")

	// The scheduler's tick one window later: pollWindowB.
	svc.now = func() time.Time { return pollWindowB }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("second EnqueuePoll: %v", err)
	}

	if n := pollRowCount(t, db); n != 2 {
		t.Fatalf("poll rows = %d, want 2 -- the later window's enqueue must create a NEW job (fails on the tenant-only key, which resolves the first row forever)", n)
	}
	second := waitForPollRun(t, runs, "window B's poll")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own poll", second)
	}
}

// TestEnqueuePoll_DeadLetteredWindowDoesNotPoisonLaterOnes pins the
// dead-letter isolation: a poll job that dead-letters poisons only its
// own window. A stuck payment left unpolled is money-path harm, and a
// permanently-failing poll pass (a database outage lasting out the retry
// budget) must never silence its tenant's later windows. A tenant-only
// key would fail this: the dead job's idempotency key would stay resolved
// forever -- every later enqueue would return the dead job's id, no
// second row would ever be created and the tenant would never be polled
// again.
func TestEnqueuePoll_DeadLetteredWindowDoesNotPoisonLaterOnes(t *testing.T) {
	q, db := startPollWindowQueue(t)
	svc := newPollingService(NewPaymentEventRepository(db), nil, q)

	// succeed is flipped only after window A's job has dead-lettered;
	// until then every Handle fails permanently, which is what
	// dead-letters it (with DefaultMaxRetries 3, the fourth attempt
	// exhausts the budget).
	var succeed bool
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypePoll, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		if !succeed {
			return jobs.Result{}, errors.New("billing: injected poll failure")
		}
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	getCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return pollWindowA }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("first EnqueuePoll: %v", err)
	}
	first := firstPollJobID(t, db)

	// Window A's poll exhausts its retries and dead-letters. Wait for the
	// terminal state rather than counting attempts: the worker may have
	// claimed the row before or after any particular write, but the
	// always-failing handler guarantees the terminal state either way.
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, err := q.Get(getCtx, first)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", first, err)
		}
		if job.Status == jobs.StatusDeadLetter {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("window A's poll never dead-lettered within 20s (status %v)", job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next window's enqueue must run its own poll, whatever happened
	// to window A's.
	succeed = true
	svc.now = func() time.Time { return pollWindowB }
	if err := svc.EnqueuePoll(ctx); err != nil {
		t.Fatalf("second EnqueuePoll: %v", err)
	}

	if n := pollRowCount(t, db); n != 2 {
		t.Fatalf("poll rows = %d, want 2 -- the dead-lettered window's key must not keep resolving for later windows (fails on the tenant-only key)", n)
	}
	second := waitForPollRun(t, runs, "window B's poll after window A dead-lettered")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's dead-lettered job -- a dead-lettered window must not poison the tenant's later windows", second)
	}
}
