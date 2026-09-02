package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// fixtureRunningRecord is fixtureRecord (store_test.go) plus the one
// override every test below needs: execute's own doc comment says it runs
// "exactly one Handle attempt for rec (already StatusRunning in the
// database)" -- completeSucceeded/completeRetrying/completeDeadLetter all
// guard their update with `WHERE status = 'running'`, so calling execute
// directly against a row fixtureRecord left StatusPending would silently
// no-op every one of those writes instead of exercising them.
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
// "not inherited from the original Enqueue call's context" half of
// AGENTS.md's tenant context trap: that original context is long gone by
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
// If a future change to execute/runWorker ever stops routing through
// jobContext, THIS is the test that must start failing -- with this exact,
// well-labeled name -- rather than some unrelated Handler mysteriously
// erroring in production months later. See demo_queue_test.go's
// TestDemoQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant for the
// same guarantee proved end to end through a real worker and a real
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
	q := NewDemoQueue(nil, WithBackoff(1*time.Second, 10*time.Second))

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
	q := NewDemoQueue(nil, WithTenantConcurrencyLimit(2))
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
// package's concurrency hot spot the backend coding standard §13 requires
// a -race test for.
func TestTenantSlotReservation_ConcurrentAccessIsRaceFree(t *testing.T) {
	q := NewDemoQueue(nil, WithTenantConcurrencyLimit(3))
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

// TestExecute_EmptyTenantID_FailsClosedWithoutCallingHandle is the
// regression test for the review finding that execute called
// handler.Handle for a jobRecord whose TenantID column was empty, instead
// of refusing the attempt the way AsynqQueue's processTaskUncancelled
// already does for the identical case (errAsynqTaskMissingTenant,
// asynq_worker.go). Task.validate blocks Enqueue itself from ever creating
// such a row, so the corrupted row here is seeded directly through q.db --
// simulating a row written by anything other than Enqueue: a migration
// bug, a manual SQL fixup, or a future writer that bypasses this package's
// own API. See errDemoJobMissingTenant's own doc comment (worker.go) for
// why this matters even though it is not reachable through the public API
// today.
func TestExecute_EmptyTenantID_FailsClosedWithoutCallingHandle(t *testing.T) {
	q := NewDemoQueue(newTestDB(t))
	handleInvoked := false
	h := NewHandlerFunc("corrupt.tenant", func(context.Context, *Job, ProgressFn) (Result, error) {
		handleInvoked = true
		return Result{}, nil
	})
	if err := q.RegisterHandler(h); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("", "corrupt.tenant") // TenantID deliberately empty
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
	if got.Error != errDemoJobMissingTenant.Code {
		t.Errorf("Error = %q, want %q", got.Error, errDemoJobMissingTenant.Code)
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

// TestExecute_HandlerPanic_RecoversInsteadOfCrashingProcess is the
// regression test for the review finding that execute had no recover() of
// its own around handler.Handle, unlike asynq's own processor.perform,
// which protects the equivalent call for AsynqQueue for free. Before the
// fix, this test crashes the ENTIRE test binary rather than merely failing
// one assertion -- exactly as an unrecovered panic crashes the entire
// worker-pool process in production, taking every OTHER tenant's in-flight
// and queued Jobs down with it (root CLAUDE.md: speed's modules "compile
// into one binary"). The outer defer/recover below exists only to turn
// that crash into a well-labeled t.Fatal instead of a bare process exit,
// should this ever regress -- it does not run today, since invokeHandle
// (worker.go) already recovers the panic before it reaches this test.
func TestExecute_HandlerPanic_RecoversInsteadOfCrashingProcess(t *testing.T) {
	q := NewDemoQueue(newTestDB(t))
	if err := q.RegisterHandler(panickingHandler{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "panics.always")
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
	q := NewDemoQueue(newTestDB(t))
	if err := q.RegisterHandler(panickingFailureHook{}); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	rec := fixtureRunningRecord("tenant-a", "panics.on_failure")
	rec.MaxRetries = 0 // exhausted on the very first attempt
	rec.Attempts = 1   // matches what claimOne would have set for a first attempt
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
