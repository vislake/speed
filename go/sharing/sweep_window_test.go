package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the window semantics of the expiry-sweep idempotency key
// (expirySweepIdempotencyKey): enqueues inside one expirySweepWindowSize
// window collapse into one job (the concurrency protection the key exists
// for), enqueues in a later window become new jobs and sweep again
// (periodicity), a sweep job that dead-letters poisons only its own
// window, never its tenant's later windows, and a later window's sweep
// really re-runs Service.Sweep, so an overage view reservation (past
// viewReservationTimeout on a share nobody accesses again) is really
// refunded by the sweep's own reservation arm. All four run against a REAL
// jobs.StandaloneQueue over a real SQLite database -- the dedupe behaviour
// under test lives in jobs' partial unique index and row semantics, which a
// fake queue cannot exercise. A tenant-only key would make the later
// enqueues of the window tests below return the first job's id and no
// second sweep would ever run; the windowed key is what lets them.
//
// EnqueueExpirySweep returns no job id (it is a fire-and-forget schedule
// point), so the tests observe the queue's own database -- the same
// *gorm.DB the queue was started over -- for row counts and ids, plus the
// registered handler's run channel for executions. The window an enqueue
// falls in is read from the module service's clock (Module.EnqueueExpirySweep
// derives the key from m.svc.now, the same seam Service.Sweep reads when it
// runs), pinned per phase so both the key resolution and the run-time
// staleness checks are deterministic; each phase's clock is only ever
// changed after the previous phase's run has been observed to complete.

// startSweepWindowQueue starts a real StandaloneQueue over its own fresh
// database with fast intervals, and returns both the queue and its database.
func startSweepWindowQueue(t *testing.T) (*jobs.StandaloneQueue, *gorm.DB) {
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

// windowA and windowB are two points in time in two different
// expirySweepWindowSize windows (10:15 and 11:15 UTC), windowB exactly one
// window later than windowA.
var (
	windowA = time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	windowB = windowA.Add(expirySweepWindowSize)
)

// sweepRowCount counts the expiry-sweep rows in the queue's own database:
// one per idempotency-key window resolved, whether the row is pending,
// running or already settled.
func sweepRowCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Table("jobs").Where("type = ?", taskTypeExpirySweep).Count(&n).Error; err != nil {
		t.Fatalf("count expiry-sweep rows: %v", err)
	}
	return n
}

// firstSweepJobID returns the id of the database's only expiry-sweep row.
func firstSweepJobID(t *testing.T, db *gorm.DB) jobs.JobID {
	t.Helper()
	if n := sweepRowCount(t, db); n != 1 {
		t.Fatalf("expiry-sweep rows = %d, want exactly 1 before reading the first job id", n)
	}
	var ids []string
	if err := db.Table("jobs").Where("type = ?", taskTypeExpirySweep).Pluck("id", &ids).Error; err != nil {
		t.Fatalf("read first expiry-sweep job id: %v", err)
	}
	return jobs.JobID(ids[0])
}

// waitForRun waits until a sweep handler run lands on runs and returns its
// job id, failing the test after timeout. runs must be a buffered channel
// the handler fills once per Handle call.
func waitForRun(t *testing.T, runs chan jobs.JobID, what string) jobs.JobID {
	t.Helper()
	select {
	case id := <-runs:
		return id
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no sweep run within 20s", what)
		return ""
	}
}

// TestModule_EnqueueExpirySweep_SameWindowEnqueuesCollapseIntoOneJob pins
// regression (b): the concurrency protection the sweep key exists for must
// survive the windowing -- two enqueues for one tenant inside the same
// expirySweepWindowSize window collapse into the first job (the real
// queue's idempotent Enqueue resolves the key to the existing row: one
// row, one run), so two scheduler replicas ticking in one window still
// never sweep the tenant twice at once.
func TestModule_EnqueueExpirySweep_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	q, db := startSweepWindowQueue(t)
	m := NewModule(newTestDB(t), WithQueue(q))
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := testCtx()
	m.svc.now = func() time.Time { return windowA }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	// A second replica's tick ten minutes later -- still inside windowA's
	// expirySweepWindowSize window.
	m.svc.now = func() time.Time { return windowA.Add(10 * time.Minute) }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 1 {
		t.Fatalf("expiry-sweep rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row", n)
	}
	first := waitForRun(t, runs, "the collapsed sweep")
	if first != firstSweepJobID(t, db) {
		t.Errorf("sweep run job id = %s, want the row's id %s", first, firstSweepJobID(t, db))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("sweep ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestModule_EnqueueExpirySweep_LaterWindowEnqueuesNewJobAndSweepsAgain pins
// periodicity: an enqueue in a later window is a NEW job and the sweep
// runs again. Under a tenant-only key the later enqueue would resolve the
// first job's id -- a permanent dedupe -- so no second row would ever be
// created and nothing would ever run again.
func TestModule_EnqueueExpirySweep_LaterWindowEnqueuesNewJobAndSweepsAgain(t *testing.T) {
	q, db := startSweepWindowQueue(t)
	m := NewModule(newTestDB(t), WithQueue(q))
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := testCtx()
	m.svc.now = func() time.Time { return windowA }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	first := waitForRun(t, runs, "window A's sweep")

	// The scheduler's tick an hour later: windowB, a different window.
	m.svc.now = func() time.Time { return windowB }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 2 {
		t.Fatalf("expiry-sweep rows = %d, want 2 -- the later window's enqueue must create a NEW job (fails on the tenant-only key, which resolves the first row forever)", n)
	}
	second := waitForRun(t, runs, "window B's sweep")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own sweep", second)
	}
}

// TestModule_EnqueueExpirySweep_DeadLetteredWindowDoesNotPoisonLaterOnes pins
// the dead-letter isolation: a sweep job that dead-letters poisons only its
// own window. Under a tenant-only key the dead job's idempotency key would
// stay resolved forever -- every later enqueue would return the dead job's
// id, no second row would ever be created and the tenant would never be
// swept again.
func TestModule_EnqueueExpirySweep_DeadLetteredWindowDoesNotPoisonLaterOnes(t *testing.T) {
	q, db := startSweepWindowQueue(t)
	m := NewModule(newTestDB(t), WithQueue(q))

	// succeed is flipped only after window A's job has dead-lettered; until
	// then every Handle fails permanently, which is what dead-letters it
	// (with DefaultMaxRetries 3, the fourth attempt exhausts the budget).
	var succeed bool
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		if !succeed {
			return jobs.Result{}, errors.New("sharing: injected sweep failure")
		}
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := testCtx()
	getCtx := pkgcore.WithTenant(context.Background(), testTenant)
	m.svc.now = func() time.Time { return windowA }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	first := firstSweepJobID(t, db)

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
	m.svc.now = func() time.Time { return windowB }
	if err := m.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 2 {
		t.Fatalf("expiry-sweep rows = %d, want 2 -- the dead-lettered window's key must not keep resolving for later windows (fails on the tenant-only key)", n)
	}
	second := waitForRun(t, runs, "window B's sweep after window A dead-lettered")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's dead-lettered job -- a dead-lettered window must not poison the tenant's later windows", second)
	}
}

