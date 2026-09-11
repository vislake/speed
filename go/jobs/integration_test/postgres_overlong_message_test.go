//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is the PostgreSQL half of the overlong-message regression
// store_test.go's unit tier pins on SQLite. The jobs table's two
// descriptive text columns -- error_message VARCHAR(4000) and
// progress_msg VARCHAR(1000), spelled identically in both of store.go's
// createJobsTableSQL statements -- were fed caller-supplied text of
// unbounded length (worker.go persists cause.Error() of a handler error;
// the ProgressFn text is handler-supplied too), and nothing in the module
// truncated. SQLite never enforces a VARCHAR width, so the unit tier
// cannot show what that meant on the other dbkit dialect: PostgreSQL
// enforces the width by character count, an overlong value makes the
// transition UPDATE fail with 22001, the task never reaches its terminal
// state, and the only recovery -- the next Start's resetInterruptedRecords
// -- re-runs the handler, which fails with the same overlong error, whose
// write is refused again: the state machine cannot advance without a code
// fix. The unit tests assert the application-layer cut; this test drives
// the real worker over a real PostgreSQL server and asserts the terminal
// state is genuinely reached with the stored values cut exactly to the
// declared widths -- an uncut value would leave the running-state wedge
// (the 22001), and a truncation width drifting from the DDL fails again
// (wider than the column re-creates the 22001; narrower merely stores
// less).
func TestStandaloneQueue_PostgresOverlongFailureMessage_ReachesTerminalState(t *testing.T) {
	// The truncation warnings go through obs.FromContext over a context
	// with no attached logger, which falls back to slog.Default() read
	// fresh per call -- the module's established capture seam (see
	// worker_test.go). No test in this package runs in parallel. buf is a
	// plain bytes.Buffer that the queue's worker goroutines write into, so
	// every read of it below follows closeQueue (which joins the worker
	// pool); the unit tier's equivalent captures are safe because they call
	// the worker machinery synchronously in the test goroutine.
	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	db := dbtest.NewPostgres(t)
	q := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(10*time.Millisecond),
		jobs.WithWorkerCount(1),
	)

	const jobType = "pg.overlong_failure"
	// The failure text a handler returns is unbounded: 500 characters past
	// the error_message column's 4000-character width. The progress report
	// is 500 past progress_msg's 1000-character width.
	handlerErr := errors.New(strings.Repeat("y", 4500))
	progressMsg := strings.Repeat("p", 1500)
	handled := make(chan struct{}, 1)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(jobType, func(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
		progress(10, progressMsg)
		select {
		case handled <- struct{}{}:
		default:
		}
		return jobs.Result{}, handlerErr
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// Start applies ensureJobsSchema (CREATE TABLE jobs with the VARCHAR
	// columns at their declared widths) against the real PostgreSQL.
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("StandaloneQueue.Start over PostgreSQL error = %v", err)
	}
	// closeQueue joins every goroutine the queue owns: Close stops the
	// poller, wg.Wait()s the worker pool and stops the writer heartbeat
	// (queue_standalone.go). It is idempotent (closeOnce) and safe to call
	// more than once, so one helper serves both the in-function calls
	// below -- every read of the capture buffer must follow it -- and this
	// cleanup, which covers the early t.Fatalf paths.
	closeQueue := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := q.Close(ctx); err != nil {
			t.Errorf("StandaloneQueue.Close error = %v", err)
		}
	}
	t.Cleanup(closeQueue)

	tenant := pkgcore.TenantID("tenant-pg-overlong")
	id, err := q.Enqueue(context.Background(), jobs.Task{
		Type:           jobType,
		TenantID:       tenant,
		IdempotencyKey: "pg-overlong-1",
	}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// WithMaxRetries(0), the first (and only) attempt's failure is a
	// dead-letter: the attempt settles within milliseconds of the handler
	// returning. An overlong error_message uncut at the write would be
	// refused with 22001 and the row would stay StatusRunning -- nothing
	// else in-process ever touches it again (retry-budget bookkeeping is
	// precisely the write that cannot land), so the bounded wait below
	// fails with the row's running status and the worker's captured 22001
	// log as the evidence of the wedge.
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	deadline := time.Now().Add(15 * time.Second)
	var job *jobs.Job
	for {
		job, err = q.Get(ctx, id)
		if err == nil && job.Status == jobs.StatusDeadLetter {
			break
		}
		if err == nil && job.Status == jobs.StatusSucceeded {
			t.Fatalf("job reached StatusSucceeded though its handler always fails -- impossible")
		}
		if time.Now().After(deadline) {
			// The failure message reads the capture buffer below, so the
			// worker pool is joined first -- same discipline as the
			// post-loop closeQueue call (see its comment).
			closeQueue()
			var status jobs.Status
			if job != nil {
				status = job.Status
			}
			t.Fatalf("job never reached a terminal state on PostgreSQL within 15s (last status %q, last error %v): pre-fix this is the wedge -- the terminal-transition UPDATE was refused for the overlong error_message and only a process restart re-attempts, re-failing with the same overlong error. Captured worker log:\n%s",
				status, err, buf.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The queue is closed before any of the capture-buffer reads below:
	// settleFailedAttempt's dead-letter branch logs its "job exhausted
	// retries, moved to dead letter" record strictly AFTER completeDeadLetter
	// commits the terminal UPDATE (the record must not precede the write,
	// or a write a concurrent Cancel no-ops would leave a cancelled Job
	// logged as dead-lettered -- worker.go), so this poll loop can observe
	// StatusDeadLetter while the worker is still emitting that post-commit
	// log line into buf. bytes.Buffer is not safe for a read concurrent
	// with that write -- -race flags exactly that pair (buf.String() here
	// versus the worker's slog write through obs.FromContext's fallback,
	// which reads slog.Default() -- this test's TextHandler over buf --
	// fresh per call) -- and the join makes the race impossible rather than
	// merely rarer: after Close returns nil no queue goroutine exists that
	// can write to buf again.
	closeQueue()

	select {
	case <-handled:
	default:
		t.Fatal("handler never ran -- the wedge below cannot be blamed on the handler not executing")
	}

	if want := strings.Repeat("y", 4000); job.Error != want {
		t.Errorf("stored error_message = %d characters, want exactly %d (the declared VARCHAR(4000) width, head of the cause preserved): %q...", len(job.Error), len(want), truncateForMessage(job.Error))
	}
	if want := strings.Repeat("p", 1000); job.ProgressMsg != want {
		t.Errorf("stored progress_msg = %d characters, want exactly %d (the declared VARCHAR(1000) width, head of the message preserved)", len(job.ProgressMsg), len(want))
	}

	out := buf.String()
	for _, want := range []string{"jobs: message changed to fit its column", "column=error_message", "column=progress_msg", "reason=column_width"} {
		if !strings.Contains(out, want) {
			t.Errorf("captured worker log missing %q -- the structured truncation warning must name the column, the width and the reason:\n%s", want, out)
		}
	}
}

// truncateForMessage shortens a possibly enormous string for embedding in
// a test failure message.
func truncateForMessage(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max]
}
