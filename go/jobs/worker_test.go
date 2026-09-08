package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// fixtureRunningRecord is fixtureRecord (store_test.go) plus the one
// override every test below needs: execute's own doc comment says it runs
// "exactly one Handle attempt for rec (already StatusRunning in the
// database)" -- completeSucceeded/completeRetrying/completeDeadLetter all
// guard their update with `WHERE status = 'running'`, so calling execute
// directly against a row fixtureRecord left StatusPending would silently
// no-op every one of those writes instead of exercising them.
//
// A real worker hands execute a record whose attempt was already counted
// by the handoff (runAttempt increments rec.Attempts and persists it via
// markAttemptStarted before calling execute); tests that seed rec.Attempts
// = 1 mirror exactly that post-handoff state. The record's ClaimedBy is
// left empty: execute never reads it.
func fixtureRunningRecord(tenant pkgcore.TenantID, jobType string) *jobRecord {
	rec := fixtureRecord(tenant, jobType)
	rec.Status = string(StatusRunning)
	return rec
}

// TestJobContext_ProducesTenantScopedContext is the positive half of "the
// tenant context trap": jobContext(tenant) must produce a context from
// which the Job's own tenant is recoverable, exactly what a worker hands
// to Handler.Handle.
func TestJobContext_ProducesTenantScopedContext(t *testing.T) {
	ctx := jobContext(pkgcore.TenantID("tenant-a"))

	got, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		t.Fatalf("MustTenantFromContext(jobContext(...)) error = %v, want nil", err)
	}
	if got != "tenant-a" {
		t.Errorf("tenant = %q, want %q", got, "tenant-a")
	}
}

// TestJobContext_IgnoresAmbientTenant proves jobContext takes no base
// context at all -- it is rooted in a fresh context.Background() every
// call -- so it can never inherit a stale or unrelated tenant from
// whatever context happens to be available at the call site. This is the
// "not inherited from the original Enqueue call's context" half of the
// tenant-context trap: that original context is long gone by
// the time a worker goroutine picks the Job back up out of SQLite, so
// jobContext does not even accept one to (mis)use.
func TestJobContext_IgnoresAmbientTenant(t *testing.T) {
	ctx := jobContext(pkgcore.TenantID("right-tenant"))
	got, ok := pkgcore.TenantFromContext(ctx)
	if !ok || got != "right-tenant" {
		t.Errorf("tenant = (%q, %v), want (%q, true)", got, ok, "right-tenant")
	}
}

// TestJobContext_ContrastWithoutRebuild_FailsClosedWithErrNoTenant is the
// negative half: the exact failure mode a worker reproduces if it EVER
// calls Handle with a context other than jobContext's own output --
// including, but not limited to, forgetting to call it at all and using a
// bare context.Background(). Any such context carries no tenant, so a
// Handler's tenant-scoped operation (dbkit.Repository[T] underneath, or
// pkgcore.MustTenantFromContext directly) fails closed with
// pkgcore.ErrNoTenant instead of silently running unscoped or against the
// wrong tenant.
//
// If a change to execute/runWorker ever stops routing through jobContext,
// THIS is the test that must fail -- with this exact, well-labeled name --
// rather than some unrelated Handler mysteriously erroring in production.
// See standalone_queue_test.go's
// TestStandaloneQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant for
// the same guarantee proved end to end through a real worker and a real
// dbkit.Repository[T] call.
func TestJobContext_ContrastWithoutRebuild_FailsClosedWithErrNoTenant(t *testing.T) {
	brokenCtx := context.Background() // what a worker gets if it skips jobContext entirely

	_, err := pkgcore.MustTenantFromContext(brokenCtx)
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("MustTenantFromContext(context.Background()) error = %v, want a wrapped pkgcore.ErrNoTenant", err)
	}

	// Contrast: the identical check against jobContext's own output
	// succeeds -- the only difference is whether jobContext was called.
	fixedCtx := jobContext(pkgcore.TenantID("tenant-a"))
	if _, err := pkgcore.MustTenantFromContext(fixedCtx); err != nil {
		t.Fatalf("MustTenantFromContext(jobContext(...)) error = %v, want nil", err)
	}
}

