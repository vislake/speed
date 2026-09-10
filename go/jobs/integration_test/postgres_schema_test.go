//go:build integration

package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/internal/testutil"
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
	// real PostgreSQL; a BLOB-typed CREATE TABLE is refused here with
	// `type "blob" does not exist`.
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

// TestStandaloneQueue_PostgresSchema_UpgradesLegacyTableWithoutTerminalPublishedAt
// is the PostgreSQL half of the terminal-column upgrade: against a real
// PostgreSQL carrying a jobs table created by a release predating
// terminal_published_at, Start's schema pass must add the column (probed
// through information_schema), backfill every already-terminal row inside
// the same transaction, leave non-terminal rows NULL, and then serve the
// terminal signal normally — a new transition publishes through a real
// bus while the historical rows are never republished. The SQLite tier
// pins the same semantics on the other dialect; this leg is what proves
// the PostgreSQL branch of the probe and the DDL transaction boundary
// actually run where the unit tier cannot reach.
func TestStandaloneQueue_PostgresSchema_UpgradesLegacyTableWithoutTerminalPublishedAt(t *testing.T) {
	db := dbtest.NewPostgres(t)
	// The pre-terminal_published_at shape on PostgreSQL: the current DDL
	// minus the new column, byte columns typed per this dialect.
	legacy := `CREATE TABLE jobs (
		id              VARCHAR(36) NOT NULL PRIMARY KEY,
		type            VARCHAR(255) NOT NULL,
		tenant_id       VARCHAR(64) NOT NULL,
		payload         BYTEA,
		idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
		status          VARCHAR(32) NOT NULL,
		claimed_by      VARCHAR(64) NOT NULL DEFAULT '',
		priority        INTEGER NOT NULL,
		progress_pct    INTEGER NOT NULL DEFAULT 0,
		progress_msg    VARCHAR(1000) NOT NULL DEFAULT '',
		result          BYTEA,
		error_message   VARCHAR(4000) NOT NULL DEFAULT '',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_retries     INTEGER NOT NULL,
		timeout_nanos   BIGINT NOT NULL,
		scheduled_at    TIMESTAMP NOT NULL,
		created_at      TIMESTAMP NOT NULL,
		updated_at      TIMESTAMP NOT NULL,
		started_at      TIMESTAMP,
		completed_at    TIMESTAMP
	)`
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy jobs table: %v", err)
	}
	now := time.Now()
	insert := func(id string, status jobs.Status, completedAt *time.Time) {
		t.Helper()
		if err := db.Exec(
			`INSERT INTO jobs (id, type, tenant_id, status, priority, max_retries, timeout_nanos, scheduled_at, created_at, updated_at, completed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "pg.legacy", "tenant-pg", string(status), int(jobs.PriorityNormal), jobs.DefaultMaxRetries, int64(jobs.DefaultTimeout),
			now, now, now, completedAt,
		).Error; err != nil {
			t.Fatalf("insert legacy row %q: %v", id, err)
		}
	}
	insert("pg-legacy-succeeded", jobs.StatusSucceeded, &now)
	insert("pg-legacy-pending", jobs.StatusPending, nil)

	bus := testutil.NewRecordingBus()
	q := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(10*time.Millisecond),
		jobs.WithEventBus(bus),
	)
	if err := q.RegisterHandler(jobs.NewHandlerFunc("pg.after-upgrade", func(context.Context, *jobs.Job, jobs.ProgressFn) (jobs.Result, error) {
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("StandaloneQueue.Start over a legacy PostgreSQL table error = %v; want nil -- the column add and backfill must run on the postgres dialect", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = q.Close(ctx)
	})

	type legacyRow struct {
		TerminalPublishedAt *time.Time `gorm:"column:terminal_published_at"`
		UpdatedAt           time.Time  `gorm:"column:updated_at"`
	}
	readRow := func(id string) legacyRow {
		t.Helper()
		var row legacyRow
		if err := db.Raw(`SELECT terminal_published_at, updated_at FROM jobs WHERE id = ?`, id).Scan(&row).Error; err != nil {
			t.Fatalf("read timestamps of %q: %v", id, err)
		}
		return row
	}
	succeeded := readRow("pg-legacy-succeeded")
	if succeeded.TerminalPublishedAt == nil {
		t.Fatal("historical terminal row came out unstamped on PostgreSQL; the column add must backfill it")
	}
	// The stamp must hold the column-add moment. It is compared against the
	// row's own updated_at -- written by this test moments earlier through
	// the same driver path -- rather than against a Go-side clock value:
	// PostgreSQL TIMESTAMP is zoneless, so the absolute label a read-back
	// carries is the driver's round-trip property (shared by every
	// timestamp column of this table), while the property this leg pins is
	// that the backfill stamped the change's own moment.
	if delta := succeeded.TerminalPublishedAt.Sub(succeeded.UpdatedAt); delta < -5*time.Second || delta > 5*time.Second {
		t.Errorf("historical row stamped at %v, want the column-add moment (the test wrote updated_at at %v)", succeeded.TerminalPublishedAt, succeeded.UpdatedAt)
	}
	if row := readRow("pg-legacy-pending"); row.TerminalPublishedAt != nil {
		t.Errorf("non-terminal row stamped at %v, want NULL", row.TerminalPublishedAt)
	}

	// The upgraded table serves the signal: nothing historical is
	// republished, and a new transition publishes through the real bus.
	// Several publish passes run in this window (the queue polls at 10ms).
	time.Sleep(50 * time.Millisecond)
	if got := len(bus.Events()); got != 0 {
		t.Fatalf("historical terminal rows were republished on PostgreSQL: %+v", bus.Events())
	}
	tenant := pkgcore.TenantID("tenant-pg")
	id, err := q.Enqueue(context.Background(), jobs.Task{Type: "pg.after-upgrade", TenantID: tenant})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	deadline := time.Now().Add(30 * time.Second)
	for {
		job, gerr := q.Get(ctx, id)
		if gerr == nil && job.Status == jobs.StatusSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never reached StatusSucceeded on the upgraded PostgreSQL table (last error %v)", gerr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	bus.WaitForPublish(t, 30*time.Second, "the new Job's terminal event", func(evt pkgcore.Event) bool {
		payload, ok := evt.Payload.(jobs.JobTerminalEvent)
		return ok && payload.JobID == id && payload.Status == jobs.StatusSucceeded
	})
}
