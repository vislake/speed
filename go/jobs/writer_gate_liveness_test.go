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
// Named for the behaviour it verifies, since it exercises
// StandaloneQueue.Start/Close, store.go's queue_writers registration and
// worker.go's heartbeat keeper together; sibling of single_writer_test.go,
// which pins the gate itself.
//
// The registration lapses in exactly two places unless the heartbeat is
// decoupled from the dispatcher poll tick: the dispatcher blocks on the
// worker handoff while every worker is busy (trigger 1), and it exits on
// stopCh while Close waits for workers to finish their current Handle
// (trigger 2). A lapse past the stale window would let a second Start steal
// the registration, resetInterruptedRecords would flip the first queue's
// mid-Handle row back to pending, and the second queue would claim and
// execute it a second time. Every test here shrinks the stale window on
// the INCUMBENT (q.writerStaleAfter, the window the queue authors into its
// own registration) so a lapse -- or a takeover, in the fourth test -- is
// reached in milliseconds instead of the production two-second floor; the
// heartbeat cadence stays the poll interval, an order of magnitude below
// the shrunken window, so a live queue can never look stale.
//
// The fourth regression arms the two sides DIFFERENTLY -- the incumbent
// beating at a cadence slower than the TAKER's own stale window. Because
// registrations carry their own stale moment (store.go's stale_at, authored
// by the owner from its own window), the taker judges the incumbent only
// by that authored moment -- never by the taker's OWN window, which would
// make an incumbent beating slower than it look crashed between beats and
// let a second Start steal a live writer's mid-Handle row into a double
// execution. The three liveness tests below all set both sides to the same
// 150ms, a construction that cannot expose that asymmetry; the fourth arms
// the sides differently on purpose.

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
// trigger 1 of the writer-gate staleness hazard: workerCount 1 and
// tenantConcurrency 2, two jobs of one tenant. The worker enters job1's long
// Handle; the dispatcher claims job2 and blocks at the worker handoff; while
// blocked it never returns to its tick branch, so any heartbeat riding that
// tick would stop. After the stale window a second StandaloneQueue.Start on
// the same database must STILL be refused: the heartbeat must not depend on
// the dispatcher being able to reach its tick. A second Start that
// succeeded would reset the first queue's mid-Handle row back to pending and
// run its Handle on the same job while the first is still in flight (the
// executionCounter proves the double run).
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
	// q1's OWN stale window, shrunk so the lapse this test pins reaches its
	// takeover threshold in milliseconds: the number q1's registration
	// authors into its stale moment, which is what a taker judges.
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
	waitSignal(t, entered, "q1's worker to enter job1's Handle -- the mid-Handle window never opened")

	// job2 shares job1's tenant, so its per-tenant slot is free (limit 2) and
	// the dispatcher claims it -- and then blocks handing it over, because the
	// one worker is still inside job1's Handle. The claimed row is the
	// deterministic marker that the dispatcher reached that block: it never
	// returns to its tick, and a heartbeat riding that tick would never
	// reach the database again from here.
	id2, err := q1.Enqueue(context.Background(), Task{Type: "writer-gate.blocked-handoff", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue(job2) error = %v", err)
	}
	waitRowStatus(t, db1, id2, StatusRunning, 3*time.Second)

	// The keeper heartbeats on its own ticker, independent of the
	// dispatcher's claim tick, so q1's registration stays fresh through the
	// whole handoff block; this wait (one full stale window plus poll
	// margin) is exactly the interval that must not go unbeaten.
	time.Sleep(q1.writerStaleAfter + 3*q1.pollInterval)

	q2 := NewStandaloneQueue(db2, opts...)
	// No stale-window override on the ACQUIRING side, deliberately: the
	// taker's own window plays no part in judging the incumbent -- q1's
	// registration carries its own stale moment (q1's override above),
	// refreshed by q1's own beats, and that is the only number judged.
	// (One shared value on both sides would make the taker's own window
	// judge the incumbent -- the cadence asymmetry the test below pins.)
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
		// The failure this branch detects: a second writer that stole the
		// gate, reset the first queue's mid-Handle row and re-claimed it.
		// Wait for its own Handle on the SAME job to enter while the first
		// Handle is still in flight (release is still shut) -- the double
		// execution, live.
		waitExecCount(t, execs, string(id1), 2, 3*time.Second)
		t.Errorf("second StandaloneQueue.Start() error = nil while the first queue's dispatcher was blocked mid-handoff; Handle entered a second time for job %q with the first Handle still in flight (executions = %d)", id1, execs.count(string(id1)))
	} else {
		if !apperr.HasCode(q2Err, ErrQueueWriterActive.Code) {
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
// of the writer-gate staleness hazard: Close closes stopCh, the dispatcher
// exits immediately, and Close's wg.Wait then waits for the worker to finish
// its current Handle. A graceful shutdown whose in-flight Handle outlives
// the stale window would open the gate mid-drain -- rolling restarts hit
// it. The registration heartbeat must keep beating through the drain, until
// every worker has finished, and only then stop (the registration itself is
// released after). A second Start during the drain that stole the stale
// registration would run its Handle on the same job while the first queue's
// worker is still mid-Handle.
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
	// q1's OWN stale window, shrunk so the lapse this test pins reaches its
	// takeover threshold in milliseconds: the number q1's registration
	// authors into its stale moment, which is what a taker judges.
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
	waitSignal(t, entered, "q1's worker to enter Handle -- the mid-Handle window never opened")

	// Close with the Handle still in flight: it must block until release
	// lets the worker finish, and must NOT open the writer gate meanwhile.
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closeDone <- q1.Close(ctx)
	}()

	// The dispatcher exits when stopCh closes; a heartbeat riding its tick
	// would stop from that moment. Poll for the real close so the stale
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

	// The registration is stale once this elapses and the worker is still
	// inside Handle (release is still shut) -- the drain has crossed the
	// stale window; the keeper must keep beating through the drain.
	time.Sleep(q1.writerStaleAfter + 3*q1.pollInterval)

	q2 := NewStandaloneQueue(db2, opts...)
	// No stale-window override on the ACQUIRING side, deliberately: the
	// taker's own window plays no part in judging the incumbent -- q1's
	// registration carries its own stale moment (q1's override above),
	// refreshed by q1's own beats, and that is the only number judged.
	// (One shared value on both sides would make the taker's own window
	// judge the incumbent -- the cadence asymmetry the test below pins.)
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
		// The failure this branch detects: a second writer that stole the
		// gate mid-drain and re-claimed the row the first queue's worker is
		// still executing -- its Handle entering a second time while the
		// first is in flight (release is still shut).
		waitExecCount(t, execs, string(id1), 2, 3*time.Second)
		t.Errorf("second StandaloneQueue.Start() error = nil during the first queue's Close drain; Handle entered a second time for job %q with the first Handle still in flight (executions = %d)", id1, execs.count(string(id1)))
	} else {
		if !apperr.HasCode(q2Err, ErrQueueWriterActive.Code) {
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
// the baseline control the two triggers above must not disturb: an idle
// first queue -- no jobs at all, dispatcher walking its clock with nothing
// to do -- whose registration has outlived the stale window must still
// refuse a second Start. The idle queue's own beats keep the registration
// fresh; this test pins that a heartbeat tied to worker activity instead of
// queue liveness would quietly reopen the gate on the quietest possible
// queue.
func TestStandaloneQueue_SecondStart_IdleQueuePastStaleWindow_StillRefused(t *testing.T) {
	db1, db2 := twoPoolSQLite(t)

	opts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithWorkerCount(1),
	}
	q1 := NewStandaloneQueue(db1, opts...)
	// q1's OWN stale window, shrunk so the lapse this test pins reaches its
	// takeover threshold in milliseconds: the number q1's registration
	// authors into its stale moment, which is what a taker judges.
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
	// Again no override on the acquiring side: an idle-but-live queue
	// cannot be stolen because ITS OWN beats keep its authored stale moment
	// fresh -- not because the taker's window happens to match.
	q2Err := q2.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = q2.Close(ctx)
	})
	if q2Err == nil {
		t.Error("second StandaloneQueue.Start() error = nil for an idle first queue whose own liveness beats kept its registration fresh -- want ErrQueueWriterActive")
	} else if !apperr.HasCode(q2Err, ErrQueueWriterActive.Code) {
		t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
	}
}

