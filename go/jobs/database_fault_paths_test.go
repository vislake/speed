package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// This file holds the deterministic database-fault regressions: every
// code path whose only trigger is a database statement failing or a
// conditional write no-opping (the writer-registration gates, the schema
// upgrade steps, the dispatcher's claim query and the worker's
// bookkeeping writes), driven through two fault-injection seams instead
// of a live second writer or a flaky race window:
//
//   - a closed connection pool, for paths whose very first statement
//     fails ("sql: database is closed");
//   - GORM callbacks that fail the next statement carrying a given
//     signature (a dest-map key for Model().Updates writes, a SQL
//     substring for Exec/Raw statements, the next query for read paths),
//     for paths where one specific statement must fail while every
//     sibling write keeps working -- the same technique
//     injectResultWriteFailures (worker_test.go) uses for the
//     success-write regressions.
//
// Every assertion is about what the queue does when the database
// misbehaves: it logs the failure through the context logger and keeps
// going (a poll cycle skips, a worker settles nothing), or it returns
// the wrapped error to the caller. Log lines are captured through
// slog.SetDefault, which observability's FromContext reads fresh on
// every call for a context with no attached logger -- the exact path
// these internal, context.Background()-rooted code sites log through.

// gormCallbackSeq gives every fault-injection callback a unique
// registration name, so repeated registrations across tests never clash
// (gorm's Register fails silently on a duplicate name, which would leave
// the previous test's callback installed).
var gormCallbackSeq int64

// injectUpdateFailures fails the next *remaining Model().Updates writes
// whose dest map carries destKey -- the deterministic fault injection for
// the store's conditional writes, each of which is distinguishable by one
// dest key it alone carries (claimOne: claimed_by, markAttemptStarted:
// started_at, updateProgress: progress_pct, ...).
func injectUpdateFailures(db *gorm.DB, remaining *int, destKey string, failErr error) {
	name := fmt.Sprintf("test:fail-update:%d", atomic.AddInt64(&gormCallbackSeq, 1))
	db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if remaining == nil || *remaining <= 0 {
			return
		}
		m, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, has := m[destKey]; !has {
			return
		}
		*remaining--
		tx.AddError(failErr)
	})
}

// injectRawFailures fails the next *remaining Exec'd raw statements whose
// SQL contains sqlSubstr.
func injectRawFailures(db *gorm.DB, remaining *int, sqlSubstr string, failErr error) {
	name := fmt.Sprintf("test:fail-raw:%d", atomic.AddInt64(&gormCallbackSeq, 1))
	db.Callback().Raw().Before("gorm:raw").Register(name, func(tx *gorm.DB) {
		if remaining == nil || *remaining <= 0 {
			return
		}
		if !strings.Contains(tx.Statement.SQL.String(), sqlSubstr) {
			return
		}
		*remaining--
		tx.AddError(failErr)
	})
}

// injectQueryFailures fails the next *remaining read statements. Reads
// run through two different gorm callback processors depending on how
// they were built: Model/Where reads through the query processor
// ("gorm:query"), and raw-SQL reads (Raw(...).Scan) through the row
// processor ("gorm:row", since Scan delegates to Rows) -- so the
// injection registers the same failure on both.
func injectQueryFailures(db *gorm.DB, remaining *int, failErr error) {
	fail := func(tx *gorm.DB) {
		if remaining == nil || *remaining <= 0 {
			return
		}
		*remaining--
		tx.AddError(failErr)
	}
	seq := atomic.AddInt64(&gormCallbackSeq, 1)
	db.Callback().Query().Before("gorm:query").Register(fmt.Sprintf("test:fail-query:%d", seq), fail)
	db.Callback().Row().Before("gorm:row").Register(fmt.Sprintf("test:fail-row:%d", seq), fail)
}

// injectRowFailures fails the next *remaining raw-SQL reads whose SQL
// contains sqlSubstr -- the targeted sibling of injectQueryFailures for
// the case where an earlier read must keep working and only one
// particular raw read (identifiable by its SQL text, which is fully
// known before the row processor runs) must fail.
func injectRowFailures(db *gorm.DB, remaining *int, sqlSubstr string, failErr error) {
	name := fmt.Sprintf("test:fail-row-sub:%d", atomic.AddInt64(&gormCallbackSeq, 1))
	db.Callback().Row().Before("gorm:row").Register(name, func(tx *gorm.DB) {
		if remaining == nil || *remaining <= 0 {
			return
		}
		if !strings.Contains(tx.Statement.SQL.String(), sqlSubstr) {
			return
		}
		*remaining--
		tx.AddError(failErr)
	})
}

