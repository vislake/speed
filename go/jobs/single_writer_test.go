package jobs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// TestStandaloneQueue_Close_WithoutStart_SkipsTheWriterRelease is the
// regression for Close's release path firing on a queue whose Start never
// ran: Close without a prior Start used to issue the release DELETE anyway
// -- against a queue_writers table Start (the schema creator) never
// created -- so the DELETE errored and a "jobs: releasing writer
// registration failed" warning was printed on a correct, documented
// usage (Close's own contract: "safe to call ... without a prior Start").
// A warning that is guaranteed on a correct path is a false warning, so
// the release is now skipped entirely when no Start ever ran: there is no
// registration to release and no heartbeat keeper to stop, and the
// warning keeps its meaning (whenever it fires, a registration this queue
// held may genuinely be stuck). Fails on the pre-fix code, where the
// warning is printed.
func TestStandaloneQueue_Close_WithoutStart_SkipsTheWriterRelease(t *testing.T) {
	// Deliberately NO ensureJobsSchema: the fresh database is exactly the
	// never-started state -- the queue_writers table does not exist.
	db := dbtest.NewSQLite(t)
	q := NewStandaloneQueue(db)

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v, want nil (documented: safe without a prior Start)", err)
	}
	// A second Close must behave identically (idempotent, still silent).
	if err := q.Close(ctx); err != nil {
		t.Fatalf("second Close() error = %v, want nil (idempotent)", err)
	}

	if out := buf.String(); strings.Contains(out, "jobs: releasing writer registration failed") {
		t.Errorf("Close without a prior Start printed the release warning:\n%s\n(no Start ever acquired a registration, so the release path must be skipped -- nothing failed to release)", out)
	}
}
