package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs/internal/testutil"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// testSystemPurpose is the pkgcore.SystemPurpose these tests declare for
// exercising the pkgcore.WithSystemContext branch of CallerMayAccess.
const testSystemPurpose = pkgcore.SystemPurpose("jobs.test_system_access")

// newTestQueue returns a StandaloneQueue backed by a private, per-test temp-file
// SQLite database with the jobs schema already applied -- but NOT started:
// callers that need Enqueue calls to land before the dispatcher's first
// poll tick (TestPriorityOrdering, in particular) construct with this,
// finish every Enqueue call they need, and only then call startQueue.
// Poll interval and backoff are both set short so tests observe outcomes
// quickly; every value remains overridable via opts.
func newTestQueue(t *testing.T, opts ...Option) *StandaloneQueue {
	t.Helper()
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	defaults := []Option{
		WithPollInterval(15 * time.Millisecond),
		WithBackoff(20*time.Millisecond, 200*time.Millisecond),
	}
	return NewStandaloneQueue(db, append(defaults, opts...)...)
}

// startQueue starts q and registers a bounded Close via t.Cleanup.
func startQueue(t *testing.T, q *StandaloneQueue) {
	t.Helper()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})
}

// pollJob polls Get until done reports true or timeout elapses.
func pollJob(t *testing.T, q *StandaloneQueue, ctx context.Context, id JobID, timeout time.Duration, done func(*Job) bool) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *Job
	for {
		job, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%q) error = %v", id, err)
		}
		last = job
		if done(job) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for job %q; last state = %+v", timeout, id, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitTerminal polls until id reaches a terminal Status.
func waitTerminal(t *testing.T, q *StandaloneQueue, ctx context.Context, id JobID) *Job {
	t.Helper()
	return pollJob(t, q, ctx, id, signalWaitTimeout, func(j *Job) bool { return j.Status.Terminal() })
}

// signalWaitTimeout bounds waitSignal: how long a channel-ready event
// may take under a slow scheduler before the test gives up. Generous by
// design -- waitSignal is event-driven, so the cap is paid only when the
// event never arrives. 5s is an eternity for the tick cadences (10-15ms
// poll intervals) every queue here runs on, and an eventual-style
// ceiling is what keeps a starved scheduler from turning a merely
// delayed start into a test failure.
const signalWaitTimeout = 5 * time.Second

// waitSignal waits until ch delivers a value and returns it, failing
// the test with what on timeout. A closed channel counts as delivered
// (the receive returns the zero value immediately), so close-based
// signals and send-based ones share this one wait. Channel-ready events
// need no polling; the cap alone is what makes them robust, so every
// wait that can stretch under load funnels through here rather than
// spelling out its own time.After window.
func waitSignal[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(signalWaitTimeout):
		t.Fatalf("timed out after %v waiting for %s", signalWaitTimeout, what)
		return *new(T)
	}
}

// TestWriterStaleAfter_OwnWindowFromOwnPollInterval pins writerStaleAfter's
// arithmetic -- the ten-poll-interval ratio and the two-second floor -- the
// stale window a queue authors into its OWN registration (the row's
// stale_at = last beat + this window). Both numbers are what keep a live
// queue's authored stale moment always beyond its next beat (window >= ten
// beats, beats arrive every poll interval), so no taker, whatever its own
// cadence, can ever find a live incumbent stale. The two boundary shapes
// are the floor (any cadence whose tenfold stays under two seconds) and
// the ratio (any cadence whose tenfold exceeds it).
func TestWriterStaleAfter_OwnWindowFromOwnPollInterval(t *testing.T) {
	cases := []struct {
		name         string
		pollInterval time.Duration
		want         time.Duration
	}{
		{"the 200ms default hits the two-second floor", DefaultPollInterval, 2 * time.Second},
		{"any sub-200ms cadence hits the two-second floor", 150 * time.Millisecond, 2 * time.Second},
		{"a one-second cadence is ten seconds", time.Second, 10 * time.Second},
		{"a five-second cadence is fifty seconds", 5 * time.Second, 50 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writerStaleAfter(tc.pollInterval); got != tc.want {
				t.Errorf("writerStaleAfter(%v) = %v, want %v", tc.pollInterval, got, tc.want)
			}
		})
	}
}

func TestStandaloneQueue_StartAndClose_Lifecycle(t *testing.T) {
	q := newTestQueue(t)

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v, want nil (no-op)", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := q.Close(ctx); err != nil {
		t.Fatalf("second Close() error = %v, want nil (idempotent)", err)
	}
}

func TestRegisterHandler_DuplicateType_Errors(t *testing.T) {
	q := NewStandaloneQueue(nil)
	h := NewHandlerFunc("widgets.resize", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, nil
	})

	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("first RegisterHandler() error = %v", err)
	}
	err := q.RegisterHandler(h)
	if !apperr.HasCode(err, ErrDuplicateHandlerType.Code) {
		t.Fatalf("second RegisterHandler() error = %v, want ErrDuplicateHandlerType", err)
	}
}