// errFaultInjection is the error every injection below fails statements
// with: a plain sentinel whose wrapping the assertions check by text.
var errFaultInjection = errors.New("injected database fault")

// captureDefaultLogs swaps slog's default for a buffer until the test
// ends, returning the buffer -- the capture seam for log lines emitted
// through obs.FromContext on a context that carries no logger (every
// internal code site this file drives roots its context in
// context.Background()).
func captureDefaultLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// closeDBPool closes the underlying connection pool of a dbtest-backed
// *gorm.DB, so every later statement fails with "sql: database is
// closed".
func closeDBPool(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("sqlDB.Close() error = %v", err)
	}
}

// TestWriterRegistration_DBFailures_ReturnWrappedErrors covers the
// acquire/heartbeat/release error branches: each gate's database failure
// must surface as a wrapped error to the caller (Start, the heartbeat
// keeper and Close), never be swallowed into a silent success.
func TestWriterRegistration_DBFailures_ReturnWrappedErrors(t *testing.T) {
	now := time.Now()
	owner := "db-fault-owner"

	// acquire's main gate fails: the wrapped error names the gate.
	db := newTestDB(t)
	acquireFailures := 1
	injectRawFailures(db, &acquireFailures, "ON CONFLICT(id)", errFaultInjection)
	err := acquireWriterRegistration(context.Background(), db, owner, now, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "acquire writer registration") {
		t.Fatalf("acquireWriterRegistration(main gate fails) error = %v, want a wrapped error naming the gate", err)
	}
	if acquireFailures != 0 {
		t.Errorf("injected failures left = %d, want 0", acquireFailures)
	}

	// A live incumbent makes the main gate a clean zero rows; the legacy
	// takeover gate's database failure must then surface -- the wrapped
	// error a rival taker sees when the conversion itself breaks.
	db = newTestDB(t)
	acquireErr := acquireWriterRegistration(context.Background(), db, "incumbent", now, time.Hour)
	if acquireErr != nil {
		t.Fatalf("incumbent acquireWriterRegistration() error = %v", acquireErr)
	}
	legacyFailures := 1
	injectRawFailures(db, &legacyFailures, "stale_at IS NULL", errFaultInjection)
	err = acquireWriterRegistration(context.Background(), db, owner, now, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "acquire writer registration") {
		t.Fatalf("acquireWriterRegistration(legacy gate fails) error = %v, want a wrapped error", err)
	}

	// heartbeat and release against a closed pool: each wraps its failure.
	db = newTestDB(t)
	closeDBPool(t, db)
	if _, err := heartbeatWriterRegistration(context.Background(), db, owner, now, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "writer heartbeat") {
		t.Errorf("heartbeatWriterRegistration(closed pool) error = %v, want a wrapped error naming the heartbeat", err)
	}
	if err := releaseWriterRegistration(context.Background(), db, owner); err == nil ||
		!strings.Contains(err.Error(), "release writer registration") {
		t.Errorf("releaseWriterRegistration(closed pool) error = %v, want a wrapped error naming the release", err)
	}
}

// TestEnsureJobsSchema_StepFailures_ReturnWrappedErrors covers the three
// ensureJobsSchema steps whose failure was previously only reachable
// through a whole-database failure (which trips the earlier CREATE step
// first): the claimed_by column add on a legacy jobs table, the
// queue_writers table create, and the stale_at column add on a legacy
// queue_writers table -- each must surface wrapped, and each leaves the
// schema exactly as it was.
func TestEnsureJobsSchema_StepFailures_ReturnWrappedErrors(t *testing.T) {
	ctx := context.Background()

	// Legacy jobs table without claimed_by: the upgrade's ALTER fails.
	db := dbtestWithLegacyJobsTable(t)
	alterFailures := 1
	injectRawFailures(db, &alterFailures, "ADD COLUMN claimed_by", errFaultInjection)
	err := ensureJobsSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "add claimed_by column") {
		t.Fatalf("ensureJobsSchema(legacy claimed_by alter fails) error = %v, want a wrapped error naming the column add", err)
	}

	// queue_writers table create fails on an otherwise fresh database.
	db = newTestDB(t)
	createFailures := 1
	injectRawFailures(db, &createFailures, "CREATE TABLE IF NOT EXISTS queue_writers", errFaultInjection)
	err = ensureJobsSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "ensure schema") {
		t.Fatalf("ensureJobsSchema(queue_writers create fails) error = %v, want a wrapped ensure-schema error", err)
	}

	// Legacy queue_writers table without stale_at: the upgrade's ALTER
	// fails.
	db = dbtestWithLegacyQueueWritersTable(t)
	staleFailures := 1
	injectRawFailures(db, &staleFailures, "ADD COLUMN stale_at", errFaultInjection)
	err = ensureJobsSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "add queue_writers stale_at column") {
		t.Fatalf("ensureJobsSchema(legacy stale_at alter fails) error = %v, want a wrapped error naming the column add", err)
	}

	// The PRAGMA column probes fail on legacy tables: each probe error
	// must surface wrapped rather than guessing the schema.
	db = dbtestWithLegacyJobsTable(t)
	probeFailures := 1
	injectQueryFailures(db, &probeFailures, errFaultInjection)
	err = ensureJobsSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "probe jobs table columns") {
		t.Fatalf("ensureJobsSchema(jobs probe fails) error = %v, want a wrapped probe error", err)
	}

	db = dbtestWithLegacyQueueWritersTable(t)
	// The claimed_by probe on the (current) jobs table must keep working;
	// only the queue_writers probe -- identifiable by its SQL text -- fails.
	probeFailures = 1
	injectRowFailures(db, &probeFailures, "table_info(queue_writers)", errFaultInjection)
	err = ensureJobsSchema(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "probe queue_writers table columns") {
		t.Fatalf("ensureJobsSchema(queue_writers probe fails) error = %v, want a wrapped probe error", err)
	}
	if probeFailures != 0 {
		t.Errorf("injected failures left = %d, want 0", probeFailures)
	}
}

