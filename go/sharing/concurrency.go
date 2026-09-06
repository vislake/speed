package sharing

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// txRetryBudget bounds how many times one of this module's guarded,
// database-arbitrated single-statement writes -- ShareRepository's
// tryRecordView, tryIncrementView and markRevoked -- retries after a
// transient, contention-only database failure: SQLite's SQLITE_BUSY, or
// PostgreSQL's detected deadlock or serialization failure.
// dbkit.IsRetryableConflict's own doc comment has each one; none is data
// corruption -- all are the database correctly refusing to let two
// overlapping writers proceed at once -- and the correct response is the
// same on both dialects: retry the guarded write from a fresh transaction,
// since the attempt that lost the race is exactly as valid as one that had
// simply started a little later.
//
// go/org/concurrency.go is the established precedent for this exact
// mechanism (its own txRetryBudget/txRetryBackoff/withRetry around the
// tree operations' multi-statement transactions); this module's guarded
// writes are single statements, so the retry lives around each write's one
// WithTenantSession rather than around a longer transaction, and -- unlike
// org's tree operations -- none of them needs to re-read anything on a
// retry: each statement's WHERE clause re-evaluates the row's state at
// execution time on every attempt, which is precisely what keeps a retried
// tryRecordView from double-counting (a view-count increment committed by
// someone else between attempts makes the retried attempt affect zero rows
// and report won == false, exactly as a first attempt would have).
//
// # Why runGuardedWrite also orders this module's own writes in-process
//
// The retry above is what a guarded write does when it genuinely loses a
// race against ANOTHER connection. On the SQLite unit tier there is a
// harder case the retry alone cannot converge on, and this module's own
// concurrent-access regression tests reproduce it: a tight convoy of many
// view-recording writers on one row keeps the file's write lock held
// almost continuously, and a writer waiting on that convoy only ever gets
// scheduled to re-check the lock at instants the lock is held -- so an
// attempt that does not win on its opening instant can wait out dbkit's
// full busy_timeout again and again without ever winning (measured on the
// plain runner's harshest scheduling: under GOMAXPROCS=1 the
// views-survive-revoke regression failed 5 of 8 runs with the retry alone,
// every lost attempt having burned the full 5s timeout; shorter per-attempt
// waits fared strictly worse, because the winning re-check only ever
// arrives after seconds of continuous waiting). runGuardedWrite therefore
// ALSO serializes this repository's own guarded writes behind one
// in-process mutex (ShareRepository.writeMu) for the duration of each
// attempt's transaction: every writer this module itself spawns -- view
// recordings, revocations, the expiry sweep -- then takes the file's write
// lock in turn with no contention at all, because no second connection of
// this module is mid-write while one holds the mutex. That is exactly the
// serialization SQLite's single-writer semantics imposes anyway, made
// orderly instead of contended: a revoke racing a view storm queues behind
// the current increment and lands on its next turn (Go's mutex starvation
// mode hands a long waiter the lock within milliseconds), deterministically,
// in every scheduling regime. The database remains the arbiter across
// connections the mutex cannot see -- other processes on the same file, or
// PostgreSQL replicas -- which is what the retry is for; the two
// mechanisms are complements, not substitutes, and the mutex's cost (this
// repository's guarded writes serialize per process) is the same
// serialization SQLite would impose anyway, while on PostgreSQL it merely
// moves a single-row update's ordering ahead of the database's own row
// lock.
//
// 5 is generous for the contention the retry actually needs to absorb once
// the mutex has removed this module's own writers from the file-lock
// lottery -- a genuine transient blip from a writer outside the mutex, or
// a PostgreSQL deadlock/serialization failure -- not a number tuned
// against a measured production workload. Exhausting the budget is
// deliberately NOT a distinct coded error (contrast org's
// org.concurrent_update, whose exhausted retry bound around a tree
// mutation is a state a caller can act on): every caller of a guarded
// write here already surfaces any store failure as sharing.internal_error
// (errors.go), and a conflict that outlasts the retry bound is the same
// class of operational fault the very first conflict would have been -- so
// withTxRetry returns the last conflict error raw and leaves that existing
// mapping to decide what the caller sees, exactly as if the retry had never
// run.
const txRetryBudget = 5

// txRetryBackoff is the fixed pause between retry attempts. It exists only
// to give the winner of a real conflict a moment to actually commit before
// the loser tries again -- retrying with no pause at all would mostly just
// reproduce the identical conflict immediately. It is deliberately small
// and fixed, not exponential: these writes sit on request-serving paths
// (Access, Revoke) as well as the expiry sweep, and txRetryBudget's small
// attempt count means the worst case (every attempt conflicts) adds at
// most a few milliseconds, not seconds.
const txRetryBackoff = 5 * time.Millisecond

// runGuardedWrite executes one guarded, database-arbitrated
// single-statement write -- fn is expected to run exactly one UPDATE whose
// WHERE clause re-evaluates the row's state -- inside a fresh
// dbkit.WithTenantSession transaction per attempt, ordered behind this
// repository's own writers by writeMu and retried through withTxRetry
// exactly as org's atomic operations retry their whole transactions (see
// txRetryBudget's own doc comment for why both halves exist). fn reports
// its outcome through captured variables (the attempt's RowsAffected-
// derived won flag, for example), exactly as org's withRetry closures
// communicate their transactions' results out.
func (r *ShareRepository) runGuardedWrite(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return withTxRetry(func() error {
		r.writeMu.Lock()
		defer r.writeMu.Unlock()
		return dbkit.WithTenantSession(ctx, r.db, fn)
	})
}

// withTxRetry runs op -- expected to open exactly one fresh
// dbkit.WithTenantSession transaction per call and to carry no state
// forward from a failed attempt -- up to txRetryBudget times, retrying
// only when op's error is one dbkit.IsRetryableConflict classifies as
// transient contention. Any other error is returned immediately,
// unretried. Exhausting the budget returns the last conflict error raw;
// each guarded write's caller wraps its store failure as ErrInternal
// (repository.go), which is the answer a conflict that outlasts the budget
// deserves -- the identical answer that same write gave the very first
// conflict before this retry existed.
func withTxRetry(op func() error) error {
	var err error
	for attempt := 0; attempt < txRetryBudget; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if !dbkit.IsRetryableConflict(err) {
			return err
		}
		if attempt < txRetryBudget-1 {
			time.Sleep(txRetryBackoff)
		}
	}
	return err
}