func TestBackoffDelay(t *testing.T) {
	q := NewStandaloneQueue(nil, WithBackoff(1*time.Second, 10*time.Second))

	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 1 * time.Second}, // treated as 1
		{attempts: 1, want: 1 * time.Second},
		{attempts: 2, want: 2 * time.Second},
		{attempts: 3, want: 4 * time.Second},
		{attempts: 4, want: 8 * time.Second},
		{attempts: 5, want: 10 * time.Second}, // would be 16s uncapped
		{attempts: 10, want: 10 * time.Second},
	}
	for _, tt := range tests {
		if got := q.backoffDelay(tt.attempts); got != tt.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestTenantSlotReservation(t *testing.T) {
	q := NewStandaloneQueue(nil, WithTenantConcurrencyLimit(2))
	tenant := pkgcore.TenantID("tenant-a")

	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("first reservation should succeed")
	}
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("second reservation should succeed: limit is 2")
	}
	if q.tryReserveTenantSlot(tenant) {
		t.Fatal("third reservation should fail: limit reached")
	}

	other := pkgcore.TenantID("tenant-b")
	if !q.tryReserveTenantSlot(other) {
		t.Fatal("a different tenant's reservation must not be affected by tenant-a's limit")
	}

	q.releaseTenantSlot(tenant)
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("after a release, a new reservation should succeed again")
	}
}

// TestTenantSlotReservation_ConcurrentAccessIsRaceFree exercises
// tryReserveTenantSlot/releaseTenantSlot from many goroutines at once:
// runDispatcher and runWorker call these concurrently by construction (the
// dispatcher increments, N worker goroutines decrement), so this is the
// package's concurrency hot spot and runs under -race.
func TestTenantSlotReservation_ConcurrentAccessIsRaceFree(t *testing.T) {
	q := NewStandaloneQueue(nil, WithTenantConcurrencyLimit(3))
	tenant := pkgcore.TenantID("tenant-a")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.tryReserveTenantSlot(tenant) {
				q.releaseTenantSlot(tenant)
			}
		}()
	}
	wg.Wait()

	q.tenantMu.Lock()
	remaining := q.runningPerTenant[tenant]
	q.tenantMu.Unlock()
	if remaining != 0 {
		t.Errorf("runningPerTenant[tenant] = %d, want 0 once every goroutine released what it reserved", remaining)
	}
}

// TestExecute_EmptyTenantID_FailsClosedWithoutCallingHandle pins that
// execute refuses an attempt for a jobRecord whose TenantID column is
// empty, mirroring asynq.Queue's processTaskUncancelled refusal for the
// identical case (errTaskMissingTenant, queue/asynq/worker.go).
// Task.validate blocks Enqueue itself from ever creating such a row, so
// the corrupted row here is seeded directly through q.db -- simulating a
// row written by anything other than Enqueue: a migration bug, a manual
// SQL fixup, or a writer that bypasses this package's own API. See
// errStandaloneJobMissingTenant's own doc comment (worker.go) for why this
// matters even though the public API cannot produce the row.
func TestExecute_EmptyTenantID_FailsClosedWithoutCallingHandle(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	handleInvoked := false
	h := NewHandlerFunc("corrupt.tenant", func(context.Context, *Job, ProgressFn) (Result, error) {
		handleInvoked = true
		return Result{}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("", "corrupt.tenant") // TenantID deliberately empty
	rec.ClaimedBy = q.owner                           // seeded under this queue's own claim, like every direct-execute seed below
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed corrupted record: %v", err)
	}

	q.execute(*rec)

	if handleInvoked {
		t.Error("execute() invoked Handle for a jobRecord with an empty TenantID; want it to fail closed without ever calling Handle")
	}

	// A Job owned by no tenant is unreachable through any ordinary tenant
	// context (pkgcore.WithTenant(ctx, "") is never reported as a usable
	// tenant -- see pkgcore.TenantFromContext) -- exactly the "only a
	// system context or a raw table scan would ever reveal it ran"
	// property the review flagged, so a system context is the only way
	// this test can read the outcome back.
	pkgcore.RegisterSystemPurpose(testSystemPurpose)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor: "test", Purpose: testSystemPurpose, Ticket: "TEST-1",
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	got, err := q.Get(sysCtx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status == StatusSucceeded {
		t.Fatal("Status = StatusSucceeded: a Job with an empty TenantID must never be allowed to succeed")
	}
	if got.Status != StatusRetrying && got.Status != StatusDeadLetter {
		t.Errorf("Status = %v, want StatusRetrying or StatusDeadLetter (an ordinary Handle failure)", got.Status)
	}
	if got.Error != errStandaloneJobMissingTenant.Code {
		t.Errorf("Error = %q, want %q", got.Error, errStandaloneJobMissingTenant.Code)
	}
}

// panickingHandler always panics from Handle, simulating a Handler
// implementation bug: a nil dereference, an out-of-range slice index
// against a malformed Job.Payload, a failed type assertion, a panicking
// third-party dependency.
type panickingHandler struct{}

func (panickingHandler) Type() string { return "panics.always" }

func (panickingHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	panic("adversarial: simulated bug in a Handler")
}

var _ Handler = panickingHandler{}

// TestExecute_HandlerPanic_RecoversInsteadOfCrashingProcess pins that
// execute's call path recovers a Handler panic the way asynq's own
// processor.perform protects the equivalent call for asynq.Queue. Without
// invokeHandle's recover, this test would crash the ENTIRE test binary
// rather than merely failing one assertion -- exactly as an unrecovered
// panic crashes the entire worker-pool process in production, taking every
// OTHER tenant's in-flight and queued Jobs down with it: the modules
// compile into one binary, so no process boundary contains the crash. The
// outer defer/recover below exists only to turn such a crash into a
// well-labeled t.Fatal instead of a bare process exit, should this ever
// regress; it does not run today, since invokeHandle (worker.go) already
// recovers the panic before it reaches this test.
func TestExecute_HandlerPanic_RecoversInsteadOfCrashingProcess(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(panickingHandler{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "panics.always")
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("execute() let the Handler's panic escape instead of recovering it: %v", r)
			}
		}()
		q.execute(*rec)
	}()

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusRetrying && got.Status != StatusDeadLetter {
		t.Errorf("Status = %v, want StatusRetrying or StatusDeadLetter -- a panic must be handled as an ordinary Handle failure", got.Status)
	}
	if got.Error == "" {
		t.Error(`Error = "", want a non-empty message recording the panic`)
	}
}