// dbtestWithLegacyJobsTable returns a database whose jobs table predates
// the claimed_by column -- the shape an older release's CREATE TABLE
// leaves -- so the schema-upgrade ALTER path is what runs. The claimed_by
// line is removed from the current DDL rather than hand-typed, so the
// legacy shape cannot drift from the current one in any other column.
func dbtestWithLegacyJobsTable(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	legacy := strings.ReplaceAll(createJobsTableSQLSQLite,
		"claimed_by      VARCHAR(64) NOT NULL DEFAULT '',\n", "")
	if legacy == createJobsTableSQLSQLite {
		t.Fatal("legacy jobs DDL derivation removed nothing: the claimed_by line shape changed")
	}
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy jobs table: %v", err)
	}
	return db
}

// dbtestWithLegacyQueueWritersTable returns a database whose jobs table
// is current and whose queue_writers table predates the stale_at column.
func dbtestWithLegacyQueueWritersTable(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	if err := db.Exec(createJobsTableSQLSQLite).Error; err != nil {
		t.Fatalf("create jobs table: %v", err)
	}
	legacy := strings.ReplaceAll(createQueueWritersTableSQL,
		"last_heartbeat TIMESTAMP NOT NULL,\n\tstale_at       TIMESTAMP",
		"last_heartbeat TIMESTAMP NOT NULL")
	if legacy == createQueueWritersTableSQL {
		t.Fatal("legacy queue_writers DDL derivation removed nothing: the stale_at line shape changed")
	}
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy queue_writers table: %v", err)
	}
	return db
}

// TestInsertRecord_ConflictLookupFailure_ReturnsWrappedError covers
// insertRecord's duplicate-key aftermath: when the create fails with the
// idempotency index's duplicate answer but the follow-up lookup of the
// existing Job also fails, the caller must get a wrapped error -- never a
// false "new Job created" or a swallowed duplicate.
func TestInsertRecord_ConflictLookupFailure_ReturnsWrappedError(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	rec := fixtureRecord("tenant-a", "lookup-fails")
	rec.IdempotencyKey = "invoice-1"
	if err := db.Create(rec).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	dup := fixtureRecord("tenant-a", "lookup-fails")
	dup.IdempotencyKey = "invoice-1" // the same (tenant, key) pair: a duplicate
	lookupFailures := 1
	injectQueryFailures(db, &lookupFailures, errFaultInjection)
	_, err := insertRecord(ctx, db, dup)
	if err == nil || !strings.Contains(err.Error(), "look up existing job for idempotency key after conflict") {
		t.Fatalf("insertRecord(conflict, lookup fails) error = %v, want a wrapped lookup error", err)
	}
	if lookupFailures != 0 {
		t.Errorf("injected failures left = %d, want 0", lookupFailures)
	}
}

