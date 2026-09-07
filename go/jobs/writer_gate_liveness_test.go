package jobs

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectSQLite with dbkit's dialect registry so the dbkit.Open
	// calls in twoPoolSQLite below have a driver to build a gorm.Dialector
	// from. The package's other test files import dbtest (which itself
	// blank-imports this driver), but this file opens its own databases
	// directly and must not depend on another file's import for its
	// dialect to exist.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the writer-gate liveness regressions: StandaloneQueue's
// single-writer registration (store.go's queue_writers row) must stay fresh
// while a worker holds a claimed row -- through the dispatcher's blocked
// dispatch handoff and through Close's drain of in-flight Handles -- so a
// concurrent Start on the same database stays refused (ErrQueueWriterActive)
// for as long as any Handle of the incumbent queue may still be running.
// Named for the behaviour it verifies, per the backend coding standard's
// test-naming rule, since it exercises StandaloneQueue.Start/Close,
// store.go's queue_writers registration and worker.go's heartbeat keeper
// together; sibling of single_writer_test.go, which pins the gate itself.
//
// Before the heartbeat was decoupled from the dispatcher poll tick, the
// registration lapsed in exactly two places: the dispatcher blocks on the
// worker handoff while every worker is busy (trigger 1) and it exits on
// stopCh while Close waits for workers to finish their current Handle
// (trigger 2). A lapse past writerStaleAfter lets a second Start steal the
// registration, resetInterruptedRecords flips the first queue's mid-Handle
// row back to pending, and the second queue claims and executes it a second
// time. Every test here shrinks the stale window (q.writerStaleAfter, the
// same field Start's acquire reads) so the lapse is reached in milliseconds
// instead of the production two-second floor; the heartbeat cadence stays
// the poll interval, an order of magnitude below the shrunken window, so a
// live queue can never look stale in the fixed code.

// twoPoolSQLite opens two independent connection pools over one private
// temp-file SQLite database -- the shape of two processes sharing one jobs
// table, and the shape every double-writer hazard needs to be reproduced
// under. dbtest.NewSQLite returns one pool only, so the second pool is
// opened via dbkit.Open directly over the same file path.
func twoPoolSQLite(t *testing.T) (*gorm.DB, *gorm.DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "dbtest.sqlite")
	open := func() *gorm.DB {
		db, err := dbkit.Open(context.Background(), dbkit.Options{
			Dialect: dbkit.DialectSQLite,
			DSN:     dsn,
		})
		if err != nil {
			t.Fatalf("dbkit.Open(%q) error = %v", dsn, err)
		}
		t.Cleanup(func() {
			sqlDB, dbErr := db.DB()
			if dbErr != nil {
				return
			}
			_ = sqlDB.Close()
		})
		return db
	}
	return open(), open()
}

