package pki

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the window semantics of the two platform-wide periodic
// tasks' idempotency keys -- the expiry scan (job.go's
// expiryScanIdempotencyKey, DefaultExpiryScanWindow) and the CRL
// regeneration (crl.go's crlRegenerateIdempotencyKey,
// DefaultCRLRegenerateWindow): enqueues inside one window collapse into
// one job (the concurrency protection the keys exist for, preserved: a
// multi-replica scheduler's same-window ticks must never fire N scans or N
// regenerations at once), an enqueue in a later window becomes a new job
// and runs again (periodicity -- without a window, jobs' unconditional
// idempotency would give each task exactly one run per database file), and
// the window boundary is the absolute-clock truncation the clock read
// falls in, sized per the stated ratio to the reference host's scheduler
// cadence.
//
// The collapse tests run against a REAL jobs.StandaloneQueue over a real
// SQLite database, the same shape go/storage's sweep_window_test.go uses:
// the dedupe behaviour under test lives in jobs' partial unique index and
// row semantics, which a fake queue cannot exercise. The windowed
// idempotency key is what the collapse tests pin: without it every enqueue
// would create its own independent job.
//
// EnqueueExpiryScan and EnqueueCRLRegenerate return no job id (both are
// fire-and-forget schedule points), so the tests observe the queue's own
// database -- the same *gorm.DB the queue was started over -- for row
// counts and ids, plus the registered handler's run channel for
// executions.

// windowA and windowB are two instants in two different one-hour windows
// (10:15 and 11:15 UTC), windowB exactly one DefaultExpiryScanWindow after
// windowA. Both tasks' default windows are one hour (the constants
// asserted below), so the pair straddles a boundary for either task.
var (
	windowA = time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	windowB = windowA.Add(DefaultExpiryScanWindow)
)

// TestEnqueueWindowDefaults_SatisfyTheWindowToIntervalRatio pins the
// sizing contract both window constants document: the window must be
// significantly larger than the reference host's scheduler interval (one
// minute, jobs.DefaultScheduleInterval, the jobs.Scheduler default the
// reference app runs its schedules at),
// or every tick would land in a fresh window and the dedup would be void.
// The 60:1 ratio is asserted as a relationship between the two constants
// so a change to either surfaces here, at the contract, rather than
// silently voiding the dedup in every host that ticks once a minute.
func TestEnqueueWindowDefaults_SatisfyTheWindowToIntervalRatio(t *testing.T) {
	const referenceHostSchedulerInterval = time.Minute
	if DefaultExpiryScanWindow != 60*referenceHostSchedulerInterval {
		t.Errorf("DefaultExpiryScanWindow = %v, want 60x the reference host's 1-minute scheduler interval (a window approaching the interval voids the dedup)", DefaultExpiryScanWindow)
	}
	if DefaultCRLRegenerateWindow != 60*referenceHostSchedulerInterval {
		t.Errorf("DefaultCRLRegenerateWindow = %v, want 60x the reference host's 1-minute scheduler interval (a window approaching the interval voids the dedup)", DefaultCRLRegenerateWindow)
	}
}

