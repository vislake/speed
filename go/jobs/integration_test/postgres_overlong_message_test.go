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
// declared widths -- the assertion that failed pre-fix as the
// running-state wedge, and fails again if a future change ever lets the
// truncation width drift from the DDL (a truncation wider than the column
// re-creates the 22001; a narrower one merely stores less).
func TestStandaloneQueue_PostgresOverlongFailureMessage_ReachesTerminalState(t *testing.T) {
	// The truncation warnings go through obs.FromContext over a context
	// with no attached logger, which falls back to slog.Default() read
	// fresh per call -- the module's established capture seam (see
	// worker_test.go). No test in this package runs in parallel.
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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})

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
	// dead-letter. Post-fix the attempt settles within milliseconds of the
	// handler returning; pre-fix the dead-letter UPDATE storing the
	// overlong error_message is refused with 22001 and the row stays
	// StatusRunning -- nothing else in-process ever touches it again
	// (retry-budget bookkeeping is precisely the write that cannot land),
	// so the bounded wait below fails with the row's running status and
	// the worker's captured 22001 log as the evidence of the wedge.
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
			var status jobs.Status
			if job != nil {
				status = job.Status
			}
			t.Fatalf("job never reached a terminal state on PostgreSQL within 15s (last status %q, last error %v): pre-fix this is the wedge -- the terminal-transition UPDATE was refused for the overlong error_message and only a process restart re-attempts, re-failing with the same overlong error. Captured worker log:\n%s",
				status, err, buf.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

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