func TestEnqueue_InvalidTask_ReturnsError(t *testing.T) {
	q := newTestQueue(t)
	_, err := q.Enqueue(context.Background(), Task{})
	if !apperr.HasCode(err, ErrInvalidTask.Code) {
		t.Fatalf("Enqueue(Task{}) error = %v, want ErrInvalidTask", err)
	}
}

// TestEnqueue_Get_HappyPath, TestGet_TenantIsolation and
// TestCancel_TenantIsolation_And_Idempotency live in
// unittest/queue_conformance_test.go's
// TestStandaloneQueue_ConformsToQueueContract, which drives the shared
// go/jobs/queuetest.AssertConforms suite (its own "enqueue_get_happy_path",
// "get_tenant_isolation" and "cancel_tenant_isolation_and_idempotency"
// subtests) -- the same suite every Queue implementation must pass.

func TestPriorityOrdering(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(1))

	var (
		mu    sync.Mutex
		order []string
	)
	if err := q.RegisterHandler(NewHandlerFunc("record", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		mu.Lock()
		order = append(order, string(job.Payload))
		mu.Unlock()
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// Both Enqueue calls land before startQueue below, so the dispatcher's
	// very first poll tick sees both rows together -- no race against
	// when that first tick happens to fire.
	lowID, err := q.Enqueue(context.Background(), Task{Type: "record", TenantID: "tenant-a", Payload: []byte("low")}, WithPriority(PriorityLow))
	if err != nil {
		t.Fatalf("Enqueue(low) error = %v", err)
	}
	highID, err := q.Enqueue(context.Background(), Task{Type: "record", TenantID: "tenant-a", Payload: []byte("high")}, WithPriority(PriorityHigh))
	if err != nil {
		t.Fatalf("Enqueue(high) error = %v", err)
	}

	startQueue(t, q)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	waitTerminal(t, q, ctx, lowID)
	waitTerminal(t, q, ctx, highID)

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "high" || got[1] != "low" {
		t.Errorf("execution order = %v, want [high low] (higher priority dispatched first, workerCount=1 makes this deterministic)", got)
	}
}

func TestDelayedExecution_DoesNotRunEarly(t *testing.T) {
	q := newTestQueue(t)
	var ran atomic.Bool
	if err := q.RegisterHandler(NewHandlerFunc("delayed", func(context.Context, *Job, ProgressFn) (Result, error) {
		ran.Store(true)
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	id, err := q.Enqueue(ctx, Task{Type: "delayed", TenantID: "tenant-a"}, WithDelay(300*time.Millisecond))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// The negative window -- the job must not run before its own
	// scheduled_at -- is asserted from the moment it first leaves
	// StatusPending, with no fixed wall-clock sleep anywhere in the
	// window. The boundary needs no test-side clock to verify: the
	// queue enforces it in the claim query itself (claimCandidatesSQL
	// selects only rows whose scheduled_at has passed, so no worker can
	// receive a delayed Job early by design), and every stamp a
	// legitimate run writes -- the claim's updated_at, the attempt's
	// started_at -- is therefore at or after scheduled_at on the same
	// row, read back from the same database clock. A stamp before
	// scheduled_at is the delay not being enforced; a stamp at or after
	// it clears the window however late this goroutine woke to look.
	// pollJob keeps waking until either verdict, so a slow scheduler
	// stretches the wait instead of invalidating the assertion.
	first := pollJob(t, q, ctx, id, signalWaitTimeout, func(j *Job) bool { return j.Status != StatusPending })
	if first.UpdatedAt.Before(first.ScheduledAt) ||
		(first.StartedAt != nil && first.StartedAt.Before(first.ScheduledAt)) {
		t.Fatalf("job ran before its scheduled_at: first observed status %v with scheduled_at %v, updated_at %v, started_at %v -- the delay was not enforced",
			first.Status, first.ScheduledAt, first.UpdatedAt, first.StartedAt)
	}

	waitTerminal(t, q, ctx, id)
	if !ran.Load() {
		t.Error("handler never ran after its delay elapsed")
	}
}

// flakyHandler fails its first failuresBefore attempts, then succeeds. It
// also implements FailureHook, so this same fixture proves the hook is
// NOT invoked on an eventual success -- only TestDeadLetter_* proves the
// positive case.
type flakyHandler struct {
	failuresBefore int32
	attempts       atomic.Int32
	onFailureCalls atomic.Int32
}

func (*flakyHandler) Type() string { return "flaky" }

func (h *flakyHandler) Handle(_ context.Context, _ *Job, _ ProgressFn) (Result, error) {
	if h.attempts.Add(1) <= h.failuresBefore {
		return Result{}, errors.New("transient failure")
	}
	return Result{Data: []byte("finally")}, nil
}

func (h *flakyHandler) OnFailure(context.Context, *Job, error) {
	h.onFailureCalls.Add(1)
}

// TestRetry_SucceedsAfterTransientFailures and
// TestDeadLetter_ExhaustsRetries_And_InvokesFailureHook live in
// unittest/queue_conformance_test.go's
// TestStandaloneQueue_ConformsToQueueContract, which drives the shared
// go/jobs/queuetest.AssertConforms suite (its own
// "retry_succeeds_after_transient_failures" and
// "dead_letter_exhausts_retries_and_invokes_failure_hook" subtests).
// flakyHandler and countingFailureHandler stay defined below/above: both
// are still used by
// TestStandaloneQueue_JobMetrics_RecordsDurationAttemptsAndDeadLetter,
// which proves this package's own metrics instrumentation rather than the
// portable Queue contract queuetest owns.

// countingFailureHandler always fails, and records every OnFailure call it
// receives on onFailureCh.
type countingFailureHandler struct {
	onFailureCh chan *Job
}

func (*countingFailureHandler) Type() string { return "always-fails" }

func (*countingFailureHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, errors.New("permanent failure")
}

func (h *countingFailureHandler) OnFailure(_ context.Context, job *Job, _ error) {
	h.onFailureCh <- job
}

// blockingHandler signals startedCh with its Job's id, then blocks until
// the test sends on releaseCh.
type blockingHandler struct {
	startedCh chan JobID
	releaseCh chan struct{}
}

func (*blockingHandler) Type() string { return "flood" }

func (h *blockingHandler) Handle(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
	h.startedCh <- job.ID
	<-h.releaseCh
	return Result{}, nil
}

// TestPerTenantConcurrencyLimiting proves one tenant's backlog cannot
// starve another tenant's Jobs, and that the per-tenant cap is actually
// enforced rather than merely documented.
func TestPerTenantConcurrencyLimiting(t *testing.T) {
	q := newTestQueue(t, WithWorkerCount(2), WithTenantConcurrencyLimit(1))

	flood := &blockingHandler{startedCh: make(chan JobID, 8), releaseCh: make(chan struct{})}
	if err := q.RegisterHandler(flood); err != nil {
		t.Fatalf("RegisterHandler(flood) error = %v", err)
	}
	quickDone := make(chan struct{})
	if err := q.RegisterHandler(NewHandlerFunc("quick", func(context.Context, *Job, ProgressFn) (Result, error) {
		close(quickDone)
		return Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler(quick) error = %v", err)
	}
	startQueue(t, q)
	// Registered after startQueue's Close cleanup, so cleaning up in LIFO
	// order releases every blocked flood Handle before the drain waits: a
	// failure mid-test must not leave a flood worker holding a row, its
	// Handle still in flight, past the end of the test.
	t.Cleanup(func() { close(flood.releaseCh) })

	var floodIDs []JobID
	for i := 0; i < 3; i++ {
		id, err := q.Enqueue(context.Background(), Task{Type: "flood", TenantID: "tenant-a"})
		if err != nil {
			t.Fatalf("Enqueue(flood %d) error = %v", i, err)
		}
		floodIDs = append(floodIDs, id)
	}
	if _, err := q.Enqueue(context.Background(), Task{Type: "quick", TenantID: "tenant-b"}); err != nil {
		t.Fatalf("Enqueue(quick) error = %v", err)
	}

	firstStarted := waitSignal(t, flood.startedCh, "the first flood job to start")

	// tenant-b's job completes promptly despite tenant-a's flood already
	// occupying a worker -- the core proof that one tenant cannot starve
	// another.
	waitSignal(t, quickDone, "tenant-b's job to complete while tenant-a's flood held the queue")

	// A second tenant-a job must NOT start while the first is still
	// running: tenant-a is at its concurrency limit of 1. Every start is
	// recorded in the buffered startedCh, so the select below is robust
	// to a late-woken test goroutine either way: a wrongful start is
	// already in the buffer whenever this goroutine next runs, and a
	// correct queue keeps the buffer empty no matter how long the hold.
	// The hold is generous -- 500ms spans some thirty of the queue's
	// 15ms dispatch ticks, deliberately, so a slow scheduler still gives
	// the dispatcher many chances to wrongly start the second job.
	select {
	case second := <-flood.startedCh:
		t.Fatalf("a second tenant-a job (%q) started while the first (%q) was still running; the per-tenant concurrency limit was not enforced", second, firstStarted)
	case <-time.After(500 * time.Millisecond):
	}

	// Releasing the first flood job frees tenant-a's one slot; each
	// release must let exactly the next queued flood job start. The
	// waits are event-driven with the shared generous cap: a slow
	// scheduler stretches them, it does not break them.
	flood.releaseCh <- struct{}{}
	waitSignal(t, flood.startedCh, "the second flood job to start after the first was released")
	flood.releaseCh <- struct{}{}
	waitSignal(t, flood.startedCh, "the third flood job to start after the second was released")
	flood.releaseCh <- struct{}{}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	for _, id := range floodIDs {
		waitTerminal(t, q, ctx, id)
	}
}

// progressHandler reports 30% then blocks until the test lets it continue,
// then reports 90% and succeeds -- so the test can observe a mid-flight
// progress report deterministically.
type progressHandler struct {
	afterFirstReport chan struct{}
	resume           chan struct{}
}

func (*progressHandler) Type() string { return "progress" }

func (h *progressHandler) Handle(_ context.Context, _ *Job, progress ProgressFn) (Result, error) {
	progress(30, "step one")
	close(h.afterFirstReport)
	<-h.resume
	progress(90, "step two")
	return Result{Data: []byte("done")}, nil
}

func TestProgressReporting(t *testing.T) {
	q := newTestQueue(t)
	h := &progressHandler{afterFirstReport: make(chan struct{}), resume: make(chan struct{})}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)
	// Registered after startQueue's Close cleanup, so cleaning up in LIFO
	// order resumes the blocked Handle before the drain waits: a failure
	// before the explicit resume below must not leave the worker
	// mid-Handle past the end of the test.
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(h.resume) }) }
	t.Cleanup(resume)

	id, err := q.Enqueue(context.Background(), Task{Type: "progress", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	waitSignal(t, h.afterFirstReport, "the handler's first progress update")

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	mid := pollJob(t, q, ctx, id, signalWaitTimeout, func(j *Job) bool { return j.ProgressPct == 30 })
	if mid.ProgressMsg != "step one" {
		t.Errorf("mid-flight ProgressMsg = %q, want %q", mid.ProgressMsg, "step one")
	}
	if mid.Status != StatusRunning {
		t.Errorf("Status while progress is mid-flight = %v, want %v", mid.Status, StatusRunning)
	}

	resume()
	final := waitTerminal(t, q, ctx, id)
	if final.ProgressPct != 90 || final.ProgressMsg != "step two" {
		t.Errorf("final progress = (%d, %q), want (90, %q)", final.ProgressPct, final.ProgressMsg, "step two")
	}
}

// widgetFixture is a minimal tenant-scoped fixture used only to prove
// TestStandaloneQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant against a
// real dbkit.Repository[T], following the same "define a small fixture
// directly" precedent go/tenancy/tenancytest's own sprocket fixture doc
// comment establishes (dbkit's own tenant-scoped test fixture lives in an
// unexported internal package this module cannot reach).
type widgetFixture struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Name     string `gorm:"column:name;size:255"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (w widgetFixture) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(w.TenantID) }

var _ dbkit.TenantScoped = widgetFixture{}

const createWidgetFixtureTableSQL = `CREATE TABLE widget_fixtures (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	name VARCHAR(255) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

// TestStandaloneQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant is this
// package's end-to-end proof of the tenant-rebuild guarantee, exercised
// through the REAL worker pool (contrast worker_test.go's
// TestJobContext_* pair, which proves the same mechanism at the unit
// level against jobContext directly, with no database involved): a
// Handler performing a genuine tenant-scoped dbkit.Repository[T] call
// succeeds using ONLY the tenant recorded on the Job -- never any ambient
// context -- because Enqueue itself is called here with
// context.Background(), carrying no tenant at all. If a worker ever
// regressed to NOT rebuilding tenant context before calling Handle, this
// Repository[T] call would fail closed with pkgcore.ErrNoTenant instead of
// finding the seeded row, and this test would fail with that error
// surfacing as the Job's own Error field.
func TestStandaloneQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := db.Exec(createWidgetFixtureTableSQL).Error; err != nil {
		t.Fatalf("create widget_fixtures table: %v", err)
	}
	repo := dbkit.NewRepository[widgetFixture](db)

	const widgetTenant = pkgcore.TenantID("widget-tenant")
	seedCtx := pkgcore.WithTenant(context.Background(), widgetTenant)
	if err := repo.Create(seedCtx, &widgetFixture{ID: "w1", Name: "gizmo"}); err != nil {
		t.Fatalf("seed widget: %v", err)
	}

	q := NewStandaloneQueue(db, WithPollInterval(15*time.Millisecond))
	if err := q.RegisterHandler(NewHandlerFunc("widget.lookup", func(ctx context.Context, _ *Job, _ ProgressFn) (Result, error) {
		// ctx here comes ONLY from the worker's rebuild (see worker.go's
		// jobContext and execute) -- Repository[T].FindByID fails closed
		// with pkgcore.ErrNoTenant if that rebuild is ever skipped, which
		// is exactly what makes this a meaningful proof rather than a
		// tautology.
		w, err := repo.FindByID(ctx, "w1")
		if err != nil {
			return Result{}, err
		}
		return Result{Data: []byte(w.Name)}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	// Enqueued from a context carrying NO tenant at all: if the eventual
	// success below depended on some ambient tenant leaking through
	// instead of the worker's own rebuild, there would be no tenant here
	// for it to leak from.
	id, err := q.Enqueue(context.Background(), Task{Type: "widget.lookup", TenantID: widgetTenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	job := waitTerminal(t, q, pkgcore.WithTenant(context.Background(), widgetTenant), id)
	if job.Status != StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}
	if job.Result == nil || string(job.Result.Data) != "gizmo" {
		t.Errorf("Result = %+v, want Data = %q", job.Result, "gizmo")
	}
}

func TestRegisterQueueDepthGauge_Smoke(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	q := NewStandaloneQueue(db)
	// Register on a test-LOCAL MeterProvider, never the process-global one:
	// the global provider can be installed only once per process (the SDK's
	// first otel.SetMeterProvider wins), and job-metrics tests need that one
	// install for themselves. A local provider exercises the identical
	// registration path with none of the cross-test coupling.
	mp := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := q.registerQueueDepthGauge(mp.Meter(InstrumentationName)); err != nil {
		t.Errorf("registerQueueDepthGauge() error = %v, want nil", err)
	}
}

// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose pins the
// stopped-queue answer of the queue-depth gauge lifecycle: the
// "jobs.queue.depth" ObservableGauge callback cannot be unregistered from
// the meter it was registered on -- the OTel API has no such operation --
// so it keeps running for the life of the process, replayed onto every
// MeterProvider the process ever installs. A queue that has been Close()d,
// and whose database the host closed afterwards, must therefore answer nil
// rather than touch its closed data source. The
// callback and Close are ordered by depthGaugeMu: the callback holds the
// read lock across its stopped-check and its query, and Close holds the
// write lock while signaling stopCh, so once Close returns no callback is
// mid-query and any later callback sees the stopped queue. See
// registerQueueDepthGauge's doc comment for the full lifecycle contract: a
// still-armed callback querying q.db after the host closed it would make
// the second Collect return an error, which fails this test.
func TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	q := NewStandaloneQueue(db)
	if _, err := q.Enqueue(context.Background(), Task{Type: "gauge.probe", TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := q.registerQueueDepthGauge(mp.Meter(InstrumentationName)); err != nil {
		t.Fatalf("registerQueueDepthGauge() error = %v", err)
	}

	// Positive control: while the queue is alive and one job sits pending,
	// a Collect must succeed and must report the backlog.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect #1 (queue alive) error = %v, want nil", err)
	}
	if depth := testutil.MetricByName(t, rm, "jobs.queue.depth"); depth == nil {
		t.Fatalf("Collect #1 missing %q while a job is pending; metrics present: %v", "jobs.queue.depth", testutil.MetricNames(rm))
	} else if g, ok := depth.Data.(metricdata.Gauge[int64]); !ok || len(g.DataPoints) == 0 {
		t.Fatalf("Collect #1 metric %q has no data points, want the pending-job backlog", "jobs.queue.depth")
	}

	// The host-side half of the Close contract: Close returns, THEN the
	// host closes the data source. After both, a Collect must succeed and
	// report nothing for this gauge -- a stopped queue answers nil, and
	// never touches its closed database.
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("closing the queue's database: %v", err)
	}

	rm = metricdata.ResourceMetrics{}
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect #2 (after Close) error = %v, want nil; a closed queue's gauge callback must not touch its closed database", err)
	}
	if depth := testutil.MetricByName(t, rm, "jobs.queue.depth"); depth != nil {
		t.Errorf("Collect #2 still reports %q after Close, want the stopped queue to answer nothing", "jobs.queue.depth")
	}
}

// TestEnqueue_LogsSingleCorrectTenantID_EvenWhenCtxTenantDiffers pins the
// "job enqueued" log line to ONE tenant_id attribute when Enqueue's caller
// context carries a different tenant than Task.TenantID -- the
// platform-level-scheduler shape (a caller enqueuing one cleanup Task per
// tenant in a loop, where no single ambient tenant exists). The logger's
// context is rebuilt from task.TenantID, never from ctx's ambient tenant:
// letting obs.FromContext(ctx) auto-attach the ctx tenant AND logging an
// explicit "tenant_id" kv for task.TenantID would render both side by
// side -- slog.TextHandler does not deduplicate repeated attribute keys,
// and the line would attribute the job to the wrong tenant.
func TestEnqueue_LogsSingleCorrectTenantID_EvenWhenCtxTenantDiffers(t *testing.T) {
	q := newTestQueue(t)

	// ctx's own ambient tenant deliberately differs from the Task being
	// enqueued, mirroring a platform scheduler that itself runs under one
	// context while looping over many tenants' own Tasks.
	ctx := pkgcore.WithTenant(context.Background(), "scheduler-tenant")
	var buf bytes.Buffer
	ctx = obs.WithLogger(ctx, slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := q.Enqueue(ctx, Task{Type: "cleanup", TenantID: "tenant-b"}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, "tenant_id="); n != 1 {
		t.Fatalf("log line has %d tenant_id attributes, want exactly 1; got: %s", n, out)
	}
	if want := "tenant_id=tenant-b"; !strings.Contains(out, want) {
		t.Errorf("log line missing %q (the Job's own owning tenant); got: %s", want, out)
	}
	if strings.Contains(out, "tenant_id=scheduler-tenant") {
		t.Errorf("log line leaked ctx's ambient tenant instead of task.TenantID; got: %s", out)
	}
}

// setupTestMeterProvider installs, as OTel's global MeterProvider for the
// duration of the test, a real SDK MeterProvider backed by a ManualReader
// (never a Prometheus/OTLP exporter -- this file only needs to read back
// exactly what was recorded, not translate it), mirroring
// go/observability/middleware_test.go's own setupMeterProvider in spirit
// (a real SDK provider, not a mock) with a lighter-weight reader since
// there is no Prometheus-naming translation to verify here. Returns the
// reader to Collect from.
func setupTestMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reader
}

// collectMetric runs a fresh Collect and returns the single metric named
// name, failing the test if it is missing -- name is always one of the
// jobDurationMetricName/jobAttemptsMetricName/jobDeadLetterMetricName
// literals queue_standalone.go defines.
//
// A Collect() error is FATAL: the queue-depth gauge lifecycle fix (see
// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose and both
// registerQueueDepthGauge doc comments) made one impossible. This test
// process runs every test in package jobs in one binary sharing one
// process-wide OTel global MeterProvider, and go.opentelemetry.io/otel's
// global package queues every otel.Meter(InstrumentationName) call made
// before the first-ever otel.SetMeterProvider (every OTHER lifecycle test
// in this file that calls Start, none of which install a real provider of
// their own) and replays them onto whatever provider IS eventually
// installed -- this test's own setupTestMeterProvider, if it runs first.
// Those replayed queue-depth callbacks close over long-finished tests'
// queues, but every such queue is Close()d before its database closes
// (startQueue's t.Cleanup, and dbtest.NewSQLite's own LIFO cleanup
// ordering), and a stopped queue's callback answers nil -- so collecting
// this process's full metric set must never error, and a Collect error
// here means the stopped-answer contract has regressed.
func collectMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil (see this helper's own doc comment for why an error is a regression)", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got = append(got, m.Name)
		}
	}
	t.Fatalf("metric %q not found; metrics present: %v", name, got)
	return metricdata.Metrics{}
}

// attrString reads key out of attrs as a plain string, for comparing
// against a metric data point's own Attributes.
func attrString(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

// counterValue returns the int64 Sum value of m's data point labeled
// exactly by jobType and status (status ignored when empty), failing the
// test if m is not a Sum[int64] or no matching data point exists. Use
// this when the data point is expected to exist; a Counter never
// incremented for a given label combination emits no data point at all
// (there is no proactive zero-valued row), so an EXPECTED-ABSENT check
// must use counterValueOrZero instead, not this function.
func counterValue(t *testing.T, m metricdata.Metrics, jobType, status string) int64 {
	t.Helper()
	v, ok := counterValueOrZero(t, m, jobType, status)
	if !ok {
		t.Fatalf("metric %q has no data point for job_type=%q status=%q", m.Name, jobType, status)
	}
	return v
}

// counterValueOrZero is counterValue's non-fatal counterpart: it reports
// (0, false) instead of failing the test when no data point matches, for
// asserting a label combination was deliberately never recorded.
func counterValueOrZero(t *testing.T, m metricdata.Metrics, jobType, status string) (int64, bool) {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if attrString(dp.Attributes, "job_type") != jobType {
			continue
		}
		if status != "" && attrString(dp.Attributes, "status") != status {
			continue
		}
		return dp.Value, true
	}
	return 0, false
}

// histogramCount returns the observation Count of m's data point labeled
// exactly by jobType/status, failing the test if m is not a
// Histogram[float64] or no matching data point exists.
func histogramCount(t *testing.T, m metricdata.Metrics, jobType, status string) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if attrString(dp.Attributes, "job_type") == jobType && attrString(dp.Attributes, "status") == status {
			return dp.Count
		}
	}
	t.Fatalf("metric %q has no data point for job_type=%q status=%q; data points: %+v", m.Name, jobType, status, hist.DataPoints)
	return 0
}

// TestStandaloneQueue_JobMetrics_RecordsDurationAttemptsAndDeadLetter
// proves that StandaloneQueue emits the four must-instrument rows beyond
// queue backlog depth -- execution duration percentiles, failure rate,
// retry count and dead-letter count -- rather than only the
// "jobs.queue.depth" gauge. The negative control is built in: were
// registerJobMetrics/recordJobMetrics/recordDeadLetter not wiring these
// instruments, collectMetric would fail with "metric ... not found" and
// no assertion below would have an instrument to read.
//
// One flaky-then-succeeds Job and one always-fails Job together exercise
// all three Status outcomes execute (worker.go) can reach:
// StatusRetrying (the flaky Job's first failed attempt), StatusSucceeded
// (its eventual success) and StatusDeadLetter (the always-fails Job,
// once MaxRetries is exhausted).
func TestStandaloneQueue_JobMetrics_RecordsDurationAttemptsAndDeadLetter(t *testing.T) {
	reader := setupTestMeterProvider(t)
	q := newTestQueue(t)

	flaky := &flakyHandler{failuresBefore: 1}
	if err := q.RegisterHandler(flaky); err != nil {
		t.Fatalf("RegisterHandler(flaky) error = %v", err)
	}
	alwaysFails := &countingFailureHandler{onFailureCh: make(chan *Job, 1)}
	if err := q.RegisterHandler(alwaysFails); err != nil {
		t.Fatalf("RegisterHandler(alwaysFails) error = %v", err)
	}
	startQueue(t, q)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	flakyID, err := q.Enqueue(context.Background(), Task{Type: "flaky", TenantID: "tenant-a"}, WithMaxRetries(5))
	if err != nil {
		t.Fatalf("Enqueue(flaky) error = %v", err)
	}
	if job := waitTerminal(t, q, ctx, flakyID); job.Status != StatusSucceeded {
		t.Fatalf("flaky job Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}

	deadID, err := q.Enqueue(context.Background(), Task{Type: "always-fails", TenantID: "tenant-a"}, WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue(always-fails) error = %v", err)
	}
	if job := waitTerminal(t, q, ctx, deadID); job.Status != StatusDeadLetter {
		t.Fatalf("always-fails job Status = %v, want %v (job: %+v)", job.Status, StatusDeadLetter, job)
	}

	attempts := collectMetric(t, reader, jobAttemptsMetricName)
	if got := counterValue(t, attempts, "flaky", string(StatusRetrying)); got != 1 {
		t.Errorf("%s{job_type=flaky,status=retrying} = %d, want 1 (retry count)", jobAttemptsMetricName, got)
	}
	if got := counterValue(t, attempts, "flaky", string(StatusSucceeded)); got != 1 {
		t.Errorf("%s{job_type=flaky,status=succeeded} = %d, want 1", jobAttemptsMetricName, got)
	}
	if got := counterValue(t, attempts, "always-fails", string(StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} = %d, want 1 (failure rate numerator)", jobAttemptsMetricName, got)
	}

	deadLetter := collectMetric(t, reader, jobDeadLetterMetricName)
	if got := counterValue(t, deadLetter, "always-fails", ""); got != 1 {
		t.Errorf("%s{job_type=always-fails} = %d, want 1", jobDeadLetterMetricName, got)
	}
	// Negative control: a Job that eventually succeeds must never be
	// counted as dead-lettered. counterValueOrZero, not counterValue: a
	// Counter never Add()-ed for job_type=flaky legitimately has no data
	// point at all, which IS the passing state here, not a test bug.
	if got, found := counterValueOrZero(t, deadLetter, "flaky", ""); found && got != 0 {
		t.Errorf("%s{job_type=flaky} = %d, want 0 (or no data point)", jobDeadLetterMetricName, got)
	}

	duration := collectMetric(t, reader, jobDurationMetricName)
	if got := histogramCount(t, duration, "flaky", string(StatusSucceeded)); got != 1 {
		t.Errorf("%s{job_type=flaky,status=succeeded} count = %d, want 1", jobDurationMetricName, got)
	}
	if got := histogramCount(t, duration, "always-fails", string(StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=always-fails,status=dead_letter} count = %d, want 1", jobDurationMetricName, got)
	}
}

// TestRegisterJobMetrics_Smoke is registerJobMetrics's own equivalent of
// TestRegisterQueueDepthGauge_Smoke immediately above: registration alone
// (no job ever executed) must not error.
func TestRegisterJobMetrics_Smoke(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	q := NewStandaloneQueue(db)
	if err := q.registerJobMetrics(); err != nil {
		t.Errorf("registerJobMetrics() error = %v, want nil", err)
	}
}

// The option-validation regressions below pin one rule, applied uniformly
// to all five With* construction options on StandaloneQueue
// (queue_standalone.go): an invalid value is refused at option time with a
// coded panic -- matching pkgcore's own constructor-time-refusal
// convention (NewSMTPMailer, NewLocalObjectStore, ...) -- never accepted
// and silently reinterpreted, and never left to fail after Start has
// already reported success. What counts as invalid is per-option and
// stated on each With* function's own doc comment, with the reason the
// value is unhonourable: worker counts and the per-tenant concurrency
// limit below 1 (a queue that silently processes nothing), and durations
// at or below zero (a zero poll interval would panic a background ticker
// only AFTER Start had succeeded; a zero or negative timeout or backoff
// cannot be honoured literally and would silently collapse onto the
// default, or into an immediate retry burst). The completeness claim is
// deliberate: every one of the five options in queue_standalone.go has a
// regression here, so the suite really does cover each option's
// invalid-value behaviour, and a new constructor option cannot be added to
// that file without landing its refusal test beside it. The per-Enqueue
// options in queue.go are a separate layer with their own individually
// documented rules -- WithMaxRetries clamps, WithTimeout falls back -- and
// are not what these tests pin.

// assertOptionPanics asserts that fn panics with a coded *apperr.Error
// carrying code -- the option-time refusal contract every With*
// construction option in queue_standalone.go shares.
func assertOptionPanics(t *testing.T, code string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s: expected a coded panic %q, got none", t.Name(), code)
		}
		e, ok := r.(*apperr.Error)
		if !ok {
			t.Fatalf("%s: panic value = %T(%v), want a coded *apperr.Error %q", t.Name(), r, r, code)
		}
		if e.Code != code {
			t.Fatalf("%s: panic code = %q, want %q", t.Name(), e.Code, code)
		}
	}()
	fn()
}

// TestWithWorkerCount_ZeroOrNegative_Refused pins the WithWorkerCount(0)
// refusal: a queue with no workers would claim every eligible Job into
// StatusRunning and execute none of them -- a silent no-op that looks like
// a healthy queue in every metric except the ones that never move.
func TestWithWorkerCount_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithWorkerCount(0) })
	assertOptionPanics(t, "jobs.worker_count_zero", func() { WithWorkerCount(-4) })
}

// TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused pins the
// WithTenantConcurrencyLimit(0) refusal: a limit of zero would refuse
// every tenant admission forever. Same fail-before shape as the worker
// count test.
func TestWithTenantConcurrencyLimit_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(0) })
	assertOptionPanics(t, "jobs.tenant_concurrency_limit_zero", func() { WithTenantConcurrencyLimit(-1) })
}

// TestWithPollInterval_ZeroOrNegative_Refused pins the WithPollInterval(0)
// refusal against the crash shape construction-time validation exists to
// prevent: accepted silently, a zero poll interval would let Start report
// success and then the dispatcher goroutine's time.NewTicker would panic
// and kill the whole process.
func TestWithPollInterval_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.poll_interval_zero", func() { WithPollInterval(0) })
	assertOptionPanics(t, "jobs.poll_interval_zero", func() { WithPollInterval(-5 * time.Millisecond) })
}

// TestWithJobTimeout_ZeroOrNegative_Refused pins the WithJobTimeout(0)
// refusal: a non-positive timeout is this package's "not set" marker, so a
// zero or negative configured default could never be honoured literally --
// it would silently leave every Job on DefaultTimeout, indistinguishable in
// operation from an option that was never passed.
func TestWithJobTimeout_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(0) })
	assertOptionPanics(t, "jobs.job_timeout_zero", func() { WithJobTimeout(-1 * time.Second) })
}

