package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// jobsTable is the standalone deployment mode's persistence table name.
const jobsTable = "jobs"

// jobRecord is StandaloneQueue's persisted row shape. It deliberately does NOT
// implement dbkit.TenantScoped — no GetTenantID method, and no embedded
// dbkit.TenantModel, which would add one by promotion. Per
// docs/internal/04-data-and-tenancy.md's data-domain table and
// dbkit/AGENTS.md's "Known limitations", jobRecord is platform data, not
// tenant data: a worker's dispatch query scans eligible Jobs across every
// tenant at once, in priority order, to enforce per-tenant concurrency
// limits — an access pattern dbkit's tenant-scoping plugin and
// Repository[T] (which filter every query to exactly one tenant, resolved
// from ctx) cannot serve, since the plugin fails a query closed the
// instant its context carries no tenant, and Repository[T]'s generic
// constraint requires TenantScoped in the first place. tenant_id is still
// a real, indexed column here — it is what the per-tenant concurrency
// gate and Queue.Get/Cancel's tenant-match check read — it is simply never
// auto-injected as a query filter. See store_test.go's
// TestJobRecord_NotTenantScoped (tenancytest.AssertNotTenantScoped) for the
// standing proof of this property.
//
// This is queried through the plain *gorm.DB dbkit.Open returns, per
// dbkit/AGENTS.md's documented pattern for identity/platform data, never
// through dbkit.Repository[T] (whose generic constraint requires
// TenantScoped and could not compile against this type even by accident).
type jobRecord struct {
	ID             string `gorm:"column:id;primaryKey;size:36"`
	Type           string `gorm:"column:type;size:255;not null"`
	TenantID       string `gorm:"column:tenant_id;size:64;not null"`
	Payload        []byte `gorm:"column:payload"`
	IdempotencyKey string `gorm:"column:idempotency_key;size:255;not null"`
	Status         string `gorm:"column:status;size:32;not null"`
	Priority       int    `gorm:"column:priority;not null"`
	ProgressPct    int    `gorm:"column:progress_pct;not null"`
	ProgressMsg    string `gorm:"column:progress_msg;size:1000;not null"`
	Result         []byte `gorm:"column:result"`
	Error          string `gorm:"column:error_message;size:4000;not null"`
	Attempts       int    `gorm:"column:attempts;not null"`
	MaxRetries     int    `gorm:"column:max_retries;not null"`
	TimeoutNanos   int64  `gorm:"column:timeout_nanos;not null"`

	// ClaimedBy records which StandaloneQueue owns this row's current or most
	// recent claim: the queue's writer-registration owner token (see
	// queueWritersTable), written by claimOne under the claiming queue's own
	// token at the moment the row flips to StatusRunning. It is the ownership
	// marker resetInterruptedRecords' WHERE clause is scoped by, and the
	// operator-visible record of which process (re)start left a row running
	// after a crash. Empty for rows that never were claimed (and for rows
	// written before the column existed).
	ClaimedBy string `gorm:"column:claimed_by;size:64;not null"`

	ScheduledAt time.Time  `gorm:"column:scheduled_at;not null"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
	StartedAt   *time.Time `gorm:"column:started_at"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName pins jobRecord to jobsTable, so it does not depend on GORM's
// pluralization of the (unexported) type name.
func (jobRecord) TableName() string { return jobsTable }

// The jobs table is executed imperatively, with a plain CREATE TABLE IF
// NOT EXISTS, the same bootstrapping pattern dbkit.MigrationRegistry
// itself uses for its own schema_migrations table (see
// go/dbkit/migrations.go's createSchemaMigrationsTableSQL) — not for the
// same chicken-and-egg reason (this table has no bootstrapping problem),
// but because this table is an implementation detail specific to the
// standalone deployment mode, with no other consumer: the distributed
// deployment mode's Queue implementation (asynq.Queue) is Redis/asynq-backed
// and never creates this table at all, so
// routing it through dbkit.MigrationRegistry's cross-module,
// Atlas-generated, versioned migration machinery — built for schema that
// ships and evolves across both deployment modes — would be
// disproportionate.
//
// Unlike the module's remaining statements, this one is NOT written as a
// single string portable across both dbkit dialects, and must never be:
// the payload and result columns hold arbitrary bytes, whose column type
// differs between the two dialects — BLOB on SQLite, BYTEA on PostgreSQL,
// which has no BLOB type at all (a BLOB-typed CREATE TABLE fails on
// PostgreSQL with `type "blob" does not exist`, the same column-type bug
// class go/integration's webhook-secret and go/org's invitation-email
// fixes already established). ensureJobsSchema therefore selects the
// dialect's own statement at Start time (createJobsTableSQL, below), the
// same db.Name() branch ensureJobsClaimedByColumn already uses. Every
// other type in the two statements — VARCHAR/INTEGER/BIGINT/TIMESTAMP —
// is genuinely identical on both dialects, and every column except the
// two byte columns is spelled exactly once in the shared shape of both
// statements so they cannot drift apart. The claim that this schema runs
// on both dialects is proven, not assumed, by the module's PostgreSQL
// integration leg (integration_test/postgres_schema_test.go), which boots
// a real StandaloneQueue over a real PostgreSQL: the earlier per-column
// bug survived precisely because SQLite-only coverage never executes the
// DDL on the dialect that rejects it.
func createJobsTableSQL(dbName string) string {
	if dbName == "sqlite" {
		return createJobsTableSQLSQLite
	}
	return createJobsTableSQLPostgres
}

const createJobsTableSQLSQLite = `CREATE TABLE IF NOT EXISTS ` + jobsTable + ` (
	id              VARCHAR(36) NOT NULL PRIMARY KEY,
	type            VARCHAR(255) NOT NULL,
	tenant_id       VARCHAR(64) NOT NULL,
	payload         BLOB,
	idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
	status          VARCHAR(32) NOT NULL,
	claimed_by      VARCHAR(64) NOT NULL DEFAULT '',
	priority        INTEGER NOT NULL,
	progress_pct    INTEGER NOT NULL DEFAULT 0,
	progress_msg    VARCHAR(1000) NOT NULL DEFAULT '',
	result          BLOB,
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

// createJobsTableSQLPostgres is the PostgreSQL spelling of
// createJobsTableSQLSQLite: identical except for the two byte columns,
// which PostgreSQL types BYTEA (BLOB does not exist there). See
// createJobsTableSQL's own doc comment for why the DDL is dialect-branched
// rather than single-statement.
const createJobsTableSQLPostgres = `CREATE TABLE IF NOT EXISTS ` + jobsTable + ` (
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

// progressMsgColumnRunes and errorMessageColumnRunes are the character
// widths of the jobs table's progress_msg and error_message columns: the
// 1000 and 4000 in the VARCHAR(1000)/VARCHAR(4000) both
// createJobsTableSQL statements spell (the two statements share every
// non-byte column's spelling, per createJobsTableSQL's own doc comment).
// PostgreSQL enforces the width by character count -- an overlong value
// makes the UPDATE fail with 22001 -- while SQLite never enforces a
// VARCHAR length at all. These constants are the single source of truth
// the write-side cut (fitDescriptiveText, below) reads, declared next to
// the DDL so the two cannot drift apart: changing a column's width means
// changing the DDL and this constant together, and the module's
// PostgreSQL integration leg
// (integration_test/postgres_overlong_message_test.go) fails any drift
// that makes the cut wider than the column (the 22001 recurs) -- the
// same shared-constant-next-to-the-declaration pattern dbkit/audit's
// model.go documents for its own column rune counts.
const (
	// progressMsgColumnRunes bounds progress_msg (a cut column).
	progressMsgColumnRunes = 1000
	// errorMessageColumnRunes bounds error_message (a cut column).
	errorMessageColumnRunes = 4000
)

// createJobsDispatchIndexSQL supports the dispatcher's claim query (status
// + scheduled_at, ordered by priority) without a full table scan.
const createJobsDispatchIndexSQL = `CREATE INDEX IF NOT EXISTS idx_jobs_dispatch
	ON ` + jobsTable + ` (status, scheduled_at, priority)`

// createJobsIdempotencySQL is a PARTIAL unique index: it applies only to
// rows with a non-empty idempotency_key, so the overwhelming majority of
// Jobs (which have none) never collide with one another. Both dbkit
// dialects support a WHERE clause on CREATE UNIQUE INDEX — this is not a
// PostgreSQL-only feature. insertRecord relies on this index (via
// gorm.ErrDuplicatedKey, which dbkit.Open's TranslateError:true makes
// driver-agnostic) to make Enqueue's idempotency check race-safe against
// two concurrent Enqueue calls for the same key, rather than a
// check-then-insert race.
const createJobsIdempotencySQL = `CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_tenant_idempotency
	ON ` + jobsTable + ` (tenant_id, idempotency_key) WHERE idempotency_key != ''`

// queueWritersTable is the single-row table through which StandaloneQueue
// enforces "one live writer per jobs table". StandaloneQueue.Start registers
// this queue under its owner token (acquireWriterRegistration) and refuses to
// start -- ErrQueueWriterActive -- while that row belongs to a live sibling;
// a dedicated heartbeat goroutine (worker.go's runWriterHeartbeat, decoupled
// from the dispatcher so the registration stays fresh while a worker holds a
// row -- including through the dispatcher's blocked handoff and through
// Close's drain of in-flight Handles) refreshes it once per poll interval;
// Close releases the row. A crashed process leaves the row behind, and the
// next Start steals it once its registration goes stale (see
// StandaloneQueue.Start, acquireWriterRegistration and stale_at) -- the same
// crash-recovery shape the jobs table's own StatusRunning rows get from
// resetInterruptedRecords, applied to the writer itself.
//
// stale_at is the row's stale moment: the timestamp past which the row's
// owner may be presumed crashed and the registration stolen by the next
// Start. The owner itself authors it -- registration time plus its own
// stale window (writerStaleAfter, computed from its OWN poll interval) at
// acquire, refreshed the same way at every heartbeat -- so a taking Start
// judges the incumbent's liveness by a number the INCUMBENT wrote, never by
// a number derived from the taker's own configuration: two queues
// configured with different poll intervals can no longer mistake each
// other's live registrations for crashed ones (see AGENTS.md's Known
// limitations). A row whose stale_at is NULL was written by a release that
// predates the column; such a row's owner cadence is unknowable, so
// acquireWriterRegistration judges it by a deliberately conservative fixed
// window over last_heartbeat (legacyRegistrationStaleAfter) instead.
//
// The table is bootstrapped alongside the jobs table (CREATE TABLE IF NOT
// EXISTS, exactly like createJobsTableSQL) for the same reason: it is an
// implementation detail of the standalone deployment mode with no other
// consumer. Row id is pinned to 1 by convention; the primary key plus the
// id=1 upsert below are what keep the table at exactly one row.
const queueWritersTable = "queue_writers"

const createQueueWritersTableSQL = `CREATE TABLE IF NOT EXISTS ` + queueWritersTable + ` (
	id            INTEGER NOT NULL PRIMARY KEY,
	owner         VARCHAR(64) NOT NULL,
	last_heartbeat TIMESTAMP NOT NULL,
	stale_at       TIMESTAMP
)`

// ErrQueueWriterActive is returned by StandaloneQueue.Start when another
// live StandaloneQueue already holds the queue_writers registration for the
// same database — a second writer on one jobs table would reset and
// re-claim the first writer's mid-Handle rows, double-executing them (see
// resetInterruptedRecords' own doc comment for the full chain). It is a
// conflict with the database's current writer, not a caller error: the
// host should either shut the other queue down (its Close releases the
// registration) or retry Start once the incumbent's registration goes
// stale.
var ErrQueueWriterActive = apperr.Conflict("jobs.queue_writer_active")

// acquireWriterRegistrationSQL is the atomic single-writer gate: it inserts
// this queue's registration, or -- when the row already exists -- replaces
// it only if the incumbent's stale moment has passed (stale_at < now; the
// incumbent is presumed crashed). The judgment datum is the stale_at the
// INCUMBENT itself wrote -- its own heartbeat cadence converted by its own
// configuration, refreshed at every one of its beats -- never a window
// derived from the acquiring queue's own poll interval, so an incumbent
// beating at a slower cadence than the taker's own stale window is judged
// by its own numbers and never looks crashed while it beats. One statement,
// so two queues racing to Start on the same database serialize on the
// row's own primary key instead of both reading "absent" and both
// proceeding: exactly one of them sees RowsAffected == 1, the other sees
// the incumbent's fresh registration and is refused. A row whose stale_at
// is NULL (written by a release predating the column) never satisfies
// stale_at < now, so this statement refuses it too -- see
// legacyTakeoverWriterRegistrationSQL for how such a row is judged.
// Portable across both dbkit dialects (SQLite 3.24+'s and PostgreSQL's
// identical UPSERT syntax).
const acquireWriterRegistrationSQL = `INSERT INTO ` + queueWritersTable + ` (id, owner, last_heartbeat, stale_at) VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET owner = excluded.owner, last_heartbeat = excluded.last_heartbeat, stale_at = excluded.stale_at
WHERE ` + queueWritersTable + `.stale_at < ?`

// legacyRegistrationStaleAfter is the fixed window acquireWriterRegistration
// applies to a registration row whose stale_at is NULL -- one written by a
// release that predated the stale_at column. Such a row's owner cadence is
// unknowable (the row records no cadence and its writer's code is gone), so
// no taker-side conversion could judge it correctly; the window is chosen
// deliberately conservative -- an order of magnitude beyond any poll
// interval a job dispatcher is plausibly configured with -- so a live
// pre-column writer is never mistaken for a crashed one during the only
// window in which such a row can exist (a rolling upgrade across the
// column's introduction), at the cost of delaying the takeover of a
// genuinely crashed pre-column writer's row by at most this window, once.
const legacyRegistrationStaleAfter = 60 * time.Second

// legacyTakeoverWriterRegistrationSQL takes over a registration row whose
// stale_at is NULL (written by a release predating the column): it replaces
// the row only when its last_heartbeat is older than
// legacyRegistrationStaleAfter. The stale_at IS NULL condition doubles as
// the race guard: a rival new-code taker that converted the same legacy
// row between this queue's failed acquireWriterRegistrationSQL attempt and
// this statement leaves the statement at zero rows -- that rival now owns a
// fresh registration with a real stale_at, which is exactly what this
// queue's retry of the main gate would judge.
const legacyTakeoverWriterRegistrationSQL = `UPDATE ` + queueWritersTable + ` SET owner = ?, last_heartbeat = ?, stale_at = ?
WHERE id = 1 AND stale_at IS NULL AND last_heartbeat < ?`

// acquireWriterRegistration claims the queue_writers row for owner (this
// queue's writer token, NewStandaloneQueue) at now, or reports
// ErrQueueWriterActive when the row already belongs to a live writer.
// staleAfter is the ACQUIRING queue's own stale window: it authors the
// stale moment this queue's own registration carries (stale_at = now +
// staleAfter, the same arithmetic the heartbeat refreshes), and it plays
// NO part in judging an incumbent -- the incumbent is judged by the stale
// moment its own registration carries (see acquireWriterRegistrationSQL),
// or, for a row predating the stale_at column, by
// legacyRegistrationStaleAfter. Passing zero therefore makes this queue's
// OWN registration stale the moment it lands (what the unit tests use to
// leave a row immediately stealable), never the incumbent's.
func acquireWriterRegistration(ctx context.Context, db *gorm.DB, owner string, now time.Time, staleAfter time.Duration) error {
	staleAt := now.Add(staleAfter)
	res := db.WithContext(ctx).Exec(acquireWriterRegistrationSQL, owner, now, staleAt, now)
	if res.Error != nil {
		return fmt.Errorf("jobs: acquire writer registration: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	// Zero rows from the main gate: either the incumbent's own stale moment
	// has not passed (a live sibling -- refused), or the row predates the
	// stale_at column and its NULL stale moment never satisfies stale_at <
	// now. The legacy takeover distinguishes the two: it is conditional on
	// the row's stale_at still being NULL AND its last_heartbeat older than
	// the conservative legacy window. Zero rows there means a live sibling
	// (a live pre-column writer still beating, or a rival new-code taker
	// that converted the row in the instant between the two statements) --
	// see ErrQueueWriterActive.
	res = db.WithContext(ctx).Exec(legacyTakeoverWriterRegistrationSQL, owner, now, staleAt, now.Add(-legacyRegistrationStaleAfter))
	if res.Error != nil {
		return fmt.Errorf("jobs: acquire writer registration: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}
	return ErrQueueWriterActive
}

// heartbeatWriterRegistration refreshes owner's registration row -- its
// last_heartbeat and its stale moment (stale_at = now + staleAfter, the
// owner's own declared stale window, mirroring acquire's authorship) -- and
// reports whether the row was still there to refresh. A false report means
// this queue's registration was stolen (its beats lapsed past its own stale
// moment and a sibling took over, or the row was removed) -- the dispatcher
// logs it and keeps running, since stopping mid-flight would strand claimed
// rows; the anomaly is operator-visible either way.
func heartbeatWriterRegistration(ctx context.Context, db *gorm.DB, owner string, now time.Time, staleAfter time.Duration) (bool, error) {
	res := db.WithContext(ctx).Exec(`UPDATE `+queueWritersTable+` SET last_heartbeat = ?, stale_at = ? WHERE id = 1 AND owner = ?`, now, now.Add(staleAfter), owner)
	if res.Error != nil {
		return false, fmt.Errorf("jobs: writer heartbeat: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// releaseWriterRegistration removes owner's registration row -- the graceful
// handover StandaloneQueue.Close performs once every worker has stopped, so
// a successor queue on the same database may Start immediately rather than
// waiting out the stale window. Removing another owner's row is impossible
// (the WHERE names owner); a row already stolen or never acquired leaves the
// delete at zero rows, which is success, not an error.
func releaseWriterRegistration(ctx context.Context, db *gorm.DB, owner string) error {
	if err := db.WithContext(ctx).Exec(`DELETE FROM `+queueWritersTable+` WHERE id = 1 AND owner = ?`, owner).Error; err != nil {
		return fmt.Errorf("jobs: release writer registration: %w", err)
	}
	return nil
}

// newWriterOwner generates this StandaloneQueue's writer-registration token:
// a fresh id per queue object, so a restarted process claims rows under a
// token no previous run ever used (see resetInterruptedRecords' doc comment
// for why the reset's own WHERE needs exactly that property).
func newWriterOwner() string { return uuid.NewString() }

// ensureJobsSchema creates the jobs table and its indexes if they do not
// already exist, adds the claimed_by column to a jobs table a pre-fix
// release created without it, creates the queue_writers single-writer
// table, and adds the stale_at column to a queue_writers table a release
// predating it created (ensureQueueWritersStaleAtColumn). Safe to call
// every time Start runs. The CREATE TABLE statement is chosen by dialect
// (createJobsTableSQL): the byte columns' type differs between SQLite
// (BLOB) and PostgreSQL (BYTEA), so there is no single-statement spelling
// of the table.
func ensureJobsSchema(ctx context.Context, db *gorm.DB) error {
	for _, stmt := range [...]string{createJobsTableSQL(db.Name()), createJobsDispatchIndexSQL, createJobsIdempotencySQL} {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return fmt.Errorf("jobs: ensure schema: %w", err)
		}
	}
	if err := ensureJobsClaimedByColumn(ctx, db); err != nil {
		return err
	}
	if err := db.WithContext(ctx).Exec(createQueueWritersTableSQL).Error; err != nil {
		return fmt.Errorf("jobs: ensure schema: %w", err)
	}
	if err := ensureQueueWritersStaleAtColumn(ctx, db); err != nil {
		return err
	}
	return nil
}

// ensureQueueWritersStaleAtColumn brings a queue_writers table created by a
// release that predated the stale_at column (see createQueueWritersTableSQL
// and acquireWriterRegistration) up to the current shape, portably across
// both dbkit dialects, with the identical technique
// ensureJobsClaimedByColumn uses for the jobs table: PostgreSQL supports
// ADD COLUMN IF NOT EXISTS natively; SQLite does not, so the sqlite path
// probes PRAGMA table_info first and alters only when the column is
// absent. A table freshly created from createQueueWritersTableSQL already
// carries the column and the probe answers "present", skipping the alter.
// Existing rows keep NULL stale_at -- the marker acquireWriterRegistration
// reads as "written by a pre-column release" -- which is exactly why the
// column is added nullable rather than with a constant default: no constant
// could truthfully say when a pre-existing row's owner goes stale.
func ensureQueueWritersStaleAtColumn(ctx context.Context, db *gorm.DB) error {
	if db.Name() != "sqlite" {
		// postgres (the only other dbkit dialect): native IF NOT EXISTS.
		if err := db.WithContext(ctx).Exec(`ALTER TABLE ` + queueWritersTable + ` ADD COLUMN IF NOT EXISTS stale_at TIMESTAMP`).Error; err != nil {
			return fmt.Errorf("jobs: add queue_writers stale_at column: %w", err)
		}
		return nil
	}
	var columns []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.WithContext(ctx).Raw(`PRAGMA table_info(` + queueWritersTable + `)`).Scan(&columns).Error; err != nil {
		return fmt.Errorf("jobs: probe queue_writers table columns: %w", err)
	}
	for _, c := range columns {
		if c.Name == "stale_at" {
			return nil
		}
	}
	if err := db.WithContext(ctx).Exec(`ALTER TABLE ` + queueWritersTable + ` ADD COLUMN stale_at TIMESTAMP`).Error; err != nil {
		return fmt.Errorf("jobs: add queue_writers stale_at column: %w", err)
	}
	return nil
}

// ensureJobsClaimedByColumn brings a jobs table created by a release that
// predated the claimed_by ownership column (see jobRecord.ClaimedBy) up to
// the current shape, portably across both dbkit dialects: PostgreSQL
// supports ADD COLUMN IF NOT EXISTS natively; SQLite does not, so the sqlite
// path probes PRAGMA table_info first and alters only when the column is
// absent. A table freshly created from createJobsTableSQL already carries
// the column and the probe answers "present", skipping the alter. The jobs
// table is bootstrapped with CREATE TABLE IF NOT EXISTS rather than through
// dbkit.MigrationRegistry (see createJobsTableSQL's own doc comment), so
// this in-place column add is the migration story for exactly the one shape
// that bootstrapping cannot evolve: a table that already exists.
func ensureJobsClaimedByColumn(ctx context.Context, db *gorm.DB) error {
	if db.Name() != "sqlite" {
		// postgres (the only other dbkit dialect): native IF NOT EXISTS.
		if err := db.WithContext(ctx).Exec(`ALTER TABLE ` + jobsTable + ` ADD COLUMN IF NOT EXISTS claimed_by VARCHAR(64) NOT NULL DEFAULT ''`).Error; err != nil {
			return fmt.Errorf("jobs: add claimed_by column: %w", err)
		}
		return nil
	}
	var columns []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.WithContext(ctx).Raw(`PRAGMA table_info(` + jobsTable + `)`).Scan(&columns).Error; err != nil {
		return fmt.Errorf("jobs: probe jobs table columns: %w", err)
	}
	for _, c := range columns {
		if c.Name == "claimed_by" {
			return nil
		}
	}
	if err := db.WithContext(ctx).Exec(`ALTER TABLE ` + jobsTable + ` ADD COLUMN claimed_by VARCHAR(64) NOT NULL DEFAULT ''`).Error; err != nil {
		return fmt.Errorf("jobs: add claimed_by column: %w", err)
	}
	return nil
}

// newJobID generates a new, application-side JobID (backend coding
// standard §5: ids are generated in the application, never
// gen_random_uuid()).
func newJobID() string { return uuid.NewString() }

// newRecord builds the jobRecord Enqueue inserts for task, resolved.
func newRecord(id string, task Task, resolved ResolvedEnqueueOptions, now time.Time) *jobRecord {
	return &jobRecord{
		ID:             id,
		Type:           task.Type,
		TenantID:       string(task.TenantID),
		Payload:        task.Payload,
		IdempotencyKey: task.IdempotencyKey,
		Status:         string(StatusPending),
		Priority:       int(resolved.Priority),
		MaxRetries:     resolved.MaxRetries,
		TimeoutNanos:   int64(resolved.Timeout),
		ScheduledAt:    resolved.ScheduledAt,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// toJob converts a persisted record to the public Job shape.
func toJob(rec *jobRecord) *Job {
	job := &Job{
		ID:             JobID(rec.ID),
		Type:           rec.Type,
		TenantID:       pkgcore.TenantID(rec.TenantID),
		Payload:        rec.Payload,
		IdempotencyKey: rec.IdempotencyKey,
		Status:         Status(rec.Status),
		Priority:       Priority(rec.Priority),
		ProgressPct:    rec.ProgressPct,
		ProgressMsg:    rec.ProgressMsg,
		Error:          rec.Error,
		Attempts:       rec.Attempts,
		MaxRetries:     rec.MaxRetries,
		ScheduledAt:    rec.ScheduledAt,
		CreatedAt:      rec.CreatedAt,
		UpdatedAt:      rec.UpdatedAt,
		StartedAt:      rec.StartedAt,
		CompletedAt:    rec.CompletedAt,
	}
	if rec.Status == string(StatusSucceeded) {
		job.Result = &Result{Data: rec.Result}
	}
	return job
}

// insertRecord creates rec, or — when rec.IdempotencyKey is non-empty and
// already used by an existing Job for the same tenant — leaves the
// database unchanged and returns that existing Job's id instead. See
// createJobsIdempotencySQL's own doc comment for why this is race-safe
// against a concurrent duplicate Enqueue rather than a check-then-insert
// race.
func insertRecord(ctx context.Context, db *gorm.DB, rec *jobRecord) (string, error) {
	err := db.WithContext(ctx).Create(rec).Error
	if err == nil {
		return rec.ID, nil
	}
	if rec.IdempotencyKey == "" || !errors.Is(err, gorm.ErrDuplicatedKey) {
		return "", fmt.Errorf("jobs: insert job: %w", err)
	}

	var existing jobRecord
	findErr := db.WithContext(ctx).
		Where("tenant_id = ? AND idempotency_key = ?", rec.TenantID, rec.IdempotencyKey).
		First(&existing).Error
	if findErr != nil {
		return "", fmt.Errorf("jobs: look up existing job for idempotency key after conflict: %w", findErr)
	}
	return existing.ID, nil
}

// findByID returns the record for id, or ErrJobNotFound when none exists.
// It performs no tenant check of its own — callers (Get, Cancel) apply
// their own access decision on the returned record's TenantID.
func findByID(ctx context.Context, db *gorm.DB, id JobID) (*jobRecord, error) {
	var rec jobRecord
	err := db.WithContext(ctx).Where("id = ?", string(id)).First(&rec).Error
	switch {
	case err == nil:
		return &rec, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrJobNotFound
	default:
		return nil, fmt.Errorf("jobs: find job: %w", err)
	}
}

// claimCandidatesSQL selects up to limit Jobs eligible to run (StatusPending
// or StatusRetrying, ScheduledAt <= now), interleaved round-robin across
// distinct tenants, with Priority ordering the rows within any one tenant's
// own share -- see claimCandidates' own doc comment for why this shape
// exists at all. The inner query ranks each tenant's own eligible
// rows independently (ROW_NUMBER() OVER (PARTITION BY tenant_id ORDER BY
// priority DESC, scheduled_at ASC)); the outer query then orders by that
// rank FIRST, so every distinct tenant present contributes its own
// rank-1 (highest-priority, oldest) row before ANY tenant contributes a
// second one, its rank-2 before any third, and so on -- exactly the
// "round-robin, then Priority within a tenant's own share" fairness
// dispatchOnce's own comment already claimed for the whole dispatch tick,
// now actually true at the SELECT itself rather than only at the
// concurrency-admission step downstream of it. Priority does not order
// across tenants: within one wave position (equal tenant_rank), the outer
// query's two remaining keys -- priority DESC, then scheduled_at ASC --
// decide which tenant's head-of-line row leads, but a tenant's rank-2 row
// never overtakes another tenant's rank-1 one whatever the two priorities
// (pinned by store_test.go's
// TestClaimCandidates_FairShareRotationAcrossTenants_PriorityWithinTenantShare).
// ROW_NUMBER() OVER (...) is
// standard SQL, supported identically by both dbkit dialects (SQLite 3.25+
// and PostgreSQL) — the "portable across both dialects" discipline
// createJobsTableSQL's own doc comment already applies to this table's
// schema applies here too, even though only SQLite is exercised in the
// standalone deployment mode today.
const claimCandidatesSQL = `
	SELECT id, type, tenant_id, payload, idempotency_key, status, priority,
	       progress_pct, progress_msg, result, error_message, attempts,
	       max_retries, timeout_nanos, scheduled_at, created_at, updated_at,
	       started_at, completed_at
	FROM (
		SELECT *,
		       ROW_NUMBER() OVER (
		           PARTITION BY tenant_id
		           ORDER BY priority DESC, scheduled_at ASC
		       ) AS tenant_rank
		FROM ` + jobsTable + `
		WHERE status IN (?, ?) AND scheduled_at <= ?
	) ranked
	ORDER BY tenant_rank ASC, priority DESC, scheduled_at ASC
	LIMIT ?
`

// claimCandidates returns up to limit Jobs eligible to run, interleaved
// round-robin across distinct tenants — see claimCandidatesSQL's own doc
// comment for the exact ordering and why it exists: without it, a single
// tenant with a backlog at or beyond limit truly-eligible rows, all older
// than every other tenant's own eligible rows, would fill the ENTIRE
// candidate window every tick, so a different tenant's eligible-and-older
// row would never be selected at all — not merely delayed — for as long as
// the flooding tenant's backlog stays at or above limit. This was a real,
// reproduced gap (see candidate_window_fairness_test.go's
// TestDispatchOnce_CandidateWindowDoesNotStarveOtherTenants, which fails
// against the naive "ORDER BY priority DESC, scheduled_at ASC LIMIT limit"
// query this replaced), distinct from — and previously masked by
// proximity to — the per-tenant CONCURRENCY-admission fairness
// TestPerTenantConcurrencyLimiting already proved: that test only ever
// exercises backlogs far smaller than claimBatchSize, so it could never
// have caught a candidate-SELECTION starvation gap this shallow.
func claimCandidates(ctx context.Context, db *gorm.DB, now time.Time, limit int) ([]jobRecord, error) {
	var recs []jobRecord
	err := db.WithContext(ctx).
		Raw(claimCandidatesSQL, string(StatusPending), string(StatusRetrying), now, limit).
		Scan(&recs).Error
	if err != nil {
		return nil, fmt.Errorf("jobs: query claim candidates: %w", err)
	}
	return recs, nil
}

// claimOne atomically transitions rec (identified by rec.ID and the status
// it was read with) to StatusRunning under owner's writer token, stamping
// the row's ClaimedBy marker. It reports false, with no error, when another
// operation already moved the row out of that status first — the
// observed-status guard in the WHERE clause turns a lost race into a no-op
// instead of a double execution, which matters if the standalone deployment
// mode's implementation is ever driven by more than the one dispatcher
// goroutine it ships with today.
//
// claimOne is deliberately only the FIRST half of a claim: it flips the
// status and records ownership but does not count the attempt. Counting
// (Attempts) and the attempt's StartedAt happen at the worker handoff,
// markAttemptStarted — the moment a worker actually takes possession of the
// row — so a Job that sits claimed-but-not-started (dispatcher claimed it,
// the worker has not received it yet) honestly reports through Get() as
// StatusRunning with Attempts not yet including an attempt no Handle has
// started, and an unclean exit in that window never consumes an attempt
// that never ran.
func claimOne(ctx context.Context, db *gorm.DB, rec jobRecord, now time.Time, owner string) (bool, error) {
	result := db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", rec.ID, rec.Status).
		Updates(map[string]any{
			"status":     string(StatusRunning),
			"claimed_by": owner,
			"updated_at": now,
		})
	if result.Error != nil {
		return false, fmt.Errorf("jobs: claim job: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// markAttemptStarted is the worker-handoff half of the two-phase claim (see
// claimOne's own doc comment): it records that the attempt a worker is about
// to execute has actually started, incrementing Attempts and stamping
// StartedAt on a row the dispatcher already claimed (StatusRunning). The
// WHERE ... status = 'running' guard keeps the write a no-op for a row a
// concurrent Cancel settled between the claim and the handoff — the attempt
// still executes (Cancel's contract lets a claimed Job run), but its count
// and start time are discarded along with its outcome, exactly like
// completeSucceeded/completeRetrying/completeDeadLetter discard the outcome
// itself. Called by the worker; execute's own in-memory rec.Attempts is
// authoritative for this attempt's retry/dead-letter arithmetic either way,
// so a failed write is logged and the attempt proceeds.
func markAttemptStarted(ctx context.Context, db *gorm.DB, id string, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", id, string(StatusRunning)).
		Updates(map[string]any{
			"attempts":   gorm.Expr("attempts + 1"),
			"started_at": now,
			"updated_at": now,
		}).Error
}

// fitColumnValue renders v storable in a column of at most maxRunes
// characters, mirroring the same-shaped helper go/dbkit/audit ships for
// its own descriptive columns (repository.go's fitColumnValue -- that one
// is unexported and this module does not depend on dbkit, so the cut is
// copied, not imported). cut reports whether the value had to be
// shortened; a value that only needed invalid-UTF-8 sanitization reports
// cut=false, so the caller can say which change happened (the warning's
// reason attribute). Invalid UTF-8 runs are sanitized to the Unicode
// replacement character (one per consecutive run, so two arbitrary byte
// runs never concatenate into a different valid value), then the value is
// cut at maxRunes runes when it is longer -- never at maxRunes bytes,
// which could split a multi-byte character and store garbage a UTF-8
// PostgreSQL would refuse with 22021. A value that is already valid UTF-8
// and within the bound is returned unchanged.
func fitColumnValue(v string, maxRunes int) (fitted string, cut bool) {
	if len(v) <= maxRunes && utf8.ValidString(v) {
		return v, false
	}
	runes := []rune(strings.ToValidUTF8(v, "\uFFFD"))
	cut = len(runes) > maxRunes
	if cut {
		runes = runes[:maxRunes]
	}
	return string(runes), cut
}

// fitDescriptiveText is the write-side choke point every persistence of a
// descriptive message on the jobs table passes through (updateProgress's
// progress_msg below, completeRetrying's and completeDeadLetter's
// error_message): it renders v fit for descriptive column columnName,
// whose declared width is maxRunes, and warns -- never refuses -- when
// the value had to be changed. error_message and progress_msg are
// DESCRIPTIVE fields, so an overlong value is cut to its column's width,
// applying the dbkit/audit fitEventToColumns division of labour in the
// cut direction: the identifier-class fields there refuse, the
// descriptive ones cut, and a refusal is exactly the failure this
// function exists to make impossible. On PostgreSQL an overlong value
// makes the terminal-transition UPDATE fail with 22001 and the task
// stays StatusRunning -- the retry-budget write (settleFailedAttempt)
// that could advance the state machine is precisely the write that
// cannot land, and the only recovery, the next Start's
// resetInterruptedRecords, re-runs the handler, which fails with the
// same overlong error, whose write is refused again: the task never
// reaches a terminal state until the code stops refusing.
//
// Every change is recorded in a structured warning through
// obs.FromContext(ctx) -- jobs sits above go/observability, unlike
// dbkit/audit, which falls back to slog.Default for the same reason its
// own warning does -- naming the job, the column, the value's original
// character count, the column's limit and which change was made
// (column_width or invalid_utf8), so a truncation -- an integrity loss
// that would otherwise be unrecordable once the database had stored the
// cut silently -- leaves an operator-recoverable trace.
func fitDescriptiveText(ctx context.Context, jobID, column string, v string, maxRunes int) string {
	fitted, cut := fitColumnValue(v, maxRunes)
	if fitted == v {
		return fitted
	}
	reason := "invalid_utf8"
	if cut {
		reason = "column_width"
	}
	obs.FromContext(ctx).Warn("jobs: message changed to fit its column",
		"job_id", jobID,
		"column", column,
		"original_chars", utf8.RuneCountInString(v),
		"limit_chars", maxRunes,
		"reason", reason)
	return fitted
}

// updateProgress persists one progress report. The WHERE ... status =
// 'running' AND claimed_by = owner guard is the same one every sibling
// execution write in this file carries
// (completeSucceeded/completeRetrying/completeDeadLetter): a progress
// report only ever legitimately comes from a running attempt of the writer
// that claimed the row (worker.go's execute passes its progress closure),
// so a write that lands after a concurrent Cancel (or any other transition)
// already moved the row out of StatusRunning must no-op rather than
// overwrite the terminal row's progress fields and push its updated_at -- a
// stale "I am working" stamp on a Job that is visibly cancelled or finished
// -- and a write from an attempt whose row another writer took over must
// not land on the new owner's row either. updateProgress reports no
// RowsAffected to its caller, exactly as before: the no-op is the write's
// answer, not an error.
func updateProgress(ctx context.Context, db *gorm.DB, owner string, id string, pct int, msg string) error {
	msg = fitDescriptiveText(ctx, id, "progress_msg", msg, progressMsgColumnRunes)
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", id, string(StatusRunning), owner).
		Updates(map[string]any{
			"progress_pct": pct,
			"progress_msg": msg,
			"updated_at":   time.Now(),
		}).Error
}

// completeSucceeded conditionally transitions id from StatusRunning to
// StatusSucceeded and reports whether the transition actually happened.
// Like claimOne, the WHERE ... status = 'running' guard turns a concurrent
// Cancel's markCancelled into the winner of the race (RowsAffected == 0,
// nil error) rather than letting this write overwrite StatusCancelled —
// see Queue.Cancel's own doc comment. The claimed_by = owner tail of the
// guard is the ownership half: an attempt's outcome write may only settle a
// row that attempt's own writer claimed (claimOne stamped owner at the
// claim), so an attempt whose row another writer took over after a
// writer-gate lapse no-ops here instead of marking the sibling's
// still-running row succeeded with this attempt's result — the no-op is
// then probed and logged as the double-execution anomaly it is (worker.go's
// logDiscardedOutcome), never merged with the concurrent-cancel case. The
// returned bool is what lets execute (worker.go) record the success log
// line and the success metrics strictly after a genuine running -> succeeded
// transition, mirroring completeDeadLetter's transition report: a record
// emitted ahead of the write would survive a no-op write and show a
// cancelled Job as succeeded.
func completeSucceeded(ctx context.Context, db *gorm.DB, owner string, id string, result Result, now time.Time) (bool, error) {
	res := db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", id, string(StatusRunning), owner).
		Updates(map[string]any{
			"status": string(StatusSucceeded),
			"result": result.Data,
			// error_message is cleared here: Job.Error's own doc comment
			// promises it is empty once Status is StatusSucceeded, but a
			// Job that failed one or more earlier attempts before this one
			// succeeded (StatusRetrying) already has a stale message in
			// this column from completeRetrying -- left alone, a clean
			// eventual success would still report its next-to-last
			// failure's message forever.
			"error_message": "",
			"updated_at":    now,
			"completed_at":  now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// completeRetrying conditionally transitions id from StatusRunning back to
// StatusRetrying, recording cause and moving ScheduledAt to nextAttempt,
// and reports whether the transition actually happened. Same concurrent-
// Cancel no-op guard, the same claimed_by = owner ownership tail (an
// attempt whose row another writer took over must not schedule the new
// owner's execution for retry), and the same transition-report role as
// completeSucceeded: execute records the retry log line and the retry
// metrics only after a genuine running -> retrying move.
func completeRetrying(ctx context.Context, db *gorm.DB, owner string, id string, cause string, nextAttempt time.Time, now time.Time) (bool, error) {
	// The recorded cause is unbounded handler error text (worker.go's
	// settleFailedAttempt passes cause.Error()); fitDescriptiveText cuts it
	// to the error_message column's declared width -- never refused, since
	// a refusal is precisely the 22001 wedge this cut exists to prevent.
	cause = fitDescriptiveText(ctx, id, "error_message", cause, errorMessageColumnRunes)
	res := db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", id, string(StatusRunning), owner).
		Updates(map[string]any{
			"status":        string(StatusRetrying),
			"error_message": cause,
			"scheduled_at":  nextAttempt,
			"updated_at":    now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// completeDeadLetter conditionally transitions id from StatusRunning to
// StatusDeadLetter, recording cause, and reports whether the transition
// actually happened. Like claimOne, the WHERE ... status = 'running' guard
// turns a concurrent Cancel's markCancelled into the winner of the race
// (RowsAffected == 0, nil error) rather than letting this write overwrite
// StatusCancelled — and the claimed_by = owner tail carries the same
// ownership half every sibling completion write carries (see
// completeSucceeded): an attempt whose row another writer took over must
// not dead-letter the new owner's execution. The returned bool is what lets
// execute (worker.go) distinguish "this attempt really dead-lettered the
// Job" from "a Cancel already settled it", so that a FailureHook's
// OnFailure runs only for a genuine running -> dead-letter transition. See
// FailureHook's own doc comment for the boundary.
func completeDeadLetter(ctx context.Context, db *gorm.DB, owner string, id string, cause string, now time.Time) (bool, error) {
	// The recorded cause is unbounded handler error text (worker.go's
	// settleFailedAttempt passes cause.Error()); fitDescriptiveText cuts it
	// to the error_message column's declared width -- never refused, since
	// a refusal is precisely the 22001 wedge this cut exists to prevent.
	cause = fitDescriptiveText(ctx, id, "error_message", cause, errorMessageColumnRunes)
	result := db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ? AND claimed_by = ?", id, string(StatusRunning), owner).
		Updates(map[string]any{
			"status":        string(StatusDeadLetter),
			"error_message": cause,
			"updated_at":    now,
			"completed_at":  now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// markCancelled conditionally transitions id to StatusCancelled from any
// non-terminal status (Pending, Retrying or Running). It is idempotent:
// RowsAffected == 0 (id already terminal) is reported as success, not an
// error — see Queue.Cancel's own doc comment.
func markCancelled(ctx context.Context, db *gorm.DB, id string, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status IN ?", id, []string{string(StatusPending), string(StatusRetrying), string(StatusRunning)}).
		Updates(map[string]any{
			"status":     string(StatusCancelled),
			"updated_at": now,
		}).Error
}

// resetInterruptedRecords recovers from an unclean process exit: any row
// still StatusRunning at Start time cannot actually be running (this
// process just started), so it is moved back to StatusPending —
// re-attempted, exactly as docs/internal/07-platform-services.md requires:
// a restarted process must recover in-flight work, not lose it. Attempts
// and StartedAt are left as-is: only an attempt the worker handoff actually
// started (markAttemptStarted) ever incremented them, so this recovery
// never grants an extra attempt beyond MaxRetries and never counts an
// attempt whose Handle never ran.
//
// The reset is scoped by the two things that make it safe to run at all,
// both established by StandaloneQueue.Start before this is called: owner
// has just acquired the queue_writers registration (acquireWriterRegistration
// refused a live sibling), so no live writer exists whose StatusRunning
// rows this could steal mid-Handle — and owner is a brand-new token
// (newWriterOwner) that no live or previously-crashed run ever claimed
// rows under. The WHERE ... claimed_by != owner clause spells that second
// property out at the statement level: a row this queue itself claimed
// (impossible at Start, since claims happen only after Start returns) would
// be left alone even if Start were ever re-entered over a live run, while
// every row some other (now provably dead) writer claimed — and every
// legacy row carrying the empty claimed_by a pre-claim_by release wrote —
// is recovered.
func resetInterruptedRecords(ctx context.Context, db *gorm.DB, now time.Time, owner string) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("status = ? AND claimed_by != ?", string(StatusRunning), owner).
		Updates(map[string]any{
			"status":     string(StatusPending),
			"updated_at": now,
		}).Error
}

// deadLetterRecords returns every StatusDeadLetter record, across every
// tenant — callers (StandaloneQueue.DeadLetterJobs) apply their own access
// decision per record, exactly as findByID's callers do.
func deadLetterRecords(ctx context.Context, db *gorm.DB) ([]jobRecord, error) {
	var recs []jobRecord
	err := db.WithContext(ctx).Where("status = ?", string(StatusDeadLetter)).Find(&recs).Error
	if err != nil {
		return nil, fmt.Errorf("jobs: query dead letter jobs: %w", err)
	}
	return recs, nil
}

// queueDepthCount is one (job type, status) bucket's current row count,
// for registerQueueDepthGauge's callback.
type queueDepthCount struct {
	Type   string
	Status string
	Count  int64
}

// queueDepthByTypeAndStatus reports the current backlog (StatusPending or
// StatusRetrying) grouped by type and status, for the "jobs.queue.depth"
// gauge.
func queueDepthByTypeAndStatus(ctx context.Context, db *gorm.DB) ([]queueDepthCount, error) {
	var counts []queueDepthCount
	err := db.WithContext(ctx).Model(&jobRecord{}).
		Select("type, status, COUNT(*) AS count").
		Where("status IN ?", []string{string(StatusPending), string(StatusRetrying)}).
		Group("type, status").
		Find(&counts).Error
	if err != nil {
		return nil, fmt.Errorf("jobs: query queue depth: %w", err)
	}
	return counts, nil
}