// TestModule_EnqueueExpirySweep_LaterWindowSweepRefundsAnOverageReservation
// is the single-module verification of the periodicity half: pins that a
// view reservation past viewReservationTimeout on a share nobody accesses
// again is refunded by sharing's own sweep refund arm -- but only if the
// scheduled sweep REALLY re-runs. The test constructs the overage
// reservation (taken at windowA-10min), lets window A's scheduled sweep run
// while the reservation is still young (it must leave it standing --
// nothing auto-converges a reservation younger than the timeout), then
// schedules window B's sweep the way the host's periodic scheduler would;
// only the windowed key lets that enqueue become a new job whose run, past
// the timeout, refunds the reservation. Under a tenant-only key, window B's
// enqueue resolves window A's completed job, no
// second sweep ever runs, and the reservation stands forever -- the legal
// recipient's one view permanently lost.
func TestModule_EnqueueExpirySweep_LaterWindowSweepRefundsAnOverageReservation(t *testing.T) {
	q, db := startSweepWindowQueue(t)
	m := NewModule(newTestDB(t), WithQueue(q))
	svc := m.svc
	// Wire svc to a real in-memory registry the way Module.Register would
	// (newTestService's own body), so Create's rate-limit read and the
	// sweep's event publish have the host seams they read at call time.
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionSensitiveShareCreate); err != nil {
		t.Fatalf("AuditActions.Add: %v", err)
	}
	svc.attach(reg)

	// The real expiry-sweep handler, wrapped only to report each completed
	// run -- the queue worker supplies the tenant context (go/jobs worker.go
	// rebuilds pkgcore.WithTenant from the job's own stored tenant).
	real := expirySweepHandler{svc: svc}
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
		result, err := real.Handle(ctx, job, progress)
		runs <- job.ID
		return result, err
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := testCtx()
	createAt := windowA.Add(-11 * time.Minute)   // 10:04: the share is minted
	reservedAt := windowA.Add(-10 * time.Minute) // 10:05: the serve reserves its one view
	one := 1
	svc.now = func() time.Time { return createAt }
	created, err := svc.Create(ctx, CreateParams{ResourceRef: "r", MaxViews: &one})
	if err != nil {
		t.Fatalf("Create(MaxViews=1): %v", err)
	}
	share, err := svc.Get(ctx, created.Share.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if won, reserveErr := svc.shares.tryReserveView(ctx, share, reservedAt); reserveErr != nil || !won {
		t.Fatalf("tryReserveView: won=%v err=%v", won, reserveErr)
	}
	// Nobody ever accesses the share again: only the periodic sweep can
	// ever refund this reservation.

	// Window A's scheduled sweep runs ten minutes after the reservation was
	// taken -- still inside viewReservationTimeout, so the pass must leave
	// it standing.
	svc.now = func() time.Time { return windowA }
	err = m.EnqueueExpirySweep(ctx)
	if err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	first := waitForRun(t, runs, "window A's sweep")

	// An hour later the host's next tick lands in window B, long past the
	// reservation's timeout. This enqueue must become a NEW job.
	svc.now = func() time.Time { return windowB }
	err = m.EnqueueExpirySweep(ctx)
	if err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}
	if n := sweepRowCount(t, db); n != 2 {
		t.Fatalf("expiry-sweep rows = %d, want 2 -- the later window's enqueue must create a NEW job (fails on the tenant-only key: the overage reservation is never swept again)", n)
	}
	second := waitForRun(t, runs, "window B's sweep")
	if second == first {
		t.Fatalf("window B's run job id = %s, the same as window A's -- the sweep never re-ran, so the overage reservation cannot have been refunded", second)
	}

	got, err := svc.Get(ctx, created.Share.ID)
	if err != nil {
		t.Fatalf("Get(after window B's sweep): %v", err)
	}
	if got.ViewsReserved != 0 || got.ViewsReservedAt != nil {
		t.Errorf("overage reservation after window B's sweep = ViewsReserved %d / ViewsReservedAt %v, want 0 / nil -- the re-run sweep's refund arm must refund it", got.ViewsReserved, got.ViewsReservedAt)
	}
	if got.RevokedAt != nil {
		t.Errorf("share's RevokedAt = %v after the sweeps, want nil -- a stale reservation alone must not revoke a live share; its legal recipient keeps the share", got.RevokedAt)
	}
}
