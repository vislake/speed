package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// testSystemPurpose is the pkgcore.SystemPurpose these tests declare for
// exercising the pkgcore.WithSystemContext branch of callerMayAccess.
const testSystemPurpose = pkgcore.SystemPurpose("jobs.test_system_access")

// newTestQueue returns a DemoQueue backed by a private, per-test temp-file
// SQLite database with the jobs schema already applied -- but NOT started:
// callers that need Enqueue calls to land before the dispatcher's first
// poll tick (TestPriorityOrdering, in particular) construct with this,
// finish every Enqueue call they need, and only then call startQueue.
// Poll interval and backoff are both set short so tests observe outcomes
// quickly; every value remains overridable via opts.
func newTestQueue(t *testing.T, opts ...Option) *DemoQueue {
	t.Helper()
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	defaults := []Option{
		WithPollInterval(15 * time.Millisecond),
		WithBackoff(20*time.Millisecond, 200*time.Millisecond),
	}
	return NewDemoQueue(db, append(defaults, opts...)...)
}

// startQueue starts q and registers a bounded Close via t.Cleanup.
func startQueue(t *testing.T, q *DemoQueue) {
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
func pollJob(t *testing.T, q *DemoQueue, ctx context.Context, id JobID, timeout time.Duration, done func(*Job) bool) *Job {
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
func waitTerminal(t *testing.T, q *DemoQueue, ctx context.Context, id JobID) *Job {
	t.Helper()
	return pollJob(t, q, ctx, id, 3*time.Second, func(j *Job) bool { return j.Status.Terminal() })
}

func TestDemoQueue_StartAndClose_Lifecycle(t *testing.T) {
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
	q := NewDemoQueue(nil)
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
// TestDemoQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant against a
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

// TestDemoQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant is this
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
func TestDemoQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant(t *testing.T) {
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

	q := NewDemoQueue(db, WithPollInterval(15*time.Millisecond))
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
	q := NewDemoQueue(db)
	if err := q.registerQueueDepthGauge(); err != nil {
		t.Errorf("registerQueueDepthGauge() error = %v, want nil", err)
	}
}