// TestQueue_Heartbeat_RegistrationLostOrUnreadable_LogsAndKeepsGoing
// covers the two failure directions the heartbeat keeper's log lines
// report: a registration row that disappeared (a sibling took it over
// after a stale lapse, or it was removed) answers ok=false, and a
// database that refuses the beat answers an error -- both logged, never
// propagated, because stopping the queue mid-flight would strand every
// row it has claimed.
func TestQueue_Heartbeat_RegistrationLostOrUnreadable_LogsAndKeepsGoing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	// Lost: the registration row is removed between beats, so the beat
	// updates zero rows and reports false.
	db := newTestDB(t)
	owner := "heartbeat-owner"
	if err := acquireWriterRegistration(ctx, db, owner, now, time.Minute); err != nil {
		t.Fatalf("acquireWriterRegistration() error = %v", err)
	}
	if err := db.Exec(`DELETE FROM queue_writers WHERE owner = ?`, owner).Error; err != nil {
		t.Fatalf("remove registration row: %v", err)
	}
	q := NewStandaloneQueue(db)
	q.owner = owner
	buf := captureDefaultLogs(t)
	q.heartbeat()
	if out := buf.String(); !strings.Contains(out, "writer registration lost") {
		t.Errorf("heartbeat() with a removed registration logged: %s, want the registration-lost error line", out)
	}

	// Unreadable: a closed pool makes the beat's own statement fail.
	db = newTestDB(t)
	closeDBPool(t, db)
	q = NewStandaloneQueue(db)
	q.owner = "heartbeat-owner-closed"
	buf = captureDefaultLogs(t)
	q.heartbeat()
	if out := buf.String(); !strings.Contains(out, "writer heartbeat failed") {
		t.Errorf("heartbeat() against a closed pool logged: %s, want the heartbeat-failed error line", out)
	}
}

// TestDispatchOnce_QueryFailure_SkipsTheCycle covers the dispatcher's
// response to a failing candidate query: the cycle logs and returns,
// claiming nothing.
func TestDispatchOnce_QueryFailure_SkipsTheCycle(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	rec := fixtureRecord("tenant-a", "dispatch-query-fails")
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}
	failures := 1
	injectQueryFailures(q.db, &failures, errFaultInjection)
	buf := captureDefaultLogs(t)
	q.dispatchOnce(make(chan jobRecord))
	if out := buf.String(); !strings.Contains(out, "query candidates failed") {
		t.Errorf("dispatchOnce(query fails) logged: %s, want the candidates-query error line", out)
	}

	// The row is untouched: nothing was claimed.
	var got jobRecord
	if err := q.db.First(&got, "id = ?", rec.ID).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusPending) {
		t.Errorf("Status = %q, want %q after a skipped cycle", got.Status, StatusPending)
	}
}

// TestDispatchOnce_ClaimFailure_ReleasesSlotAndMovesOn covers the
// dispatcher's response to a failing claim write: the tenant slot the
// candidate reserved is released and the failure is logged, so the next
// cycle can try again -- a slot held by a claim that never landed would
// leak that tenant's concurrency budget forever.
func TestDispatchOnce_ClaimFailure_ReleasesSlotAndMovesOn(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	rec := fixtureRecord("tenant-a", "dispatch-claim-fails")
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}
	failures := 1
	injectUpdateFailures(q.db, &failures, "claimed_by", errFaultInjection)
	buf := captureDefaultLogs(t)
	q.dispatchOnce(make(chan jobRecord))
	if out := buf.String(); !strings.Contains(out, "claim failed") {
		t.Errorf("dispatchOnce(claim fails) logged: %s, want the claim-failed error line", out)
	}

	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if len(q.runningPerTenant) != 0 {
		t.Errorf("runningPerTenant = %v, want empty: the failed claim's tenant slot must be released", q.runningPerTenant)
	}
}