// TestStandaloneQueue_SecondStart_IncumbentCadenceSlowerThanTakersWindow_StillRefused
// is the cadence-asymmetry regression: the incumbent beats at a cadence
// SLOWER than the taking side's stale window -- the two queues configured
// with different poll intervals -- the shape the three tests above cannot
// expose (they overwrite both sides to the same 150ms). The taker must
// judge the incumbent's registration only by the stale moment the
// registration's OWNER authored (its own window applied to its own beats):
// judging by the TAKER's own stale window would make an incumbent whose
// beats arrive slower than that window look crashed between beats, so a
// second Start on the same database would steal the live incumbent's
// registration, resetInterruptedRecords would flip its mid-Handle row back
// to pending, and the second queue's Handle on the same job would enter
// while the first was still in flight -- the double-execution hazard the
// single-writer gate prevents. The same second Start, at the same instant,
// is refused under the authored-moment judgment.
//
// The cadences are the production shape scaled to milliseconds: the
// incumbent polls once per second (beating every second, an order of
// magnitude inside its own ten-second window -- a live queue can never
// look stale), while the taker polls every ten milliseconds with its stale
// window shrunk to 150ms -- standing in for the default-configuration
// taker whose two-second floor sits below an incumbent configured at a
// multi-second poll interval. 300ms after one observed beat of the
// incumbent (150ms past the taker's window, 700ms before the incumbent's
// next beat) is the deterministic moment at which a taker-window judgment
// would steal and the authored-moment judgment refuses.
func TestStandaloneQueue_SecondStart_IncumbentCadenceSlowerThanTakersWindow_StillRefused(t *testing.T) {
	db1, db2 := twoPoolSQLite(t)

	entered := make(chan struct{}, 8)
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release)
	execs := &executionCounter{exec: make(map[string]int32)}
	handler := NewHandlerFunc("writer-gate.cadence", func(_ context.Context, job *Job, _ ProgressFn) (Result, error) {
		execs.add(string(job.ID))
		select {
		case entered <- struct{}{}:
		default:
		}
		<-releaseCh
		return Result{}, nil
	})

	// The incumbent runs at its own cadence -- a one-second poll interval,
	// its stale window computed from THAT (ten seconds, unshrunk). Its
	// beats arrive every second, ten times inside its own window.
	slowOpts := []Option{
		WithPollInterval(time.Second),
		WithWorkerCount(1),
		WithTenantConcurrencyLimit(2),
	}
	q1 := NewStandaloneQueue(db1, slowOpts...)
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

	id, err := q1.Enqueue(context.Background(), Task{Type: "writer-gate.cadence", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	waitSignal(t, entered, "q1's worker to enter Handle -- the mid-Handle window never opened")

	// Anchor the takeover attempt to a REAL beat of the incumbent: poll the
	// queue_writers row until last_heartbeat advances past q1's Start (q1's
	// keeper beats once per second). Everything below is measured from that
	// beat, so the incumbent's liveness at the takeover moment is a fact of
	// the record, never an assumption about goroutine timing.
	startReturned := time.Now()
	var beatAt time.Time
	beatDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(beatDeadline) {
		var row struct {
			LastHeartbeat time.Time `gorm:"column:last_heartbeat"`
		}
		if err := db1.WithContext(context.Background()).
			Raw(`SELECT last_heartbeat FROM ` + queueWritersTable + ` WHERE id = 1`).
			Scan(&row).Error; err == nil && row.LastHeartbeat.After(startReturned) {
			beatAt = row.LastHeartbeat
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if beatAt.IsZero() {
		t.Fatal("timed out waiting for q1's registration heartbeat to advance past its Start -- the keeper never beat")
	}

	// Sleep to 300ms after that beat: 150ms past the taking side's shrunken
	// stale window below (the moment a taker-window judgment would call the
	// incumbent crashed), 700ms short of the incumbent's next beat (so the
	// live beat this test judges cannot be refreshed away mid-Start).
	time.Sleep(300 * time.Millisecond)

	// The taker runs at the OTHER cadence -- ten milliseconds -- and its
	// stale window is shrunk to 150ms: a window below the incumbent's
	// one-second beat cadence, the exact ratio at which a taker-window
	// judgment would steal a live writer. This number authors only the
	// taker's OWN registration (which never lands -- the taker is refused),
	// and plays no part in judging the incumbent's authored stale moment.
	fastOpts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithWorkerCount(1),
		WithTenantConcurrencyLimit(2),
	}
	q2 := NewStandaloneQueue(db2, fastOpts...)
	q2.writerStaleAfter = 150 * time.Millisecond
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
		// The failure this branch detects: a second writer that stole the
		// live incumbent's registration, reset its mid-Handle row and
		// re-claimed it. Wait for its own Handle on the SAME job to enter
		// while the first Handle is still in flight (release is still shut)
		// -- the double execution, live, of a writer whose heartbeats were
		// arriving on time throughout.
		waitExecCount(t, execs, string(id), 2, 3*time.Second)
		t.Errorf("second StandaloneQueue.Start() error = nil while the incumbent's own heartbeats were still arriving on time; Handle entered a second time for job %q with the first Handle still in flight (executions = %d)", id, execs.count(string(id)))
	} else {
		if !apperr.HasCode(q2Err, ErrQueueWriterActive.Code) {
			t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
		}
	}

	release()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q1, ctx, id)
	if job.Status != StatusSucceeded {
		t.Errorf("job Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}
	if got := execs.count(string(id)); got != 1 {
		t.Errorf("job %q Handle ran %d times, want exactly 1 -- a live incumbent beating at its own cadence must never be taken over", id, got)
	}
}
