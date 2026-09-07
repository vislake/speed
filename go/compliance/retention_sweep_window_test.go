package compliance

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/compliance/internal/testutil"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the window semantics of the retention-sweep idempotency
// key (retentionSweepIdempotencyKey): enqueues inside one
// retentionSweepWindowSize window collapse into one job (the concurrency
// protection the key exists for, preserved), enqueues in a later window
// become new jobs and sweep again (periodicity), and a sweep job that
// dead-letters poisons only its own window, never its tenant's later
// windows. All three run against a REAL jobs.StandaloneQueue over a real
// SQLite database -- the dedupe behaviour under test lives in jobs'
// partial unique index and row semantics, which a fake queue cannot
// exercise. Tests (b) and (c) fail on the pre-window key (tenant-only):
// the later enqueue resolves the first job's id and no second sweep ever
// runs.
//
// EnqueueRetentionSweep returns no job id (it is a fire-and-forget
// schedule point), so the tests observe the queue's own database -- the
// same *gorm.DB the queue was started over -- for row counts and ids,
// plus the registered handler's run channel for executions.

// startRetentionSweepWindowQueue starts a real StandaloneQueue over its
// own fresh database with fast intervals, registering cleanup, and
// returns both the queue and its database.
func startRetentionSweepWindowQueue(t *testing.T) (*jobs.StandaloneQueue, *gorm.DB) {
	t.Helper()
	db := testutil.NewDB(t)
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

// retentionWindowA and retentionWindowB are two points in time in two
// different retentionSweepWindowSize windows (10:15 and 11:15 UTC),
// windowB exactly one window later than windowA.
var (
	retentionWindowA = time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	retentionWindowB = retentionWindowA.Add(retentionSweepWindowSize)
)

// retentionSweepRowCount counts the retention-sweep rows in the queue's
// own database: one per idempotency-key window resolved, whether the row
// is pending, running or already settled.
func retentionSweepRowCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Table("jobs").Where("type = ?", taskTypeRetentionSweep).Count(&n).Error; err != nil {
		t.Fatalf("count retention-sweep rows: %v", err)
	}
	return n
}

// firstRetentionSweepJobID returns the id of the database's only
// retention-sweep row.
func firstRetentionSweepJobID(t *testing.T, db *gorm.DB) jobs.JobID {
	t.Helper()
	if n := retentionSweepRowCount(t, db); n != 1 {
		t.Fatalf("retention-sweep rows = %d, want exactly 1 before reading the first job id", n)
	}
	var ids []string
	if err := db.Table("jobs").Where("type = ?", taskTypeRetentionSweep).Pluck("id", &ids).Error; err != nil {
		t.Fatalf("read first retention-sweep job id: %v", err)
	}
	return jobs.JobID(ids[0])
}

// waitForRetentionRun waits until a sweep handler run lands on runs and
// returns its job id, failing the test after timeout. runs must be a
// buffered channel the handler fills once per Handle call.
func waitForRetentionRun(t *testing.T, runs chan jobs.JobID, what string) jobs.JobID {
	t.Helper()
	select {
	case id := <-runs:
		return id
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no sweep run within 20s", what)
		return ""
	}
}