// panickingFailureHook always fails Handle (so a Job exhausts retries and
// dead-letters) and panics from OnFailure once invoked, simulating a
// business-module bug inside failure compensation itself -- a
// credit-refund call against a malformed Job.Payload, for example.
type panickingFailureHook struct{}

func (panickingFailureHook) Type() string { return "panics.on_failure" }

func (panickingFailureHook) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, errors.New("permanent failure")
}

func (panickingFailureHook) OnFailure(context.Context, *Job, error) {
	panic("adversarial: simulated bug in OnFailure")
}

var (
	_ Handler     = panickingFailureHook{}
	_ FailureHook = panickingFailureHook{}
)

// TestExecute_FailureHookPanic_RecoversInsteadOfCrashingProcess is the
// FailureHook.OnFailure half of the same panic-recovery gap: the review's
// suggested fix direction explicitly calls out OnFailure alongside Handle
// ("if it can also panic"), since it is exactly as much a
// business-module-authored callback and just as capable of panicking. Same
// crash-before/recovers-after shape as
// TestExecute_HandlerPanic_RecoversInsteadOfCrashingProcess, exercised
// through the dead-letter path instead of the ordinary-retry path.
func TestExecute_FailureHookPanic_RecoversInsteadOfCrashingProcess(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(panickingFailureHook{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "panics.on_failure")
	rec.MaxRetries = 0 // exhausted on the very first attempt
	rec.Attempts = 1   // matches the post-handoff state: runAttempt counted this first attempt
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("execute() let OnFailure's panic escape instead of recovering it: %v", r)
			}
		}()
		q.execute(*rec)
	}()

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusDeadLetter {
		t.Errorf("Status = %v, want StatusDeadLetter", got.Status)
	}
}

// cancelledBeforeDeadLetterHandler always fails from Handle (so a Job with
// MaxRetries == 0 exhausts retries on the first attempt) and records every
// OnFailure call it receives on onFailureCh -- the execute-level counterpart
// of standalone_queue_test.go's controlledFailureHandler, for tests that
// drive the dead-letter failure path directly instead of through a live
// worker.
type cancelledBeforeDeadLetterHandler struct {
	onFailureCh chan struct{}
}

func (*cancelledBeforeDeadLetterHandler) Type() string { return "cancel-race.dead_letter" }

func (*cancelledBeforeDeadLetterHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{}, errors.New("permanent failure")
}

func (h *cancelledBeforeDeadLetterHandler) OnFailure(context.Context, *Job, error) {
	h.onFailureCh <- struct{}{}
}

var (
	_ Handler     = (*cancelledBeforeDeadLetterHandler)(nil)
	_ FailureHook = (*cancelledBeforeDeadLetterHandler)(nil)
)

