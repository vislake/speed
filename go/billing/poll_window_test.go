package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the window semantics of the poll idempotency key
// (pollIdempotencyKey): enqueues inside one poll window collapse
// into one job (the concurrency protection the key exists for,
// preserved), enqueues in a later window become new jobs and the poll
// runs again (periodicity -- the property the pre-window key destroyed,
// since jobs' dedup is permanent on StandaloneQueue and a tenant-only key
// gave each tenant exactly one poll task per database file), and a poll
// job that dead-letters poisons only its own window, never its tenant's
// later windows. All three run against a REAL jobs.StandaloneQueue over a
// real SQLite database -- the dedupe behaviour under test lives in jobs'
// partial unique index and row semantics, which a fake queue cannot
// exercise. Tests (b) and (c) fail on the pre-window key (tenant only):
// the later enqueue resolves the first job's id and no second poll ever
// runs.
//
// The window boundary constants below are the implementation's own
// (pollIdempotencyWindowSize, 15 minutes -- DefaultPollStuckAfter, the
// poll's own detection granularity) spelled as local literals so this
// file compiles and runs against the pre-window code unchanged; a drift
// between the two would break test (b) loudly, since an enqueue
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

// TestEnqueuePoll_SameWindowEnqueuesCollapseIntoOneJob pins regression
// (a): the concurrency protection the poll key exists for must survive
// the windowing -- two enqueues for one tenant inside the same poll
// window collapse into the first job (one row, one run), so two scheduler
// replicas ticking in one window still never poll the tenant twice at
// once.
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

// TestEnqueuePoll_LaterWindowEnqueuesNewJobAndRunsAgain pins regression
// (b): an enqueue in a later window is a NEW job and the poll runs again.
// Fails on the pre-window key (tenant only), where the later enqueue
// resolves the first job's id -- the first-ever poll's permanent dedupe --
// so no second row is ever created and nothing ever runs again: the harm
// the windowing closes, a stuck payment that is never actively polled
// once the payment chain is connected (the single run of the tenant's
// one-ever poll usually finds nothing, since no row is stuck yet at
// DefaultPollStuckAfter past its start).
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

// TestEnqueuePoll_DeadLetteredWindowDoesNotPoisonLaterOnes pins regression
// (c): a poll job that dead-letters poisons only its own window. A stuck
// payment left unpolled is money-path harm, and a permanently-failing
// poll pass (a database outage lasting out the retry budget) must never
// silence its tenant's later windows. Fails on the pre-window key (tenant
// only), where the dead job's idempotency key stays resolved forever --
// every later enqueue returns the dead job's id, no second row is ever
// created and the tenant is never polled again.
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
