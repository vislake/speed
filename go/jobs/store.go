package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
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

	ScheduledAt time.Time  `gorm:"column:scheduled_at;not null"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
	StartedAt   *time.Time `gorm:"column:started_at"`
	CompletedAt *time.Time `gorm:"column:completed_at"`
}

// TableName pins jobRecord to jobsTable, so it does not depend on GORM's
// pluralization of the (unexported) type name.
func (jobRecord) TableName() string { return jobsTable }

// createJobsTableSQL is executed imperatively, with a plain CREATE TABLE IF
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
// disproportionate. The statement is written to be portable across
// both dbkit dialects anyway
// (VARCHAR/TEXT/INTEGER/TIMESTAMP, application-generated ids, no
// PostgreSQL- or SQLite-specific syntax), matching the backend coding
// standard's dual-dialect rule, even though only SQLite is exercised in
// the standalone deployment mode — see AGENTS.md's Known
// limitations for why this module's own tests do not also run this
// against dbtest.NewPostgres.
const createJobsTableSQL = `CREATE TABLE IF NOT EXISTS ` + jobsTable + ` (
	id              VARCHAR(36) NOT NULL PRIMARY KEY,
	type            VARCHAR(255) NOT NULL,
	tenant_id       VARCHAR(64) NOT NULL,
	payload         BLOB,
	idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
	status          VARCHAR(32) NOT NULL,
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

// ensureJobsSchema creates the jobs table and its indexes if they do not
// already exist. Safe to call every time Start runs.
func ensureJobsSchema(ctx context.Context, db *gorm.DB) error {
	for _, stmt := range [...]string{createJobsTableSQL, createJobsDispatchIndexSQL, createJobsIdempotencySQL} {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return fmt.Errorf("jobs: ensure schema: %w", err)
		}
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

// claimCandidates returns up to limit Jobs eligible to run (StatusPending
// or StatusRetrying, ScheduledAt <= now), ordered by Priority descending
// then ScheduledAt ascending — the order dispatchOnce offers them to
// per-tenant concurrency gating and claimOne in.
func claimCandidates(ctx context.Context, db *gorm.DB, now time.Time, limit int) ([]jobRecord, error) {
	var recs []jobRecord
	err := db.WithContext(ctx).
		Where("status IN ? AND scheduled_at <= ?", []string{string(StatusPending), string(StatusRetrying)}, now).
		Order("priority DESC, scheduled_at ASC").
		Limit(limit).
		Find(&recs).Error
	if err != nil {
		return nil, fmt.Errorf("jobs: query claim candidates: %w", err)
	}
	return recs, nil
}

// claimOne atomically transitions rec (identified by rec.ID and the status
// it was read with) to StatusRunning, incrementing Attempts. It reports
// false, with no error, when another operation already moved the row out
// of that status first — the observed-status guard in the WHERE clause
// turns a lost race into a no-op instead of a double execution, which
// matters if the standalone deployment mode's implementation is ever
// driven by more than the one dispatcher goroutine it ships with today.
func claimOne(ctx context.Context, db *gorm.DB, rec jobRecord, now time.Time) (bool, error) {
	result := db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", rec.ID, rec.Status).
		Updates(map[string]any{
			"status":     string(StatusRunning),
			"attempts":   rec.Attempts + 1,
			"started_at": now,
			"updated_at": now,
		})
	if result.Error != nil {
		return false, fmt.Errorf("jobs: claim job: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// updateProgress persists one progress report.
func updateProgress(ctx context.Context, db *gorm.DB, id string, pct int, msg string) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"progress_pct": pct,
			"progress_msg": msg,
			"updated_at":   time.Now(),
		}).Error
}

// completeSucceeded conditionally transitions id from StatusRunning to
// StatusSucceeded. Like claimOne, the WHERE ... status = 'running' guard
// makes this a no-op (RowsAffected == 0, no error) rather than an
// overwrite when a concurrent Cancel already moved the row to
// StatusCancelled while Handle was still executing — see Queue.Cancel's
// own doc comment.
func completeSucceeded(ctx context.Context, db *gorm.DB, id string, result Result, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", id, string(StatusRunning)).
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
		}).Error
}

// completeRetrying conditionally transitions id from StatusRunning back to
// StatusRetrying, recording cause and moving ScheduledAt to nextAttempt.
// Same concurrent-Cancel no-op guard as completeSucceeded.
func completeRetrying(ctx context.Context, db *gorm.DB, id string, cause string, nextAttempt time.Time, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", id, string(StatusRunning)).
		Updates(map[string]any{
			"status":        string(StatusRetrying),
			"error_message": cause,
			"scheduled_at":  nextAttempt,
			"updated_at":    now,
		}).Error
}

// completeDeadLetter conditionally transitions id from StatusRunning to
// StatusDeadLetter. Same concurrent-Cancel no-op guard as
// completeSucceeded.
func completeDeadLetter(ctx context.Context, db *gorm.DB, id string, cause string, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("id = ? AND status = ?", id, string(StatusRunning)).
		Updates(map[string]any{
			"status":        string(StatusDeadLetter),
			"error_message": cause,
			"updated_at":    now,
			"completed_at":  now,
		}).Error
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
// a restarted process must recover in-flight work, not lose it. Its
// Attempts count is left as-is (already incremented at the claim that was
// interrupted), so this recovery does not grant an extra attempt beyond
// MaxRetries.
func resetInterruptedRecords(ctx context.Context, db *gorm.DB, now time.Time) error {
	return db.WithContext(ctx).Model(&jobRecord{}).
		Where("status = ?", string(StatusRunning)).
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