// TestExecute_FinalFailureAfterCancel_DoesNotRunOnFailure is the regression
// test pins that execute does NOT run a FailureHook's OnFailure when a
// concurrent Cancel has already moved the Job to StatusCancelled: the
// dead-letter write is a status-guarded no-op in that case (RowsAffected ==
// 0, nil error), and business compensation for a Job the caller
// deliberately cancelled would violate FailureHook's own
// contract (handler.go) that OnFailure runs only after StatusDeadLetter is
// actually persisted. Deterministic by construction -- markCancelled lands
// before execute's failure path runs, so completeDeadLetter must report no
// transition, no OnFailure may run, and the persisted terminal state must
// stay StatusCancelled; OnFailure running anyway fails the test. See
// store_test.go's
// TestCompleteDeadLetter_NoTransitionWhenAlreadyCancelled for the
// store-level half of the same race, and standalone_queue_test.go's
// TestStandaloneQueue_CancelBeatsFinalFailure_NoOnFailure_DeadLetterNeverPersisted
// for the live-worker half.
func TestExecute_FinalFailureAfterCancel_DoesNotRunOnFailure(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	onFailureCh := make(chan struct{}, 1)
	h := &cancelledBeforeDeadLetterHandler{onFailureCh: onFailureCh}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "cancel-race.dead_letter")
	rec.MaxRetries = 0 // exhausted on the very first attempt
	rec.Attempts = 1   // matches the post-handoff state: runAttempt counted this first attempt
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}

	// The race execute loses: Cancel (markCancelled) persists
	// StatusCancelled before the final attempt's failure path runs.
	if err := markCancelled(context.Background(), q.db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}

	q.execute(*rec)

	select {
	case <-onFailureCh:
		t.Fatal("OnFailure ran for a Job a concurrent Cancel already moved to StatusCancelled; the failure outcome of a cancelled Job must be discarded, never compensated")
	default:
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v (the dead-letter write must not overwrite the cancellation)", got.Status, StatusCancelled)
	}
}