// waitRowStatus polls the persisted row until its status is want, or fails
// the test at timeout.
func waitRowStatus(t *testing.T, db *gorm.DB, id JobID, want Status, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rec, err := findByID(context.Background(), db, id)
		if err == nil && Status(rec.Status) == want {
			return
		}
		if time.Now().After(deadline) {
			got := "<unreadable>"
			if rec, err := findByID(context.Background(), db, id); err == nil {
				got = rec.Status
			}
			t.Fatalf("timed out after %v waiting for row %q to reach status %q; last status %q", timeout, id, want, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitExecCount polls until the handler has entered count times for job id,
// or fails the test at timeout.
func waitExecCount(t *testing.T, execs *executionCounter, id string, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for execs.count(id) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %d Handle executions of job %q; got %d", timeout, want, id, execs.count(id))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// executionCounter counts Handle executions per job id, for the
// exactly-once assertions the gate tests below make. The count for job1
// reaching two while the release gate is still shut is the deterministic
// double-execution evidence: two Handles of one row in flight at once.
type executionCounter struct {
	mu   sync.Mutex
	exec map[string]int32
}

func (c *executionCounter) add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exec[id]++
}

func (c *executionCounter) count(id string) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exec[id]
}

// TestStandaloneQueue_SecondStart_DispatcherBlockedOnHandoff_StillRefused is
// trigger 1 of the writer-gate staleness defect: workerCount 1 and
// tenantConcurrency 2, two jobs of one tenant. The worker enters job1's long
// Handle; the dispatcher claims job2 and blocks at the worker handoff; while
// blocked it never returns to its tick branch, so the registration heartbeat
// (which rode that tick) stopped. After the stale window a second
// StandaloneQueue.Start on the same database must STILL be refused: the
// heartbeat must not depend on the dispatcher being able to reach its tick.
// Fails on the pre-fix code, where the second Start succeeds, its
// resetInterruptedRecords flips the first queue's mid-Handle row back to
// pending, and the second queue's Handle on the same job enters while the
// first one is still in flight (the executionCounter proves the double run).
func TestStandaloneQueue_SecondStart_DispatcherBlockedOnHandoff_StillRefused(t *testing.T) {
	db1, db2 := twoPoolSQLite(t)

	entered := make(chan struct{}, 8)
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	execs := &executionCounter{exec: make(map[string]int32)}
	handler := NewHandlerFunc("writer-gate.blocked-handoff", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		execs.add(string(job.ID))
		select {
		case entered <- struct{}{}:
		default:
		}
		<-releaseCh
		return Result{}, nil
	})

	opts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithWorkerCount(1),
		WithTenantConcurrencyLimit(2),
	}
	q1 := NewStandaloneQueue(db1, opts...)
	q1.writerStaleAfter = 150 * time.Millisecond
	if err := q1.RegisterHandler(handler); err != nil {
		t.Fatalf("q1 RegisterHandler() error = %v", err)
	}
	if err := q1.Start(context.Background()); err != nil {
		t.Fatalf("q1 Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q1.Close(ctx)
	})

	id1, err := q1.Enqueue(context.Background(), Task{Type: "writer-gate.blocked-handoff", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(job1) error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for q1's worker to enter job1's Handle -- the mid-Handle window never opened")
	}

	// job2 shares job1's tenant, so its per-tenant slot is free (limit 2) and
	// the dispatcher claims it -- and then blocks handing it over, because the
	// one worker is still inside job1's Handle. The claimed row is the
	// deterministic marker that the dispatcher reached that block: it never
	// returns to its tick (and pre-fix, never heartbeats again) from here.
	id2, err := q1.Enqueue(context.Background(), Task{Type: "writer-gate.blocked-handoff", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(job2) error = %v", err)
	}
	waitRowStatus(t, db1, id2, StatusRunning, 3*time.Second)

	// Pre-fix, the last heartbeat is the tick that claimed job2; the
	// registration is stale once the window elapses. Post-fix the keeper
	// heartbeats on its own ticker through the whole handoff block, so this
	// wait is exactly the condition the fix must survive.
	time.Sleep(q1.writerStaleAfter + 3*q1.pollInterval)

	q2 := NewStandaloneQueue(db2, opts...)
	q2.writerStaleAfter = 150 * time.Millisecond // the acquiring side judges staleness against its own window
	if err := q2.RegisterHandler(handler); err != nil {
		t.Fatalf("q2 RegisterHandler() error = %v", err)
	}
	q2Err := q2.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q2.Close(ctx)
	})

	if q2Err == nil {
		// Pre-fix failure evidence: the second writer stole the gate, reset
		// the first queue's mid-Handle row and re-claimed it. Wait for its
		// own Handle on the SAME job to enter while the first Handle is still
		// in flight (release is still shut) -- the double execution, live.
		waitExecCount(t, execs, string(id1), 2, 3*time.Second)
		t.Errorf("second StandaloneQueue.Start() error = nil while the first queue's dispatcher was blocked mid-handoff; Handle entered a second time for job %q with the first Handle still in flight (executions = %d)", id1, execs.count(string(id1)))
	} else {
		if appErr, ok := apperr.As(q2Err); !ok || appErr.Code != ErrQueueWriterActive.Code {
			t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
		}
	}

	release()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job1 := waitTerminal(t, q1, ctx, id1)
	if job1.Status != StatusSucceeded {
		t.Errorf("job1 Status = %v, want %v (job: %+v)", job1.Status, StatusSucceeded, job1)
	}
	job2 := waitTerminal(t, q1, ctx, id2)
	if job2.Status != StatusSucceeded {
		t.Errorf("job2 Status = %v, want %v (job: %+v)", job2.Status, StatusSucceeded, job2)
	}
	if got := execs.count(string(id1)); got != 1 {
		t.Errorf("job %q Handle ran %d times, want exactly 1 -- a second writer must never reset and re-claim a row its sibling is executing", id1, got)
	}
	if got := execs.count(string(id2)); got != 1 {
		t.Errorf("job %q Handle ran %d times, want exactly 1", id2, got)
	}
}