// startEnqueueWindowQueue starts a real StandaloneQueue over its own fresh
// database with fast intervals, returning both the queue and its database
// (jobs' tables are created by Start itself, go/jobs' ensureJobsSchema).
func startEnqueueWindowQueue(t *testing.T) (*jobs.StandaloneQueue, *gorm.DB) {
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

// windowRowCount counts the rows of one task type in the queue's own
// database: one per idempotency-key window resolved, whether the row is
// pending, running or already settled.
func windowRowCount(t *testing.T, db *gorm.DB, taskType string) int64 {
	t.Helper()
	var n int64
	if err := db.Table("jobs").Where("type = ?", taskType).Count(&n).Error; err != nil {
		t.Fatalf("count %q rows: %v", taskType, err)
	}
	return n
}

// firstWindowJobID returns the id of the database's only row of taskType.
func firstWindowJobID(t *testing.T, db *gorm.DB, taskType string) jobs.JobID {
	t.Helper()
	if n := windowRowCount(t, db, taskType); n != 1 {
		t.Fatalf("%q rows = %d, want exactly 1 before reading the first job id", taskType, n)
	}
	var ids []string
	if err := db.Table("jobs").Where("type = ?", taskType).Pluck("id", &ids).Error; err != nil {
		t.Fatalf("read first %q job id: %v", taskType, err)
	}
	return jobs.JobID(ids[0])
}

// waitForWindowRun waits until a handler run lands on runs and returns its
// job id, failing the test after timeout. runs must be a buffered channel
// the handler fills once per Handle call.
func waitForWindowRun(t *testing.T, runs chan jobs.JobID, what string) jobs.JobID {
	t.Helper()
	select {
	case id := <-runs:
		return id
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no run within 20s", what)
		return ""
	}
}

// --- Expiry scan (job.go) ------------------------------------------------

// TestEnqueueExpiryScan_SameWindowEnqueuesCollapseIntoOneJob pins the
// expiry scan's same-window collapse: two enqueues inside one
// DefaultExpiryScanWindow window -- a second replica's tick ten minutes
// after the first's -- collapse into the first job (the real queue's
// idempotent Enqueue resolves the key to the existing row: one row, one
// run), so N scheduler replicas never fire N concurrent scans of one
// window. Without the windowed key the two enqueues would create two
// independent jobs and the scan would run twice.
func TestEnqueueExpiryScan_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	svc := newTestService(t)
	q, db := startEnqueueWindowQueue(t)
	svc.attachQueue(q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpiryScan, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := context.Background()
	svc.now = func() time.Time { return windowA }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("first EnqueueExpiryScan: %v", err)
	}
	// A second replica's tick ten minutes later -- still inside windowA's
	// DefaultExpiryScanWindow window.
	svc.now = func() time.Time { return windowA.Add(10 * time.Minute) }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("second EnqueueExpiryScan: %v", err)
	}

	if n := windowRowCount(t, db, taskTypeExpiryScan); n != 1 {
		t.Fatalf("expiry-scan rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row (fails on the keyless pre-fix enqueue, which created two independent jobs)", n)
	}
	first := waitForWindowRun(t, runs, "the collapsed scan")
	if first != firstWindowJobID(t, db, taskTypeExpiryScan) {
		t.Errorf("scan run job id = %s, want the row's id %s", first, firstWindowJobID(t, db, taskTypeExpiryScan))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("expiry scan ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnqueueExpiryScan_LaterWindowEnqueuesNewJobAndScansAgain pins the
// expiry scan's cross-window periodicity: an enqueue in a later window is
// a NEW job and the scan runs again -- the property that keeps the scan
// periodic on queues whose idempotency is unconditional. (A keyless task
// also runs each tick; this test guards the windowed key against
// regressing into a window-less constant key, which would resolve the
// first window's job forever and stop the scan after one run per database
// file.)
func TestEnqueueExpiryScan_LaterWindowEnqueuesNewJobAndScansAgain(t *testing.T) {
	svc := newTestService(t)
	q, db := startEnqueueWindowQueue(t)
	svc.attachQueue(q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpiryScan, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := context.Background()
	svc.now = func() time.Time { return windowA }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("first EnqueueExpiryScan: %v", err)
	}
	first := waitForWindowRun(t, runs, "window A's scan")

	// A tick one DefaultExpiryScanWindow later: windowB, a different window.
	svc.now = func() time.Time { return windowB }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("second EnqueueExpiryScan: %v", err)
	}

	if n := windowRowCount(t, db, taskTypeExpiryScan); n != 2 {
		t.Fatalf("expiry-scan rows = %d, want 2 -- the later window's enqueue must create a NEW job", n)
	}
	second := waitForWindowRun(t, runs, "window B's scan")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own scan", second)
	}
}