// TestExecute_FinalFailureAfterCancel_RecordsNoDeadLetterLogOrMetric pins
// the ordering rule that execute's dead-letter branch records its log line
// and metrics -- the "job exhausted retries, moving to dead letter" log,
// the jobs.job.dead_letter counter, and the StatusDeadLetter rows of the
// attempts/duration instruments -- only strictly AFTER completeDeadLetter's
// transition report, and only for a genuine running -> dead-letter move. A
// record emitted ahead of the write would survive a no-op write (a
// concurrent Cancel settling the Job) and show a cancelled Job as
// dead-lettered although no dead-letter was ever persisted. A cancelled
// Job gets exactly one truthful record: the "job cancelled before its
// outcome could be recorded, outcome discarded" Info line carrying the
// discarded_outcome=dead_letter attribute; the log line or either metric
// instrument firing before the no-op write is discovered fails this
// test.
func TestExecute_FinalFailureAfterCancel_RecordsNoDeadLetterLogOrMetric(t *testing.T) {
	reader := setupTestMeterProvider(t)
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.registerJobMetrics(); err != nil {
		t.Fatalf("registerJobMetrics() error = %v", err)
	}

	// Capture execute's log lines: execute derives its logger from
	// obs.FromContext over a freshly built context carrying no attached
	// logger, which falls back to slog.Default() read fresh per call
	// (go/observability's FromContext contract) -- so a temporary
	// slog.SetDefault is the module's established capture seam for worker
	// logging. No test in this package runs in parallel, so the process-wide
	// swap cannot leak into a concurrent test.
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	h := &cancelledBeforeDeadLetterHandler{onFailureCh: make(chan struct{}, 1)}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const jobType = "cancel-race.dead_letter"

	// Control first: a genuine dead-letter must still be logged and counted
	// exactly once. It also guarantees the metric data points exist to read
	// back -- a Counter never Add()-ed emits no data point at all, which
	// would make the cancelled Job's expected absence indistinguishable from
	// a never-instrumented run.
	control := fixtureRunningRecord("tenant-a", jobType)
	control.MaxRetries = 0 // exhausted on the very first attempt
	control.Attempts = 1
	// Seeded under this queue's own claim, like the sibling tests above:
	// the dead-letter write the control's genuine transition depends on
	// carries claimed_by = owner.
	control.ClaimedBy = q.owner
	if err := q.db.Create(control).Error; err != nil {
		t.Fatalf("seed control running record: %v", err)
	}
	q.execute(*control)

	// The cancelled Job: Cancel (markCancelled) settles the row before the
	// final attempt's failure path runs -- the same deterministic race
	// TestExecute_FinalFailureAfterCancel_DoesNotRunOnFailure constructs.
	rec := fixtureRunningRecord("tenant-a", jobType)
	rec.MaxRetries = 0 // exhausted on the very first attempt
	rec.Attempts = 1
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	if err := markCancelled(context.Background(), q.db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}
	q.execute(*rec)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	got, err := q.Get(ctx, JobID(rec.ID))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != StatusCancelled {
		t.Errorf("Status = %v, want %v (the dead-letter write must not overwrite the cancellation)", got.Status, StatusCancelled)
	}

	deadLetter := collectMetric(t, reader, jobDeadLetterMetricName)
	if got := counterValue(t, deadLetter, jobType, ""); got != 1 {
		t.Errorf("%s{job_type=%s} = %d, want 1 (the control's genuine dead-letter only; the cancelled Job's discarded failure must not be counted)", jobDeadLetterMetricName, jobType, got)
	}
	attempts := collectMetric(t, reader, jobAttemptsMetricName)
	if got := counterValue(t, attempts, jobType, string(StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=%s,status=dead_letter} = %d, want 1 (the control's genuine dead-letter only)", jobAttemptsMetricName, jobType, got)
	}
	duration := collectMetric(t, reader, jobDurationMetricName)
	if got := histogramCount(t, duration, jobType, string(StatusDeadLetter)); got != 1 {
		t.Errorf("%s{job_type=%s,status=dead_letter} count = %d, want 1 (the control's genuine dead-letter only)", jobDurationMetricName, jobType, got)
	}

	out := buf.String()
	if strings.Contains(out, "moving to dead letter") {
		t.Error("dead-letter log fired before completeDeadLetter's transition report; a cancelled Job must never be logged as moving to dead letter")
	}
	if got := strings.Count(out, "moved to dead letter"); got != 1 {
		t.Errorf("dead-letter log line appears %d times, want exactly 1 (the control's genuine dead-letter only)", got)
	}
	if got := strings.Count(out, "job cancelled before its outcome could be recorded, outcome discarded"); got != 1 {
		t.Errorf("cancelled-outcome Info line appears %d times, want exactly 1 (the one truthful record a cancelled Job's discarded failure is allowed to emit)", got)
	}
	if !strings.Contains(out, "discarded_outcome=dead_letter") {
		t.Error("cancelled-outcome Info line lacks the discarded_outcome=dead_letter attribute naming the dead-letter record the cancellation discarded")
	}
}

// TestExecute_NoOpOutcomeWrite_RowStolenByAnotherWriter_LogsHonestlyNotCancelled
// pins the classification execute's three outcome branches give a no-op'd
// outcome write (moved == false): only a row that is actually
// StatusCancelled may be logged as "job cancelled before its ..." -- a
// stolen row no-ops the identical write, and a blanket cancellation
// explanation would erase the very first evidence of a double execution.
// After a writer-gate lapse, a second queue's resetInterruptedRecords flips
// the first queue's mid-Handle row back to StatusPending and re-claims it,
// so the first queue's outcome write finds no running row: execute must
// probe the row and log the cancellation explanation only when the row is
// actually StatusCancelled (the cancel marker verified), and otherwise say
// honestly that the row is no longer running, naming the state it found.
// Deterministic by construction: each of the first three legs seeds a
// StatusRunning record, has writer-a claim it, has writer-b's recovery reset
// it back to pending -- the exact store-level steal -- and then executes one
// genuine Handle whose outcome write must no-op. Leg 4 adds the stealing
// writer's RE-CLAIM of the reset row: the row is running again -- under a
// claim this queue does not own -- the state in which an outcome write
// whose WHERE named only id and status would not even no-op: it would
// settle the sibling's running row with this attempt's result. The guarded
// write (WHERE ... AND claimed_by = owner) no-ops, and the probe logs its
// running-under-another-writer Error line; each leg logging "job cancelled
// before its ..." for the stolen row fails this test.
func TestExecute_NoOpOutcomeWrite_RowStolenByAnotherWriter_LogsHonestlyNotCancelled(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	ctx := context.Background()

	var succeededHandles, retriedHandles, deadLetteredHandles int
	succeed := NewHandlerFunc("discard.stolen.succeed", func(context.Context, *Job, ProgressFn) (Result, error) {
		succeededHandles++
		return Result{Data: []byte("done")}, nil
	})
	fail := NewHandlerFunc("discard.stolen.fail", func(context.Context, *Job, ProgressFn) (Result, error) {
		retriedHandles++
		return Result{}, errors.New("transient failure")
	})
	deadLetter := NewHandlerFunc("discard.stolen.dead_letter", func(context.Context, *Job, ProgressFn) (Result, error) {
		deadLetteredHandles++
		return Result{}, errors.New("permanent failure")
	})
	for _, h := range []Handler{succeed, fail, deadLetter} {
		if err := q.RegisterHandler(h); err != nil {
			t.Fatalf("RegisterHandler() error = %v", err)
		}
	}

	// steal moves rec from StatusRunning into another writer's reset: writer-a
	// claims the row, then writer-b's Start-time recovery flips every row not
	// claimed by writer-b back to pending -- the exact step that precedes a
	// double execution after a writer-gate lapse. execute's subsequent Handle
	// runs on a row the database no longer holds as running.
	steal := func(rec *jobRecord) {
		claimed, err := claimOne(ctx, q.db, *rec, time.Now(), "writer-a")
		if err != nil {
			t.Fatalf("claimOne() error = %v", err)
		}
		if !claimed {
			t.Fatalf("claimOne() = false, want true (the seeded running record must be claimable)")
		}
		if err := resetInterruptedRecords(ctx, q.db, time.Now(), "writer-b"); err != nil {
			t.Fatalf("resetInterruptedRecords() error = %v", err)
		}
	}

	// runOne executes one leg: steal rec, execute a genuine Handle for it
	// while capturing every log line the leg emits, and hand the buffer back.
	runOne := func(rec *jobRecord) *bytes.Buffer {
		steal(rec)
		prevDefault := slog.Default()
		var buf bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer func() { slog.SetDefault(prevDefault) }()
		q.execute(*rec)
		return &buf
	}

	// Leg 1 -- the success branch: a genuinely executed, successful Handle
	// whose success write finds the row already reset by another writer.
	recSuccess := fixtureRunningRecord("tenant-a", "discard.stolen.succeed")
	if err := q.db.Create(recSuccess).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	buf := runOne(recSuccess)
	if succeededHandles != 1 {
		t.Fatalf("Handle ran %d times, want exactly 1 (the leg must prove the job genuinely executed before judging its log lines)", succeededHandles)
	}
	if out := buf.String(); strings.Contains(out, "job cancelled before its outcome could be recorded") {
		t.Errorf("stolen row logged as cancelled (success leg): %s -- a row another writer reset is NOT a cancelled job; the cancel explanation erased the double-run evidence", out)
	} else if !strings.Contains(out, "row not running when the write landed") {
		t.Errorf("missing the honest discarded-outcome line for the stolen row: %s", out)
	}

	// Leg 2 -- the retry branch: a genuinely executed, failed Handle (retries
	// remaining) whose retry write finds the row already reset.
	recRetry := fixtureRunningRecord("tenant-a", "discard.stolen.fail")
	recRetry.Attempts = 1 // matches the post-handoff state: runAttempt counted this first attempt
	recRetry.MaxRetries = 5
	if err := q.db.Create(recRetry).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	buf = runOne(recRetry)
	if retriedHandles != 1 {
		t.Fatalf("Handle ran %d times, want exactly 1 (the leg must prove the job genuinely executed before judging its log lines)", retriedHandles)
	}
	if out := buf.String(); strings.Contains(out, "job cancelled before its outcome could be recorded") {
		t.Errorf("stolen row logged as cancelled (retry leg): %s -- a row another writer reset is NOT a cancelled job; the cancel explanation erased the double-run evidence", out)
	} else if !strings.Contains(out, "row not running when the write landed") {
		t.Errorf("missing the honest discarded-outcome line for the stolen row: %s", out)
	}

	// Leg 3 -- the dead-letter branch: the same steal against the final,
	// retries-exhausted attempt whose dead-letter write finds the row reset.
	recDead := fixtureRunningRecord("tenant-a", "discard.stolen.dead_letter")
	recDead.Attempts = 1   // matches the post-handoff state
	recDead.MaxRetries = 0 // exhausted on the very first attempt
	if err := q.db.Create(recDead).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	buf = runOne(recDead)
	if deadLetteredHandles != 1 {
		t.Fatalf("Handle ran %d times, want exactly 1 (the leg must prove the job genuinely executed before judging its log lines)", deadLetteredHandles)
	}
	if out := buf.String(); strings.Contains(out, "job cancelled before its outcome could be recorded") {
		t.Errorf("stolen row logged as cancelled (dead-letter leg): %s -- a row another writer reset is NOT a cancelled job; the cancel explanation erased the double-run evidence", out)
	} else if !strings.Contains(out, "row not running when the write landed") {
		t.Errorf("missing the honest discarded-outcome line for the stolen row: %s", out)
	}

	// Leg 4 -- the running-under-another-writer shape: the steal followed by
	// the stealing writer's re-claim, so the row is StatusRunning under a
	// claim this queue does not own while this attempt's Handle finishes. The
	// outcome write must no-op (its WHERE names this writer's own claim), and
	// the probe must log its running-under-another-writer Error line -- never
	// the cancellation explanation, never the generic not-running warn. A
	// completion write whose WHERE named only id and status would let this
	// attempt's success LAND on the sibling's running row, which fails this
	// test.
	recReclaimed := fixtureRunningRecord("tenant-a", "discard.stolen.succeed")
	if err := q.db.Create(recReclaimed).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	steal(recReclaimed)
	var resetReclaimed jobRecord
	if err := q.db.First(&resetReclaimed, "id = ?", recReclaimed.ID).Error; err != nil {
		t.Fatalf("re-read reset record: %v", err)
	}
	if claimed, err := claimOne(ctx, q.db, resetReclaimed, time.Now(), "writer-c"); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	} else if !claimed {
		t.Fatalf("claimOne() = false, want true (the reset row must be claimable by its new writer)")
	}
	prevDefault := slog.Default()
	var reclaimedBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&reclaimedBuf, nil)))
	q.execute(*recReclaimed)
	slog.SetDefault(prevDefault)
	if succeededHandles != 2 {
		t.Fatalf("Handle ran %d times, want exactly 2 (the leg must prove the job genuinely executed before judging its log lines)", succeededHandles)
	}
	if out := reclaimedBuf.String(); strings.Contains(out, "job cancelled before its outcome could be recorded") {
		t.Errorf("stolen row logged as cancelled (reclaimed leg): %s -- a row another writer is executing is NOT a cancelled job", out)
	} else if !strings.Contains(out, "running under another writer") {
		t.Errorf("missing the running-under-another-writer Error line for the reclaimed row: %s", out)
	}
	gotReclaimed, err := findByID(context.Background(), q.db, JobID(recReclaimed.ID))
	if err != nil {
		t.Fatalf("findByID(%q) error = %v", recReclaimed.ID, err)
	}
	if gotReclaimed.Status != string(StatusRunning) {
		t.Errorf("Status = %q, want %q (the discarded outcome must leave the reclaimed row running under its new writer, not settled and not cancelled)", gotReclaimed.Status, StatusRunning)
	}
	if gotReclaimed.ClaimedBy != "writer-c" {
		t.Errorf("ClaimedBy = %q, want %q (the discarded outcome must not touch the row's ownership)", gotReclaimed.ClaimedBy, "writer-c")
	}

	// The first three legs' rows must still sit where the steal left them:
	// the no-op outcome write neither recorded the outcome nor cancelled the
	// job -- the row belongs to the stealing writer's recovery, exactly as it
	// did before execute ran.
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	for _, rec := range []*jobRecord{recSuccess, recRetry, recDead} {
		got, err := q.Get(tenantCtx, JobID(rec.ID))
		if err != nil {
			t.Fatalf("Get(%q) error = %v", rec.ID, err)
		}
		if got.Status != StatusPending {
			t.Errorf("Status = %v, want %v (the discarded outcome must leave the stolen row pending, not cancelled and not settled)", got.Status, StatusPending)
		}
	}
}

