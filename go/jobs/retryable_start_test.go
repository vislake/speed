package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the Start-retryability regression: a Start that FAILED
// (here: a schema error injected by a wrongly-shaped jobs table) must be
// genuinely retryable -- a second Start after the cause is fixed must
// actually start the queue, not return nil with nothing running, which
// would leave every enqueued Job pending forever. Named for the behaviour
// it verifies.

// TestStandaloneQueue_Start_IsRetryableAfterFailure is the regression
// pinning that a FAILED Start must not permanently consume the Start gate:
// a Start that fails records no "started" state, so the second Start --
// after the host fixed whatever broke the first -- genuinely launches the
// dispatcher and workers. A second Start that returned nil without
// launching anything would leave every Job enqueued after it Pending
// forever, with no error anywhere telling the host why. Deterministic
// orchestration: Start #1 fails against a sabotaged table (a "jobs" table
// with the right name but none of the columns the dispatch index needs --
// CREATE TABLE IF NOT EXISTS no-ops and the index statement errors), the
// test drops the sabotage, and Start #2 must genuinely start the queue:
// the probe Job enqueued after it must reach StatusSucceeded -- a
// nil-returning second Start leaves the probe Pending and times out.
func TestStandaloneQueue_Start_IsRetryableAfterFailure(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := db.Exec(`CREATE TABLE ` + jobsTable + ` (id VARCHAR(36) NOT NULL PRIMARY KEY)`).Error; err != nil {
		t.Fatalf("sabotage the schema: %v", err)
	}

	q := NewStandaloneQueue(db,
		WithPollInterval(15*time.Millisecond),
		WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		WithWorkerCount(1),
	)
	if err := q.RegisterHandler(NewHandlerFunc("echo", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{Data: []byte("ok")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	if err := q.Start(context.Background()); err == nil {
		t.Fatal("first Start() error = nil, want an error (the sabotaged table must break schema creation)")
	}

	// The host fixes the cause: drop the sabotage so the next Start can
	// create the real schema.
	if err := db.Exec(`DROP TABLE ` + jobsTable).Error; err != nil {
		t.Fatalf("remove the sabotage: %v", err)
	}

	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v, want nil (a failed Start must be retryable once the cause is fixed)", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})

	id, err := q.Enqueue(context.Background(), Task{Type: "echo", TenantID: "tenant-a", Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	job := waitTerminal(t, q, ctx, id)
	if job.Status != StatusSucceeded {
		t.Fatalf("job Status = %v, want %v (job: %+v) -- a nil-returning Start that never launched the workers leaves every Job pending forever", job.Status, StatusSucceeded, job)
	}
}