// TestWithBackoff_ZeroOrNegative_Refused pins the WithBackoff zero-or-
// negative refusal on each bound: backoffDelay would return a zero or
// negative delay from such a configuration, collapsing the exponential
// spread into an immediate retry burst at the poll cadence until retries
// are exhausted -- the opposite of what the option exists to configure.
func TestWithBackoff_ZeroOrNegative_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.backoff_base_zero", func() { WithBackoff(0, DefaultBackoffMax) })
	assertOptionPanics(t, "jobs.backoff_base_zero", func() { WithBackoff(-1*time.Second, DefaultBackoffMax) })
	assertOptionPanics(t, "jobs.backoff_max_zero", func() { WithBackoff(DefaultBackoffBase, 0) })
	assertOptionPanics(t, "jobs.backoff_max_zero", func() { WithBackoff(DefaultBackoffBase, -1*time.Second) })
}

// TestWithEventBus_Nil_Refused pins the WithEventBus(nil) refusal: omitting
// the option already means "publish nothing", so an explicit nil can only be
// a caller belief that a bus is wired when none is, and the two must not
// silently collapse -- the queue would look signal-wired while publishing
// nothing.
func TestWithEventBus_Nil_Refused(t *testing.T) {
	assertOptionPanics(t, "jobs.event_bus_nil", func() { WithEventBus(nil) })
}