// injectResultWriteFailures registers a GORM update callback on db that
// fails the next *n UPDATE statements whose update map carries the
// "result" column -- the signature of completeSucceeded, the one outcome
// write that persists a success result (completeRetrying, completeDeadLetter,
// markAttemptStarted and updateProgress never touch the result column, so
// they pass through untouched) -- with failErr, then stops failing. It is
// the deterministic fault injection the outcome-write-failure regressions
// below need: a failure that hits exactly the write under test and leaves
// every other write -- in particular the convergence writes the fix
// performs -- working.
func injectResultWriteFailures(db *gorm.DB, remaining *int, failErr error) {
	db.Callback().Update().Before("gorm:update").Register("test:fail-result-writes", func(tx *gorm.DB) {
		if remaining == nil || *remaining <= 0 {
			return
		}
		m, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, has := m["result"]; !has {
			return
		}
		*remaining--
		tx.AddError(failErr)
	})
}

// TestExecute_SuccessWriteFailure_SchedulesRetry_InsteadOfLeavingRowRunning
// pins the convergence of the success-result persistence hole: when
// completeSucceeded itself fails, execute must not only log "jobs:
// persisting success failed" and return -- with no time-based lease and no
// reaper in StandaloneQueue, nothing in a live process would ever move a
// row left StatusRunning again (it would converge only at the next Start's
// resetInterruptedRecords). Instead the attempt converges through the same
// terminal machinery a handler failure uses: the row is exactly as the
// failure path finds it (running, outcome unpersisted), so the attempt is
// settled as failed with an internal cause naming the persistence failure
// -- a retry scheduled while attempts remain, a dead-letter once the
// budget is exhausted. Deterministic by construction: the seeded running
// record's success write fails exactly once (injectResultWriteFailures),
// and execute must schedule the retry -- a row still StatusRunning after
// execute returns fails this test.
func TestExecute_SuccessWriteFailure_SchedulesRetry_InsteadOfLeavingRowRunning(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(NewHandlerFunc("succeeds.once", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{Data: []byte("ok")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "succeeds.once")
	rec.Attempts = 1   // matches the post-handoff state: runAttempt counted this first attempt
	rec.MaxRetries = 3 // retries remain after this attempt
	// Seeded under this queue's own claim: the outcome and convergence
	// writes carry claimed_by = owner, exactly as they do for a row this
	// queue's dispatcher really claimed.
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}

	failures := 1
	injectResultWriteFailures(q.db, &failures, errors.New("injected result-write failure"))
	q.execute(*rec)

	var got jobRecord
	if err := q.db.First(&got, "id = ?", rec.ID).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusRetrying) {
		t.Errorf("Status = %v, want %v -- a failed success write must converge to a scheduled retry, never stay running", got.Status, StatusRetrying)
	}
	if !strings.Contains(got.Error, "jobs: persisting success result failed") {
		t.Errorf("error_message = %q, want it to name the persistence failure as the retry's cause", got.Error)
	}
	if !got.ScheduledAt.After(rec.ScheduledAt) {
		t.Errorf("ScheduledAt = %v, want it moved past %v by the retry backoff", got.ScheduledAt, rec.ScheduledAt)
	}
	if failures != 0 {
		t.Errorf("injected failures left = %d, want 0 (the injection must have fired exactly once)", failures)
	}
}