// TestStandaloneQueue_SecondStart_DuringCloseDrain_StillRefused is trigger 2
// of the writer-gate staleness defect: Close closes stopCh, the dispatcher
// exits immediately, and Close's wg.Wait then waits for the worker to finish
// its current Handle. Any graceful shutdown whose in-flight Handle outlives
// the stale window used to open the gate mid-drain -- rolling restarts hit
// it. The registration heartbeat must keep beating through the drain, until
// every worker has finished, and only then stop (the registration itself is
// released after). Fails on the pre-fix code: a second Start during the
// drain steals the stale registration and its Handle on the same job enters
// while the first queue's worker is still mid-Handle.
func TestStandaloneQueue_SecondStart_DuringCloseDrain_StillRefused(t *testing.T) {
	db1, db2 := twoPoolSQLite(t)

	entered := make(chan struct{}, 8)
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	execs := &executionCounter{exec: make(map[string]int32)}
	handler := NewHandlerFunc("writer-gate.close-drain", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		execs.add(string(job.ID))
		select {
		case entered <- struct{}{}:
		default:
		}
		<-releaseCh
		return Result{}, nil
	})

	opts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithWorkerCount(1),
		WithTenantConcurrencyLimit(2),
	}
	q1 := NewStandaloneQueue(db1, opts...)
	q1.writerStaleAfter = 150 * time.Millisecond
	if err := q1.RegisterHandler(handler); err != nil {
		t.Fatalf("q1 RegisterHandler() error = %v", err)
	}
	if err := q1.Start(context.Background()); err != nil {
		t.Fatalf("q1 Start() error = %v", err)
	}

	id1, err := q1.Enqueue(context.Background(), Task{Type: "writer-gate.close-drain", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for q1's worker to enter Handle -- the mid-Handle window never opened")
	}

	// Close with the Handle still in flight: it must block until release
	// lets the worker finish, and must NOT open the writer gate meanwhile.
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closeDone <- q1.Close(ctx)
	}()

	// The dispatcher exits when stopCh closes; from that moment pre-fix no
	// heartbeat reaches the database. Poll for the real close so the stale
	// window below is measured from it, not from Close's call.
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-q1.stopCh:
			goto dispatcherGone
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Close to shut the dispatcher (stopCh never closed)")
		}
		time.Sleep(2 * time.Millisecond)
	}
dispatcherGone:

	// Pre-fix, the registration is stale once this elapses and the worker is
	// still inside Handle (release is still shut) -- the drain has crossed
	// the stale window. Post-fix the keeper keeps beating through the drain.
	time.Sleep(q1.writerStaleAfter + 3*q1.pollInterval)

	q2 := NewStandaloneQueue(db2, opts...)
	q2.writerStaleAfter = 150 * time.Millisecond // the acquiring side judges staleness against its own window
	if err := q2.RegisterHandler(handler); err != nil {
		t.Fatalf("q2 RegisterHandler() error = %v", err)
	}
	q2Err := q2.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q2.Close(ctx)
	})

	if q2Err == nil {
		// Pre-fix failure evidence: the second writer stole the gate mid-
		// drain and re-claimed the row the first queue's worker is still
		// executing -- its Handle enters a second time while the first is in
		// flight (release is still shut).
		waitExecCount(t, execs, string(id1), 2, 3*time.Second)
		t.Errorf("second StandaloneQueue.Start() error = nil during the first queue's Close drain; Handle entered a second time for job %q with the first Handle still in flight (executions = %d)", id1, execs.count(string(id1)))
	} else {
		if appErr, ok := apperr.As(q2Err); !ok || appErr.Code != ErrQueueWriterActive.Code {
			t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
		}
	}

	release()
	if err := <-closeDone; err != nil {
		t.Errorf("q1.Close() error = %v, want nil (the drain finishes once the in-flight Handle completes)", err)
	}

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q1, ctx, id1)
	if job.Status != StatusSucceeded {
		t.Errorf("job Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}
	if got := execs.count(string(id1)); got != 1 {
		t.Errorf("job %q Handle ran %d times, want exactly 1 -- Close must keep the writer gate shut until every worker has finished", id1, got)
	}
}

// TestStandaloneQueue_SecondStart_IdleQueuePastStaleWindow_StillRefused is
// the reviewer-baseline control the two triggers above must not disturb: an
// idle first queue -- no jobs at all, dispatcher walking its clock with
// nothing to do -- whose registration has outlived the stale window must
// still refuse a second Start. Before and after the fix the idle queue's own
// beats keep the registration fresh; this test pins that a future heartbeat
// change (say, one tied to worker activity instead of queue liveness) cannot
// quietly reopen the gate on the quietest possible queue.
func TestStandaloneQueue_SecondStart_IdleQueuePastStaleWindow_StillRefused(t *testing.T) {
	db1, db2 := twoPoolSQLite(t)

	opts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithWorkerCount(1),
	}
	q1 := NewStandaloneQueue(db1, opts...)
	q1.writerStaleAfter = 150 * time.Millisecond
	if err := q1.Start(context.Background()); err != nil {
		t.Fatalf("q1 Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q1.Close(ctx)
	})

	// Idle for more than double the stale window: nothing was ever enqueued,
	// so any beat reaching the database is the queue's own liveness signal.
	time.Sleep(2*q1.writerStaleAfter + 3*q1.pollInterval)

	q2 := NewStandaloneQueue(db2, opts...)
	q2.writerStaleAfter = 150 * time.Millisecond // same window on the acquiring side, so an idle-but-live queue cannot be stolen
	q2Err := q2.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q2.Close(ctx)
	})
	if q2Err == nil {
		t.Error("second StandaloneQueue.Start() error = nil for an idle first queue whose own liveness beats kept its registration fresh -- want ErrQueueWriterActive")
	} else if appErr, ok := apperr.As(q2Err); !ok || appErr.Code != ErrQueueWriterActive.Code {
		t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
	}
}
