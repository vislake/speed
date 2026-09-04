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
	return pollJob(t, q, ctx, id, 3*time.Second, func(j *Job) bool { return j.Status.Terminal() })
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
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrDuplicateHandlerType.Code {
		t.Fatalf("second RegisterHandler() error = %v, want ErrDuplicateHandlerType", err)
	}
}

func TestEnqueue_InvalidTask_ReturnsError(t *testing.T) {
	q := newTestQueue(t)
	_, err := q.Enqueue(context.Background(), Task{})
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrInvalidTask.Code {
		t.Fatalf("Enqueue(Task{}) error = %v, want ErrInvalidTask", err)
	}
}

func TestEnqueue_Get_HappyPath(t *testing.T) {
	q := newTestQueue(t)
	if err := q.RegisterHandler(NewHandlerFunc("echo", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		return Result{Data: job.Payload}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	id, err := q.Enqueue(context.Background(), Task{Type: "echo", TenantID: "tenant-a", Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q, ctx, id)
	if job.Status != StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}
	if job.Result == nil || string(job.Result.Data) != "hello" {
		t.Errorf("Result = %+v, want Data = %q", job.Result, "hello")
	}
	if job.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", job.Attempts)
	}
	if job.CompletedAt == nil {
		t.Error("CompletedAt = nil, want set")
	}
}

// TestGet_TenantIsolation needs no running dispatcher: Get reads directly,
// so the enqueued Job is simply left StatusPending forever.
func TestGet_TenantIsolation(t *testing.T) {
	q := newTestQueue(t)
	id, err := q.Enqueue(context.Background(), Task{Type: "noop", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	ownerCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	_, err = q.Get(ownerCtx, id)
	if err != nil {
		t.Errorf("Get() with the owning tenant error = %v, want nil", err)
	}

	otherCtx := pkgcore.WithTenant(context.Background(), "tenant-b")
	_, err = q.Get(otherCtx, id)
	if !isJobNotFound(err) {
		t.Errorf("Get() with a different tenant error = %v, want ErrJobNotFound", err)
	}

	_, err = q.Get(context.Background(), id)
	if !isJobNotFound(err) {
		t.Errorf("Get() with no tenant and no system context error = %v, want ErrJobNotFound", err)
	}

	pkgcore.RegisterSystemPurpose(testSystemPurpose)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Actor: "test", Purpose: testSystemPurpose})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	_, err = q.Get(sysCtx, id)
	if err != nil {
		t.Errorf("Get() with a system context error = %v, want nil", err)
	}

	_, err = q.Get(ownerCtx, JobID("no-such-id"))
	if !isJobNotFound(err) {
		t.Errorf("Get() for a nonexistent id error = %v, want ErrJobNotFound", err)
	}
}

// TestCancel_TenantIsolation_And_Idempotency also needs no running
// dispatcher: WithDelay(time.Hour) keeps the Job safely StatusPending for
// the duration of the test.
func TestCancel_TenantIsolation_And_Idempotency(t *testing.T) {
	q := newTestQueue(t)
	id, err := q.Enqueue(context.Background(), Task{Type: "noop", TenantID: "tenant-a"}, WithDelay(time.Hour))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	otherCtx := pkgcore.WithTenant(context.Background(), "tenant-b")
	err = q.Cancel(otherCtx, id)
	if !isJobNotFound(err) {
		t.Errorf("Cancel() from a different tenant error = %v, want ErrJobNotFound", err)
	}

	ownerCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	err = q.Cancel(ownerCtx, id)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	job, err := q.Get(ownerCtx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v", job.Status, StatusCancelled)
	}

	err = q.Cancel(ownerCtx, id)
	if err != nil {
		t.Errorf("second Cancel() error = %v, want nil (idempotent)", err)
	}
}

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

	id, err := q.Enqueue(context.Background(), Task{Type: "delayed", TenantID: "tenant-a"}, WithDelay(300*time.Millisecond))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if ran.Load() {
		t.Fatal("handler ran before its ScheduledAt time")
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job, err := q.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if job.Status != StatusPending {
		t.Errorf("Status before the delay elapses = %v, want %v", job.Status, StatusPending)
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

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	q := newTestQueue(t)
	h := &flakyHandler{failuresBefore: 2}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	id, err := q.Enqueue(context.Background(), Task{Type: "flaky", TenantID: "tenant-a"}, WithMaxRetries(5))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q, ctx, id)
	if job.Status != StatusSucceeded {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}
	if job.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (2 failures + 1 success)", job.Attempts)
	}
	if job.Error != "" {
		t.Errorf("Error = %q, want empty after eventual success", job.Error)
	}
	if h.onFailureCalls.Load() != 0 {
		t.Errorf("onFailureCalls = %d, want 0: FailureHook must not fire for a Job that eventually succeeds", h.onFailureCalls.Load())
	}
}

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