// TestEnqueueRetentionSweep_SameWindowEnqueuesCollapseIntoOneJob pins
// regression (a): the concurrency protection the sweep key exists for must
// survive the windowing -- two enqueues for one tenant inside the same
// retentionSweepWindowSize window collapse into the first job (one row,
// one run), so two scheduler replicas ticking in one window still never
// sweep the tenant twice at once.
func TestEnqueueRetentionSweep_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	svc := newRetentionService()
	q, db := startRetentionSweepWindowQueue(t)
	svc.queue = q
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeRetentionSweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return retentionWindowA }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("first EnqueueRetentionSweep: %v", err)
	}
	// A second replica's tick ten minutes later -- still inside
	// retentionWindowA's retentionSweepWindowSize window.
	svc.now = func() time.Time { return retentionWindowA.Add(10 * time.Minute) }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("second EnqueueRetentionSweep: %v", err)
	}

	if n := retentionSweepRowCount(t, db); n != 1 {
		t.Fatalf("retention-sweep rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row", n)
	}
	first := waitForRetentionRun(t, runs, "the collapsed sweep")
	if first != firstRetentionSweepJobID(t, db) {
		t.Errorf("sweep run job id = %s, want the row's id %s", first, firstRetentionSweepJobID(t, db))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("sweep ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnqueueRetentionSweep_LaterWindowEnqueuesNewJobAndSweepsAgain pins
// regression (b): an enqueue in a later window is a NEW job and the sweep
// runs again. Fails on the pre-window key (tenant only), where the later
// enqueue resolves the first job's id -- the first-ever sweep's permanent
// dedupe -- so no second row is ever created and nothing ever runs again.
func TestEnqueueRetentionSweep_LaterWindowEnqueuesNewJobAndSweepsAgain(t *testing.T) {
	svc := newRetentionService()
	q, db := startRetentionSweepWindowQueue(t)
	svc.queue = q
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeRetentionSweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return retentionWindowA }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("first EnqueueRetentionSweep: %v", err)
	}
	first := waitForRetentionRun(t, runs, "window A's sweep")

	// The scheduler's tick an hour later: retentionWindowB, a different
	// window.
	svc.now = func() time.Time { return retentionWindowB }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("second EnqueueRetentionSweep: %v", err)
	}

	if n := retentionSweepRowCount(t, db); n != 2 {
		t.Fatalf("retention-sweep rows = %d, want 2 -- the later window's enqueue must create a NEW job (fails on the tenant-only key, which resolves the first row forever)", n)
	}
	second := waitForRetentionRun(t, runs, "window B's sweep")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own sweep", second)
	}
}

// TestEnqueueRetentionSweep_DeadLetteredWindowDoesNotPoisonLaterOnes pins
// regression (c): a sweep job that dead-letters poisons only its own
// window. Retention overrun is a legal obligation, so a dead-lettered
// sweep must never silence its tenant's later windows. Fails on the
// pre-window key (tenant only), where the dead job's idempotency key stays
// resolved forever -- every later enqueue returns the dead job's id, no
// second row is ever created and the tenant is never swept again.
func TestEnqueueRetentionSweep_DeadLetteredWindowDoesNotPoisonLaterOnes(t *testing.T) {
	svc := newRetentionService()
	q, db := startRetentionSweepWindowQueue(t)
	svc.queue = q

	// succeed is flipped only after window A's job has dead-lettered; until
	// then every Handle fails permanently, which is what dead-letters it
	// (with DefaultMaxRetries 3, the fourth attempt exhausts the budget).
	var succeed bool
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeRetentionSweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		if !succeed {
			return jobs.Result{}, errors.New("compliance: injected sweep failure")
		}
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	getCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	svc.now = func() time.Time { return retentionWindowA }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("first EnqueueRetentionSweep: %v", err)
	}
	first := firstRetentionSweepJobID(t, db)

	// Window A's sweep exhausts its retries and dead-letters. Wait for the
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
			t.Fatalf("window A's sweep never dead-lettered within 20s (status %v)", job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next window's enqueue must run its own sweep, whatever happened
	// to window A's.
	succeed = true
	svc.now = func() time.Time { return retentionWindowB }
	if err := svc.EnqueueRetentionSweep(ctx); err != nil {
		t.Fatalf("second EnqueueRetentionSweep: %v", err)
	}

	if n := retentionSweepRowCount(t, db); n != 2 {
		t.Fatalf("retention-sweep rows = %d, want 2 -- the dead-lettered window's key must not keep resolving for later windows (fails on the tenant-only key)", n)
	}
	second := waitForRetentionRun(t, runs, "window B's sweep after window A dead-lettered")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's dead-lettered job -- a dead-lettered window must not poison the tenant's later windows", second)
	}
}