// TestEnqueueExpiryScan_WindowBoundaryIsPinnedByTheClock pins the expiry
// scan's window boundary against the pinned clock: the boundary is exactly
// expiryScanWindowStart's absolute-clock truncation -- two enqueues inside
// one window (the window's start, and one nanosecond before its end) share
// one key, and an enqueue AT the next window's start gets a fresh one. A
// keyless enqueue would carry an empty key and defeat the collapse; these
// key assertions refuse that shape.
func TestEnqueueExpiryScan_WindowBoundaryIsPinnedByTheClock(t *testing.T) {
	svc := newTestService(t)
	queue := &recordingQueue{}
	svc.attachQueue(queue)
	ctx := context.Background()

	windowStart := windowA.Truncate(svc.expiryScanWindow)
	svc.now = func() time.Time { return windowStart }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("EnqueueExpiryScan at the window start: %v", err)
	}
	// One nanosecond before the window ends -- the same window still.
	svc.now = func() time.Time { return windowStart.Add(svc.expiryScanWindow - time.Nanosecond) }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("EnqueueExpiryScan just before the window ends: %v", err)
	}
	if len(queue.tasks) != 2 {
		t.Fatalf("Enqueue was called %d times, want 2", len(queue.tasks))
	}
	wantInWindow := expiryScanIdempotencyKey(windowStart)
	if queue.tasks[0].IdempotencyKey != wantInWindow || queue.tasks[1].IdempotencyKey != wantInWindow {
		t.Errorf("same-window keys = %q and %q, want both %q (regression: the windowed key must collapse one window's enqueues)", queue.tasks[0].IdempotencyKey, queue.tasks[1].IdempotencyKey, wantInWindow)
	}

	// The next window's start: a fresh key naming the new window.
	nextStart := windowStart.Add(svc.expiryScanWindow)
	svc.now = func() time.Time { return nextStart }
	if err := svc.EnqueueExpiryScan(ctx); err != nil {
		t.Fatalf("EnqueueExpiryScan at the next window's start: %v", err)
	}
	if len(queue.tasks) != 3 {
		t.Fatalf("Enqueue was called %d times, want 3", len(queue.tasks))
	}
	wantNextWindow := expiryScanIdempotencyKey(nextStart)
	if queue.tasks[2].IdempotencyKey != wantNextWindow {
		t.Errorf("next-window key = %q, want %q -- each window must name its own key", queue.tasks[2].IdempotencyKey, wantNextWindow)
	}
}

// --- CRL regeneration (crl.go) ------------------------------------------