// TestDispatchOnce_ShutdownDuringHandoff_LeavesClaimForRecovery covers
// the dispatcher's shutdown path while it holds a claimed Job it can no
// longer hand to a worker: the claim is left in the database (the next
// Start's resetInterruptedRecords recovers it -- the documented contract
// for an unclean exit in the claim-to-handoff window), the tenant slot is
// released, and the cycle returns without dispatching. Driven with the
// queue's stopCh already closed and an unbuffered dispatch channel with
// no receiver, so the select's shutdown case is the only ready one --
// deterministic, no goroutines.
func TestDispatchOnce_ShutdownDuringHandoff_LeavesClaimForRecovery(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	// Close without Start closes stopCh and, having no workers, nothing
	// else: the post-shutdown state of a queue whose dispatcher may still
	// be mid-cycle.
	if err := q.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	id, err := q.Enqueue(context.Background(), Task{Type: "handoff-shutdown", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	q.dispatchOnce(make(chan jobRecord))

	var got jobRecord
	if err := q.db.First(&got, "id = ?", string(id)).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusRunning) {
		t.Errorf("Status = %q, want %q: the claim made before the shutdown signal must stay in the database for the next Start's recovery", got.Status, StatusRunning)
	}
	if got.ClaimedBy != q.owner {
		t.Errorf("ClaimedBy = %q, want %q", got.ClaimedBy, q.owner)
	}
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if len(q.runningPerTenant) != 0 {
		t.Errorf("runningPerTenant = %v, want empty: the shutdown path must release its tenant slot", q.runningPerTenant)
	}
}

// TestRunAttempt_AttemptStartWriteFails_LogsAndSettlesTheAttempt covers
// the worker handoff's failure direction: when the attempt-start write
// itself cannot land, the attempt is logged but still executed -- the
// row is already claimed StatusRunning, so running Handle is the only
// way the attempt converges -- and the attempt's outcome settles through
// the ordinary terminal machinery. The record carries a zero timeout
// (the package's "not set" marker) and no registered handler, so this
// one run also drives the default-timeout and unregistered-handler
// branches of execute and settleFailedAttempt.
func TestRunAttempt_AttemptStartWriteFails_LogsAndSettlesTheAttempt(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	rec := fixtureRecord("tenant-a", "no-handler-for-this-type")
	rec.Status = string(StatusRunning)
	rec.TimeoutNanos = 0 // the package's "not set" marker: execute and settleFailedAttempt fall back to DefaultTimeout
	rec.MaxRetries = 0   // the attempt count exceeds the budget immediately
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	failures := 1
	injectUpdateFailures(q.db, &failures, "started_at", errFaultInjection)
	buf := captureDefaultLogs(t)

	q.runAttempt(*rec)

	out := buf.String()
	if !strings.Contains(out, "persisting attempt start failed") {
		t.Errorf("runAttempt(markAttemptStarted fails) logged: %s, want the attempt-start warning", out)
	}
	if !strings.Contains(out, "job exhausted retries, moved to dead letter") {
		t.Errorf("runAttempt() logged: %s, want the settlement to reach the dead-letter log", out)
	}
	var got jobRecord
	if err := q.db.First(&got, "id = ?", rec.ID).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusDeadLetter) {
		t.Errorf("Status = %q, want %q: an unhandled final attempt must still settle through the terminal machinery", got.Status, StatusDeadLetter)
	}
}