func TestDeadLetter_ExhaustsRetries_And_InvokesFailureHook(t *testing.T) {
	q := newTestQueue(t)
	h := &countingFailureHandler{onFailureCh: make(chan *Job, 1)}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	startQueue(t, q)

	id, err := q.Enqueue(context.Background(), Task{Type: "always-fails", TenantID: "tenant-a"}, WithMaxRetries(1))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q, ctx, id)
	if job.Status != StatusDeadLetter {
		t.Fatalf("Status = %v, want %v (job: %+v)", job.Status, StatusDeadLetter, job)
	}
	if job.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (1 initial + 1 retry, MaxRetries=1)", job.Attempts)
	}
	if job.Error != "permanent failure" {
		t.Errorf("Error = %q, want %q", job.Error, "permanent failure")
	}

	select {
	case hookJob := <-h.onFailureCh:
		if hookJob.ID != id {
			t.Errorf("OnFailure job.ID = %q, want %q", hookJob.ID, id)
		}
		if hookJob.Status != StatusDeadLetter {
			t.Errorf("OnFailure job.Status = %v, want %v", hookJob.Status, StatusDeadLetter)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("FailureHook.OnFailure was never called")
	}

	dead, err := q.DeadLetterJobs(ctx)
	if err != nil {
		t.Fatalf("DeadLetterJobs() error = %v", err)
	}
	found := false
	for _, j := range dead {
		if j.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("DeadLetterJobs() = %v, want it to include %q", dead, id)
	}
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

	var firstStarted JobID
	select {
	case firstStarted = <-flood.startedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("no flood job started at all")
	}

	// tenant-b's job completes promptly despite tenant-a's flood already
	// occupying a worker -- the core proof that one tenant cannot starve
	// another.
	select {
	case <-quickDone:
	case <-time.After(1 * time.Second):
		t.Fatal("tenant-b's job never completed while tenant-a's flood held the queue")
	}

	// A second tenant-a job must NOT start while the first is still
	// running: tenant-a is at its concurrency limit of 1.
	select {
	case second := <-flood.startedCh:
		t.Fatalf("a second tenant-a job (%q) started while the first (%q) was still running; the per-tenant concurrency limit was not enforced", second, firstStarted)
	case <-time.After(200 * time.Millisecond):
	}

	flood.releaseCh <- struct{}{}
	select {
	case <-flood.startedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("second flood job never started after the first was released")
	}
	flood.releaseCh <- struct{}{}
	select {
	case <-flood.startedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("third flood job never started after the second was released")
	}
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

	id, err := q.Enqueue(context.Background(), Task{Type: "progress", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	select {
	case <-h.afterFirstReport:
	case <-time.After(1 * time.Second):
		t.Fatal("handler never reported its first progress update")
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	mid := pollJob(t, q, ctx, id, 1*time.Second, func(j *Job) bool { return j.ProgressPct == 30 })
	if mid.ProgressMsg != "step one" {
		t.Errorf("mid-flight ProgressMsg = %q, want %q", mid.ProgressMsg, "step one")
	}
	if mid.Status != StatusRunning {
		t.Errorf("Status while progress is mid-flight = %v, want %v", mid.Status, StatusRunning)
	}

	close(h.resume)
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
// package's end-to-end proof of AGENTS.md's central guarantee, exercised
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

// TestStandaloneQueue_DepthGauge_StopsQueryingAfterClose is the regression
// proof for the queue-depth gauge lifecycle defect the reference app's own
// wiring surfaced (its obs.Init shutdown failed with "jobs: query queue
// depth: sql: database is closed"): the "jobs.queue.depth" ObservableGauge
// callback cannot be unregistered from the meter it was registered on -- the
// OTel API has no such operation -- so it keeps running for the life of the
// process, replayed onto every MeterProvider the process ever installs. A
// queue that has been Close()d, and whose database the host has since closed,
// must therefore answer nil rather than touch its closed data source. The
// callback and Close are ordered by depthGaugeMu: the callback holds the
// read lock across its stopped-check and its query, and Close holds the
// write lock while signaling stopCh, so once Close returns no callback is
// mid-query and any later callback sees the stopped queue. See
// registerQueueDepthGauge's doc comment for the full lifecycle contract.
//
// Before the fix, the second Collect returned an error from the still-armed
// callback querying q.db after the host had closed it.
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

// TestEnqueue_LogsSingleCorrectTenantID_EvenWhenCtxTenantDiffers is the
// regression proof for the "job enqueued" log line carrying two
// conflicting tenant_id attributes whenever Enqueue's caller context
// carries a different tenant than Task.TenantID -- AGENTS.md's own
// "platform-level scheduler enqueuing one cleanup Task per tenant in a
// loop" example is exactly this shape (see "Why Task carries its own
// TenantID instead of Enqueue resolving it from ctx"). Before the fix,
// obs.FromContext(ctx) auto-attached ctx's own ambient tenant AND Enqueue
// additionally logged an explicit "tenant_id" kv for task.TenantID, so
// the rendered line carried the ctx tenant (wrong for this log line) and
// the Job's own tenant (right) side by side -- slog.TextHandler does not
// deduplicate repeated attribute keys, so both survived verbatim.
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
// literals standalone_queue.go defines.
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

// TestStandaloneQueue_JobMetrics_RecordsDurationAttemptsAndDeadLetter is the
// regression proof that StandaloneQueue actually emits the four
// docs/internal/09-observability.md must-instrument rows beyond queue
// backlog depth -- execution duration percentiles, failure rate, retry
// count and dead-letter count -- rather than only the "jobs.queue.depth"
// gauge. Before registerJobMetrics/recordJobMetrics/recordDeadLetter
// existed, none of the three assertions below had any instrument to read
// back at all: collectMetric itself would fail with "metric ... not
// found", which is the negative control this test relies on (there is no
// separate "before" build to run it against, since the instruments
// literally did not exist).
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