// TestEnqueueCRLRegenerate_SameWindowEnqueuesCollapseIntoOneJob is the
// CRL task's twin of TestEnqueueExpiryScan_SameWindowEnqueuesCollapseIntoOneJob:
// two regenerations enqueued inside one DefaultCRLRegenerateWindow window
// collapse into one job, so N scheduler replicas never fire N concurrent
// regenerations of one window (each of which would produce its own full
// document and advance the authority's crl_number register). Without the
// windowed key the two enqueues would create two independent jobs and
// regenerate twice.
func TestEnqueueCRLRegenerate_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	ca := newTestCAService(t)
	q, db := startEnqueueWindowQueue(t)
	ca.attachQueue(q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeCRLRegenerate, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := context.Background()
	ca.now = func() time.Time { return windowA }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("first EnqueueCRLRegenerate: %v", err)
	}
	// A second replica's tick ten minutes later -- still inside windowA's
	// DefaultCRLRegenerateWindow window.
	ca.now = func() time.Time { return windowA.Add(10 * time.Minute) }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("second EnqueueCRLRegenerate: %v", err)
	}

	if n := windowRowCount(t, db, taskTypeCRLRegenerate); n != 1 {
		t.Fatalf("CRL-regenerate rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row (fails on the keyless pre-fix enqueue, which created two independent jobs)", n)
	}
	first := waitForWindowRun(t, runs, "the collapsed regeneration")
	if first != firstWindowJobID(t, db, taskTypeCRLRegenerate) {
		t.Errorf("regeneration run job id = %s, want the row's id %s", first, firstWindowJobID(t, db, taskTypeCRLRegenerate))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("CRL regeneration ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnqueueCRLRegenerate_LaterWindowEnqueuesNewJobAndRegeneratesAgain is
// the CRL task's twin of
// TestEnqueueExpiryScan_LaterWindowEnqueuesNewJobAndScansAgain: an enqueue
// in a later window is a NEW job and regenerates again -- without which a
// default-validity CRL (DefaultCRLValidity, seven days) would outlive its
// NextUpdate with no scheduled refresh (the window-less constant-key
// failure mode).
func TestEnqueueCRLRegenerate_LaterWindowEnqueuesNewJobAndRegeneratesAgain(t *testing.T) {
	ca := newTestCAService(t)
	q, db := startEnqueueWindowQueue(t)
	ca.attachQueue(q)
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeCRLRegenerate, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := context.Background()
	ca.now = func() time.Time { return windowA }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("first EnqueueCRLRegenerate: %v", err)
	}
	first := waitForWindowRun(t, runs, "window A's regeneration")

	// A tick one DefaultCRLRegenerateWindow later: windowB, a different
	// window.
	ca.now = func() time.Time { return windowB }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("second EnqueueCRLRegenerate: %v", err)
	}

	if n := windowRowCount(t, db, taskTypeCRLRegenerate); n != 2 {
		t.Fatalf("CRL-regenerate rows = %d, want 2 -- the later window's enqueue must create a NEW job", n)
	}
	second := waitForWindowRun(t, runs, "window B's regeneration")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own regeneration", second)
	}
}

// TestEnqueueCRLRegenerate_WindowBoundaryIsPinnedByTheClock is the CRL
// task's twin of TestEnqueueExpiryScan_WindowBoundaryIsPinnedByTheClock:
// with a pinned clock, the window boundary is exactly
// crlRegenerateWindowStart's absolute-clock truncation. A keyless enqueue
// would carry an empty key and defeat the collapse; the key assertions
// below refuse that shape.
func TestEnqueueCRLRegenerate_WindowBoundaryIsPinnedByTheClock(t *testing.T) {
	ca := newTestCAService(t)
	queue := &recordingQueue{}
	ca.attachQueue(queue)
	ctx := context.Background()

	windowStart := windowA.Truncate(ca.crlRegenerateWindow)
	ca.now = func() time.Time { return windowStart }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("EnqueueCRLRegenerate at the window start: %v", err)
	}
	// One nanosecond before the window ends -- the same window still.
	ca.now = func() time.Time { return windowStart.Add(ca.crlRegenerateWindow - time.Nanosecond) }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("EnqueueCRLRegenerate just before the window ends: %v", err)
	}
	if len(queue.tasks) != 2 {
		t.Fatalf("Enqueue was called %d times, want 2", len(queue.tasks))
	}
	wantInWindow := crlRegenerateIdempotencyKey(windowStart)
	if queue.tasks[0].IdempotencyKey != wantInWindow || queue.tasks[1].IdempotencyKey != wantInWindow {
		t.Errorf("same-window keys = %q and %q, want both %q (regression: the windowed key must collapse one window's enqueues)", queue.tasks[0].IdempotencyKey, queue.tasks[1].IdempotencyKey, wantInWindow)
	}

	// The next window's start: a fresh key naming the new window.
	nextStart := windowStart.Add(ca.crlRegenerateWindow)
	ca.now = func() time.Time { return nextStart }
	if err := ca.EnqueueCRLRegenerate(ctx); err != nil {
		t.Fatalf("EnqueueCRLRegenerate at the next window's start: %v", err)
	}
	if len(queue.tasks) != 3 {
		t.Fatalf("Enqueue was called %d times, want 3", len(queue.tasks))
	}
	wantNextWindow := crlRegenerateIdempotencyKey(nextStart)
	if queue.tasks[2].IdempotencyKey != wantNextWindow {
		t.Errorf("next-window key = %q, want %q -- each window must name its own key", queue.tasks[2].IdempotencyKey, wantNextWindow)
	}
}