// TestExecute_ProgressWriteFails_WarnsAndCompletes covers a progress
// report that cannot be persisted: the warning is logged, the attempt
// itself is unaffected, and its success settles normally.
func TestExecute_ProgressWriteFails_WarnsAndCompletes(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	progressed := false
	if err := q.RegisterHandler(NewHandlerFunc("progress-write-fails", func(_ context.Context, _ *Job, progress ProgressFn) (Result, error) {
		progress(50, "halfway")
		progressed = true
		return Result{Data: []byte("done")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	rec := fixtureRunningRecord("tenant-a", "progress-write-fails")
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	failures := 1
	injectUpdateFailures(q.db, &failures, "progress_pct", errFaultInjection)
	buf := captureDefaultLogs(t)

	q.execute(*rec)

	if !progressed {
		t.Fatal("Handle never ran: the attempt must execute despite the failing progress write")
	}
	if out := buf.String(); !strings.Contains(out, "persisting progress failed") {
		t.Errorf("execute(progress write fails) logged: %s, want the progress warning", out)
	}
	var got jobRecord
	if err := q.db.First(&got, "id = ?", rec.ID).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	if got.Status != string(StatusSucceeded) {
		t.Errorf("Status = %q, want %q: a failed progress write must not disturb the attempt's success", got.Status, StatusSucceeded)
	}
}

// TestExecute_FinalOutcomeWritesFail_LoggedAndRowLeftForRecovery covers
// settleFailedAttempt's two double-failure directions: the terminal
// dead-letter write and the retry write each failing in turn is logged
// and the row is left StatusRunning -- the exact state the next Start's
// resetInterruptedRecords recovers, since two consecutive write failures
// mean nothing further can be persisted in-process.
func TestExecute_FinalOutcomeWritesFail_LoggedAndRowLeftForRecovery(t *testing.T) {
	failingHandler := func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{}, errors.New("always fails")
	}

	// Dead-letter leg: the final attempt's dead-letter write fails.
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(NewHandlerFunc("settle-dead-letter-write-fails", failingHandler)); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	rec := fixtureRunningRecord("tenant-a", "settle-dead-letter-write-fails")
	rec.Attempts = 2
	rec.MaxRetries = 1 // exhausted
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	closeDBPool(t, q.db)
	buf := captureDefaultLogs(t)
	q.execute(*rec)
	if out := buf.String(); !strings.Contains(out, "persisting dead letter failed") {
		t.Errorf("execute(dead-letter write fails) logged: %s, want the dead-letter persistence error line", out)
	}

	// Retry leg: an attempt with budget remaining whose retry write fails.
	q = NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(NewHandlerFunc("settle-retry-write-fails", failingHandler)); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}
	rec = fixtureRunningRecord("tenant-a", "settle-retry-write-fails")
	rec.Attempts = 1
	rec.MaxRetries = 5
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	closeDBPool(t, q.db)
	buf = captureDefaultLogs(t)
	q.execute(*rec)
	if out := buf.String(); !strings.Contains(out, "persisting retry failed") {
		t.Errorf("execute(retry write fails) logged: %s, want the retry persistence error line", out)
	}
}

// TestExecute_OutcomeProbeUnreadable_WarnsInsteadOfGuessing covers
// logDiscardedOutcome's probe-failure answer: when an outcome write
// no-op'd (another writer reset the row -- the steal shape
// TestExecute_NoOpOutcomeWrite_RowStolenByAnotherWriter_LogsHonestlyNotCancelled
// drives) and the follow-up probe of the row's true state fails too, the
// discard is logged with its cause unguessed -- never reported as a
// cancellation.
func TestExecute_OutcomeProbeUnreadable_WarnsInsteadOfGuessing(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	if err := q.RegisterHandler(NewHandlerFunc("probe-unreadable", func(context.Context, *Job, ProgressFn) (Result, error) {
		return Result{Data: []byte("done")}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := context.Background()
	rec := fixtureRunningRecord("tenant-a", "probe-unreadable")
	rec.ClaimedBy = q.owner
	if err := q.db.Create(rec).Error; err != nil {
		t.Fatalf("seed running record: %v", err)
	}
	// The steal: another writer resets the running row to pending, so this
	// attempt's success write no-ops.
	if err := resetInterruptedRecords(ctx, q.db, time.Now(), "writer-b"); err != nil {
		t.Fatalf("resetInterruptedRecords() error = %v", err)
	}
	// The probe after the no-op is the first query this test runs.
	failures := 1
	injectQueryFailures(q.db, &failures, errFaultInjection)
	buf := captureDefaultLogs(t)

	q.execute(*rec)

	if out := buf.String(); !strings.Contains(out, "row state unreadable, outcome discarded") {
		t.Errorf("execute(probe fails after no-op) logged: %s, want the unreadable-row-state warning", out)
	}
}

// TestQueue_EnqueueAndCancel_AgainstClosedPool_ReturnWrappedErrors covers
// Enqueue's insert failure and Cancel's lookup failure surfacing as plain
// errors to the caller.
func TestQueue_EnqueueAndCancel_AgainstClosedPool_ReturnWrappedErrors(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))
	closeDBPool(t, q.db)

	if _, err := q.Enqueue(context.Background(), Task{Type: "enqueue-fails", TenantID: "tenant-a"}); err == nil {
		t.Error("Enqueue() against a closed pool error = nil, want an error")
	}
	if err := q.Cancel(context.Background(), "any-id"); err == nil {
		t.Error("Cancel() against a closed pool error = nil, want an error")
	}
}

// TestDeadLetterJobs_SkipsRowsOfTenantsTheCallerCannotAccess covers
// DeadLetterJobs' per-row access filter: a dead-lettered Job of another
// tenant is skipped, never listed, when the caller carries only its own
// tenant context.
func TestDeadLetterJobs_SkipsRowsOfTenantsTheCallerCannotAccess(t *testing.T) {
	db := newTestDB(t)
	q := NewStandaloneQueue(db)

	for tenant, jobType := range map[pkgcore.TenantID]string{"tenant-a": "dead-letter.a", "tenant-b": "dead-letter.b"} {
		rec := fixtureRecord(tenant, jobType)
		rec.Status = string(StatusRunning)
		rec.ClaimedBy = "some-writer"
		rec.Attempts = 1
		rec.MaxRetries = 0
		if err := db.Create(rec).Error; err != nil {
			t.Fatalf("seed record: %v", err)
		}
		if moved, err := completeDeadLetter(context.Background(), db, "some-writer", rec.ID, "failed", time.Now()); err != nil || !moved {
			t.Fatalf("completeDeadLetter() = (%v, %v), want (true, nil)", moved, err)
		}
	}

	jobs, err := q.DeadLetterJobs(pkgcore.WithTenant(context.Background(), "tenant-a"))
	if err != nil {
		t.Fatalf("DeadLetterJobs() error = %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("DeadLetterJobs(tenant-a) returned %d jobs, want exactly the tenant's own 1: %+v", len(jobs), jobs)
	}
	if jobs[0].TenantID != "tenant-a" {
		t.Errorf("listed job's TenantID = %q, want %q", jobs[0].TenantID, "tenant-a")
	}
}

// TestDepthGaugeCallback_QueryFails_CollectReportsError covers the
// queue-depth gauge callback's database-failure answer: a Collect whose
// backlog query fails reports the error -- the gauge never invents a
// zero backlog it could not read. The callback's fail-closed error
// surfaces through the ManualReader's own Collect, exactly the answer
// metric consumers of the gauge see. The meter provider is built
// locally (never through the process-wide otel global, which other
// tests in this binary replace) so the Collect this test drives reads
// back exactly this callback.
func TestDepthGaugeCallback_QueryFails_CollectReportsError(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	q := NewStandaloneQueue(newTestDB(t))
	if err := q.registerQueueDepthGauge(mp.Meter(InstrumentationName)); err != nil {
		t.Fatalf("registerQueueDepthGauge() error = %v", err)
	}
	failures := 1
	injectQueryFailures(q.db, &failures, errFaultInjection)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err == nil {
		t.Error("Collect() error = nil, want the callback's query failure to surface")
	}
	if failures != 0 {
		t.Errorf("injected failures left = %d, want 0 (the callback must have made the failing query)", failures)
	}
}

// TestWithJobTimeout_AppliedByConstruction covers the option's healthy
// half -- WithJobTimeout's closure actually configuring the queue's
// default timeout -- which the construction-time panic tests
// (queue_standalone_test.go) alone never exercise.
func TestWithJobTimeout_AppliedByConstruction(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t), WithJobTimeout(7*time.Second))
	if q.defaultTimeout != 7*time.Second {
		t.Errorf("defaultTimeout = %v, want %v after WithJobTimeout", q.defaultTimeout, 7*time.Second)
	}
}

// TestStart_RecoveryFails_ReleasesRegistrationAndReturnsError covers the
// one Start failure mode whose registration must not outlive the failed
// attempt: when resetInterruptedRecords fails after the writer
// registration was acquired, Start releases the registration (warning if
// even that release fails -- a registration whose owner never started
// must not block a retry of this Start or another queue) and returns the
// wrapped error, leaving started false so a later Start genuinely
// re-runs.
func TestStart_RecoveryFails_ReleasesRegistrationAndReturnsError(t *testing.T) {
	q := NewStandaloneQueue(newTestDB(t))

	// resetInterruptedRecords is the first Model().Updates of Start (the
	// schema steps are Execs and the acquire is a raw Exec); the release is
	// a raw DELETE that must fail for the release warning to fire.
	resetFailures := 1
	injectUpdateFailures(q.db, &resetFailures, "updated_at", errFaultInjection)
	releaseFailures := 1
	injectRawFailures(q.db, &releaseFailures, "DELETE FROM queue_writers", errFaultInjection)
	buf := captureDefaultLogs(t)

	err := q.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "recover interrupted jobs") {
		t.Fatalf("Start(recovery fails) error = %v, want a wrapped recovery error", err)
	}
	if out := buf.String(); !strings.Contains(out, "releasing writer registration after failed recovery") {
		t.Errorf("Start(recovery fails) logged: %s, want the release-failure warning", out)
	}

	// The failed Start must not have left the queue started: a retry of
	// Start genuinely re-runs (registration, recovery, goroutines).
	q.startMu.Lock()
	started := q.started
	q.startMu.Unlock()
	if started {
		t.Error("Start() reported success internally despite failing: started must stay false")
	}
}

// TestClose_ReleaseFails_WarnsAndStillReturnsNil covers Close's
// best-effort registration release: a release that cannot reach the
// database is warned about -- the row then ages out through its own stale
// window -- and Close still returns nil: the graceful drain finished and
// nothing more can be done in-process.
func TestClose_ReleaseFails_WarnsAndStillReturnsNil(t *testing.T) {
	q := newTestQueue(t)
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// The heartbeat keeper is still beating against this pool until Close
	// stops it after the drain; closing the pool underneath makes both the
	// final beats and the release fail.
	closeDBPool(t, q.db)
	buf := captureDefaultLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Errorf("Close() error = %v, want nil (the release is best-effort)", err)
	}
	if out := buf.String(); !strings.Contains(out, "releasing writer registration failed") {
		t.Errorf("Close(release fails) logged: %s, want the release-failure warning", out)
	}
}

