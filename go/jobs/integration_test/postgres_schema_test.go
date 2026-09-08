//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is go/jobs's PostgreSQL integration leg. The standalone
// deployment mode's schema (store.go's ensureJobsSchema) is created
// imperatively whenever a StandaloneQueue starts over a database, and a
// real PostgreSQL database is an ordinary backing store for that same
// standalone implementation (a single binary on real PostgreSQL is the
// ordinary small-customer production shape). The module's unit tier runs
// SQLite only; this leg runs the real schema DDL against a real PostgreSQL
// (dbtest.NewPostgres) and drives a real row through the real worker, so
// a dialect bug in the DDL fails here instead of in a customer's boot.
//
// The leg exists because of exactly such a bug: createJobsTableSQL
// declared the payload and result columns BLOB, which SQLite accepts and
// PostgreSQL rejects outright (PostgreSQL has no BLOB type -- the byte
// type is BYTEA), so a standalone-mode queue booted over real PostgreSQL
// failed its schema creation at Start with `type "blob" does not exist`.
// The DDL is now branched by dialect (BLOB on SQLite, BYTEA on
// PostgreSQL); this test pins the PostgreSQL half against a real server,
// the same coverage the two prior fixes of this column-type bug class
// (go/integration's webhook secret, go/org's invitation email) proved was
// the only thing that actually stops the recurrence.
func TestStandaloneQueue_PostgresSchema_BuildsAndRoundTrips(t *testing.T) {
	db := dbtest.NewPostgres(t)
	q := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(10*time.Millisecond),
		jobs.WithWorkerCount(1),
	)
	payload := []byte{0x00, 0xFF, 0x01, 0xFE, 0x7F, 'p', 'g'} // arbitrary bytes, not a UTF-8 string
	received := make(chan []byte, 1)
	if err := q.RegisterHandler(jobs.NewHandlerFunc("pg.roundtrip", func(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		received <- job.Payload
		return jobs.Result{Data: []byte("postgres-result-bytes")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	// Start applies ensureJobsSchema (CREATE TABLE jobs with BYTEA columns,
	// the partial idempotency index, the queue_writers table) against the
	// real PostgreSQL. Pre-fix, this is where the run fails: the
	// BLOB-typed CREATE TABLE is refused with `type "blob" does not exist`.
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("StandaloneQueue.Start over PostgreSQL error = %v; want nil -- the jobs DDL must build on the postgres dialect", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})

	tenant := pkgcore.TenantID("tenant-pg")
	id, err := q.Enqueue(context.Background(), jobs.Task{
		Type:           "pg.roundtrip",
		TenantID:       tenant,
		Payload:        payload,
		IdempotencyKey: "pg-roundtrip-1",
	}, jobs.WithMaxRetries(0))
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// Wait -- bounded, no wall-clock race on a healthy run -- for the real
	// worker to claim the row off PostgreSQL and settle it as Succeeded
	// with the result bytes persisted back through the BYTEA column.
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	deadline := time.Now().Add(30 * time.Second)
	for {
		job, err := q.Get(ctx, id)
		if err == nil && job.Status == jobs.StatusSucceeded {
			if string(job.Result.Data) != "postgres-result-bytes" {
				t.Fatalf("succeeded job result = %q, want the handler's bytes read back through the postgres result column", job.Result.Data)
			}
			break
		}
		if time.Now().After(deadline) {
			var status jobs.Status
			if job, gerr := q.Get(ctx, id); gerr == nil {
				status = job.Status
			}
			t.Fatalf("job never reached StatusSucceeded on PostgreSQL within 30s (last status %q, last error %v)", status, err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Fatalf("handler payload = %v, want %v -- the payload bytes must survive the postgres round trip", got, payload)
		}
	default:
		t.Fatal("handler never ran")
	}

	// The idempotent duplicate of the same key must collapse to the first
	// Job's id through the real partial unique index on PostgreSQL, the
	// same dedupe answer the SQLite tier pins.
	if dup, err := q.Enqueue(context.Background(), jobs.Task{
		Type:           "pg.roundtrip",
		TenantID:       tenant,
		Payload:        payload,
		IdempotencyKey: "pg-roundtrip-1",
	}); err != nil {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	} else if dup != id {
		t.Fatalf("duplicate Enqueue() = %s, want the first job's id %s (partial unique index on postgres)", dup, id)
	}
}