// succeedWithHookHandler succeeds from every Handle and records each
// OnFailure call on onFailureCh -- the terminal-attempt counterpart of
// cancelledBeforeDeadLetterHandler, for a handler whose Handle succeeds but
// whose success cannot be persisted.
type succeedWithHookHandler struct {
	jobType     string
	onFailureCh chan struct{}
}

func (h *succeedWithHookHandler) Type() string { return h.jobType }
func (h *succeedWithHookHandler) Handle(context.Context, *Job, ProgressFn) (Result, error) {
	return Result{Data: []byte("ok")}, nil
}

func (h *succeedWithHookHandler) OnFailure(context.Context, *Job, error) {
	h.onFailureCh <- struct{}{}
}

var (
	_ Handler     = (*succeedWithHookHandler)(nil)
	_ FailureHook = (*succeedWithHookHandler)(nil)
)

// TestExecute_SuccessWriteFailure_OnFinalAttempt_DeadLettersWithCauseAndHook
// is the terminal-attempt half of the same success-result persistence-hole
// pin: a Job whose retry budget is exhausted (attempts > MaxRetries) and
// whose final attempt's success write fails must converge to
// StatusDeadLetter -- operator-visible through DeadLetterJobs with the
// persistence failure as its recorded cause, and running the standard
// FailureHook once, exactly like any other genuine running -> dead-letter
// transition -- never a row stuck StatusRunning with no outcome. A path
// that logged the persistence failure and returned, leaving the row
// running and the hook silent, fails this test.
func TestExecute_SuccessWriteFailure_OnFinalAttempt_DeadLettersWithCauseAndHook(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	h := &succeedWithHookHandler{jobType: "succeeds.on_final_attempt", onFailureCh: make(chan struct{}, 1)}
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "succeeds.on_final_attempt")
	rec.Attempts = 2   // matches the post-handoff state
	rec.MaxRetries = 1 // exhausted: attempts(2) > MaxRetries(1)
	// Seeded under this queue's own claim, like the sibling test above.
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}

	failures := 1
	injectResultWriteFailures(q.db, &failures, errors.New("injected result-write failure"))
	q.execute(*rec)

	var got jobRecord
	if err := q.db.First(&got, "id = ?", rec.ID).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusDeadLetter) {
		t.Errorf("Status = %v, want %v -- a final attempt whose success could not be persisted must dead-letter, never stay running", got.Status, StatusDeadLetter)
	}
	if !strings.Contains(got.Error, "jobs: persisting success result failed") {
		t.Errorf("error_message = %q, want it to name the persistence failure as the dead-letter's cause", got.Error)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt = nil, want it stamped by the dead-letter transition")
	}
	select {
	case <-h.onFailureCh:
	default:
		t.Error("OnFailure not called -- a genuine running -> dead-letter transition must run the FailureHook once, exactly like any other dead-letter")
	}
}
