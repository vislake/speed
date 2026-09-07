package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the StandaloneQueue single-writer regressions: a second
// live StandaloneQueue on the same jobs table must be refused at Start
// (ErrQueueWriterActive), never allowed to reset and re-claim the first
// queue's mid-Handle rows into a double execution. Named for the behaviour
// it verifies, per the backend coding standard's test-naming rule, since
// it exercises StandaloneQueue.Start, store.go's queue_writers
// registration and worker.go's claim together.

// TestStandaloneQueue_SecondLiveWriterOnSameDatabase_IsRefused_NoDoubleHandle
// is the deterministic end-to-end regression for the unscoped
// resetInterruptedRecords defect: two StandaloneQueue instances over ONE
// jobs table, the first mid-Handle on a Job, the second starting up. Before
// the fix the second Start reset the first's StatusRunning row to Pending
// (no ownership concept existed) and claimed it for itself, so the same Job
// ran Handle twice -- on the money path, that is a double charge. After the
// fix the second Start must refuse (ErrQueueWriterActive, coded) because
// the first queue's writer registration is live, and Handle must run
// exactly once. Deterministic orchestration: the first queue's single
// worker is blocked inside Handle (entered/release channels) for the whole
// second Start, so the second queue's refusal -- or, pre-fix, its claim of
// the mid-Handle row -- happens while the row is provably mid-Handle.
func TestStandaloneQueue_SecondLiveWriterOnSameDatabase_IsRefused_NoDoubleHandle(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}

	var handles atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	handler := NewHandlerFunc("double-run", func(context.Context, *Job, ProgressFn) (Result, error) {
		handles.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return Result{}, nil
	})

	opts := []Option{
		WithPollInterval(10 * time.Millisecond),
		WithBackoff(10*time.Millisecond, 50*time.Millisecond),
		WithWorkerCount(1),
		WithTenantConcurrencyLimit(2),
	}
	q1 := NewStandaloneQueue(db, opts...)
	if err := q1.RegisterHandler(handler); err != nil {
		t.Fatalf("q1 RegisterHandler() error = %v", err)
	}
	if err := q1.Start(context.Background()); err != nil {
		t.Fatalf("q1 Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q1.Close(ctx)
	})

	id, err := q1.Enqueue(context.Background(), Task{Type: "double-run", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for q1's worker to enter Handle -- the mid-Handle window never opened")
	}

	// The second queue over the SAME database: pre-fix this Starts cleanly,
	// resets q1's mid-Handle row and runs Handle a second time.
	q2 := NewStandaloneQueue(db, opts...)
	if err := q2.RegisterHandler(handler); err != nil {
		t.Fatalf("q2 RegisterHandler() error = %v", err)
	}
	q2Err := q2.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q2.Close(ctx)
	})

	close(release)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q1, ctx, id)
	if job.Status != StatusSucceeded {
		t.Fatalf("job Status = %v, want %v (job: %+v)", job.Status, StatusSucceeded, job)
	}

	if q2Err == nil {
		t.Error("second StandaloneQueue.Start() error = nil, want a refusal: two live writers on one jobs table double-execute every row the second one resets")
	} else {
		if appErr, ok := apperr.As(q2Err); !ok || appErr.Code != ErrQueueWriterActive.Code {
			t.Errorf("second StandaloneQueue.Start() error = %v, want code %q", q2Err, ErrQueueWriterActive.Code)
		}
	}
	if got := handles.Load(); got != 1 {
		t.Errorf("Handle ran %d times across two queues on one database, want exactly 1 (the second writer must never reset and re-claim a mid-Handle row)", got)
	}
}