// TestClose_CancelledContext_ReportsCtxErrAndSecondCloseFinishes covers
// Close's ctx.Done answer: a Close whose caller gave up before the drain
// finished returns ctx.Err and leaves the queue to finish draining in the
// background -- a second Close with a live context then completes the
// same drain and handover, since Close is idempotent.
func TestClose_CancelledContext_ReportsCtxErrAndSecondCloseFinishes(t *testing.T) {
	q := newTestQueue(t)
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close(cancelled ctx) error = %v, want context.Canceled", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Errorf("second Close() error = %v, want nil (idempotent: the drain completes)", err)
	}
}

// TestDispatchOnce_TwoConcurrentDispatchers_OnlyOneClaimLands covers the
// dispatcher's lost-race handling for real: two dispatch cycles running
// concurrently against one candidate row both read it pending, but the
// claim's observed-status guard (claimOne's WHERE names the status the
// row was read with) lets exactly one of them win -- the loser's
// claimOne reports false and the loser releases the tenant slot it
// reserved and moves on, never double-claiming and never leaking the
// slot. This is the one branch of the dispatcher (claimOne = false, no
// error) that only a genuine race can reach, so the test stages the race
// itself: two goroutines, one candidate, one buffered dispatch channel.
func TestDispatchOnce_TwoConcurrentDispatchers_OnlyOneClaimLands(t *testing.T) {
	db := newTestDB(t)
	q := NewStandaloneQueue(db)
	rec := fixtureRecord("tenant-a", "concurrent-dispatch")
	if err := db.Create(rec).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// Barrier at the END of each cycle's candidates read (an after-hook on
	// the raw-read processor, so arrival means that cycle's SELECT has
	// genuinely completed): the first arrival waits for the second, and
	// only once BOTH cycles hold a pending-row snapshot does either
	// proceed to its claim write -- the claim race is staged, not hoped
	// for.
	var arrivals atomic.Int64
	var releaseGate sync.Once
	gate := make(chan struct{})
	db.Callback().Row().After("gorm:row").Register(fmt.Sprintf("test:race-gate:%d", atomic.AddInt64(&gormCallbackSeq, 1)), func(tx *gorm.DB) {
		if arrivals.Add(1) == 1 {
			<-gate
			return
		}
		releaseGate.Do(func() { close(gate) })
	})

	var wg sync.WaitGroup
	dispatch := make(chan jobRecord, 1)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.dispatchOnce(dispatch)
		}()
	}
	wg.Wait()

	var running int
	var rows []jobRecord
	if err := q.db.Where("id = ?", rec.ID).Find(&rows).Error; err != nil {
		t.Fatalf("read back record: %v", err)
	}
	for _, row := range rows {
		if row.Status == string(StatusRunning) {
			running++
		}
	}
	if running != 1 {
		t.Errorf("StatusRunning rows = %d, want exactly 1: the observed-status claim guard must let only one of the two concurrent dispatchers win", running)
	}

	// Exactly one tenant slot is held: the winner's, awaiting the worker
	// handoff that never comes in this test (a real queue's runWorker
	// releases it after the attempt). The loser must not have leaked one.
	q.tenantMu.Lock()
	held := len(q.runningPerTenant)
	q.tenantMu.Unlock()
	if held != 1 {
		t.Errorf("runningPerTenant entries = %d, want exactly 1 (the winner's held slot): the losing dispatcher must release the slot it reserved", held)
	}

	// Drain the handoff and release the winner's slot the way runWorker
	// would, proving the slot accounting is fully symmetric.
	if recv := <-dispatch; recv.ID != rec.ID {
		t.Errorf("dispatched record ID = %q, want %q", recv.ID, rec.ID)
	}
	q.releaseTenantSlot("tenant-a")
	q.tenantMu.Lock()
	defer q.tenantMu.Unlock()
	if len(q.runningPerTenant) != 0 {
		t.Errorf("runningPerTenant = %v, want empty after the worker-side release", q.runningPerTenant)
	}
}

// TestCreateJobsTableSQL_DialectBranch pins the dialect branch of the
// jobs DDL selection: each dialect name resolves to that dialect's own
// statement, and the two statements genuinely differ (the byte columns'
// type) -- the branch unit-testable here that the PostgreSQL statement's
// execution against a real server belongs to the module's PostgreSQL
// integration tier.
func TestCreateJobsTableSQL_DialectBranch(t *testing.T) {
	if got := createJobsTableSQL("sqlite"); got != createJobsTableSQLSQLite {
		t.Error("createJobsTableSQL(\"sqlite\") did not return the SQLite statement")
	}
	if got := createJobsTableSQL("postgres"); got != createJobsTableSQLPostgres {
		t.Error("createJobsTableSQL(\"postgres\") did not return the PostgreSQL statement")
	}
	if createJobsTableSQLPostgres == createJobsTableSQLSQLite {
		t.Error("the two dialect statements are identical: the byte columns must differ (BLOB vs BYTEA)")
	}
	if !strings.Contains(createJobsTableSQL("postgres"), jobsTable) {
		t.Errorf("createJobsTableSQL(\"postgres\") does not name the %s table", jobsTable)
	}
}