// TestExpiryScanKeyMatchesTheSchedulerDerivation pins the schedule
// migration's key identity: the same (task type, window) must resolve one
// idempotency key through the module's own schedule point and through the
// jobs.Scheduler's derivation over the module's declaration, or a
// scheduler tick and a manual enqueue landing in one window would run the
// scan twice. The key literal below is the pinned string.
func TestExpiryScanKeyMatchesTheSchedulerDerivation(t *testing.T) {
	svc := newTestService(t)
	queue := &recordingQueue{}
	svc.attachQueue(queue)

	windowStart := windowA.Truncate(svc.expiryScanWindow)
	svc.now = func() time.Time { return windowStart.Add(time.Minute) }
	if err := svc.EnqueueExpiryScan(context.Background()); err != nil {
		t.Fatalf("EnqueueExpiryScan: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("Enqueue was called %d times, want 1", len(queue.tasks))
	}
	manual := queue.tasks[0].IdempotencyKey
	if want := "pki.expiry_scan:2026-09-07T10:00:00Z"; manual != want {
		t.Fatalf("the manual path resolved key %q, want the pinned %q", manual, want)
	}

	decl := svc.expiryScanSchedule()
	if decl.Type != taskTypeExpiryScan || decl.Every != svc.expiryScanWindow {
		t.Errorf("declaration = %+v, want the site's own type %q and the configured window %s", decl, taskTypeExpiryScan, svc.expiryScanWindow)
	}
	if decl.Scope != pkgcore.PeriodicScopePlatform || decl.PlatformTenant != platformScanTenantID {
		t.Errorf("declaration = %+v, want platform scope under the scan sentinel %q", decl, platformScanTenantID)
	}
	if got := jobs.SchedulePlatformIdempotencyKey(decl.KeyPrefix, windowStart); got != manual {
		t.Errorf("the scheduler-derived key %q != the manual key %q -- one window would run twice", got, manual)
	}
}

// TestCRLRegenerateKeyMatchesTheSchedulerDerivation is the CRL task's twin
// of TestExpiryScanKeyMatchesTheSchedulerDerivation: the same (task type,
// window) resolves one key through the module's own schedule point and
// through the scheduler's derivation over the declaration. The key literal
// below is the pinned string.
func TestCRLRegenerateKeyMatchesTheSchedulerDerivation(t *testing.T) {
	ca := newTestCAService(t)
	queue := &recordingQueue{}
	ca.attachQueue(queue)

	windowStart := windowA.Truncate(ca.crlRegenerateWindow)
	ca.now = func() time.Time { return windowStart.Add(time.Minute) }
	if err := ca.EnqueueCRLRegenerate(context.Background()); err != nil {
		t.Fatalf("EnqueueCRLRegenerate: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("Enqueue was called %d times, want 1", len(queue.tasks))
	}
	manual := queue.tasks[0].IdempotencyKey
	if want := "pki.crl_regenerate:2026-09-07T10:00:00Z"; manual != want {
		t.Fatalf("the manual path resolved key %q, want the pinned %q", manual, want)
	}

	decl := ca.crlRegenerateSchedule()
	if decl.Type != taskTypeCRLRegenerate || decl.Every != ca.crlRegenerateWindow {
		t.Errorf("declaration = %+v, want the site's own type %q and the configured window %s", decl, taskTypeCRLRegenerate, ca.crlRegenerateWindow)
	}
	if decl.Scope != pkgcore.PeriodicScopePlatform || decl.PlatformTenant != platformCRLRegenerateTenantID {
		t.Errorf("declaration = %+v, want platform scope under the CRL sentinel %q", decl, platformCRLRegenerateTenantID)
	}
	if got := jobs.SchedulePlatformIdempotencyKey(decl.KeyPrefix, windowStart); got != manual {
		t.Errorf("the scheduler-derived key %q != the manual key %q -- one window would run twice", got, manual)
	}
}
