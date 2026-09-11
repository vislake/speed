package authn

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// txRetryBudget bounds how many times an authn store call that runs through
// withConflictRetry (below) re-runs after a transient, contention-only
// database failure: SQLite's SQLITE_BUSY, or PostgreSQL's detected deadlock
// or serialization failure (dbkit.IsRetryableConflict's own doc comment has
// each one and why dbkit, not authn, is what recognizes them). None is data
// corruption -- all are the database correctly refusing to let two
// overlapping writers proceed at once -- and the correct response on both
// dialects is the same: run the call again from scratch, since the attempt
// that lost the race is exactly as valid as one that had simply started a
// little later.
//
// go/org/concurrency.go and go/sharing/concurrency.go are the established
// precedents for this mechanism (their own txRetryBudget/txRetryBackoff and
// retry envelopes around entire transactions); this module's store calls
// follow the identical shape, with the retry living around ONE repository
// call per attempt. The budget of 5 is the same generous bound both
// precedents use, not a number tuned against a measured workload.
//
// The latency envelope here is wider than the precedents' own "at most a
// few milliseconds" because of what a failed attempt itself costs: under
// SQLite every conflicted statement first WAITS through the dialect's
// bounded busy_timeout (dbkit/dialect/sqlite's 5s default) before it
// reports the conflict, and this module builds no in-process ordering gate
// of the sharing kind, so a statement the file's holder keeps blocking for
// the whole busy window is retried up to txRetryBudget times. The worst
// case for one wrapped call is therefore txRetryBudget expired busy windows
// (~25s on the 5s default) when contention outlasts every attempt -- and the
// budget's value is exactly that the retry, unlike the un-retried call,
// survives a lock that frees within that span. Exhausting the budget
// returns the last conflict error raw: no distinct coded error, because
// every caller here already maps an unclassified store failure to
// ErrInternal (the module's existing internal-error path), and a conflict
// that outlasts the retry bound is the same class of operational fault the
// first conflict would have been -- so the response it produces is exactly
// the one a single un-retried attempt produces today.
const txRetryBudget = 5

// txRetryBackoff is the fixed pause between retry attempts. It exists only
// to give the winner of a real conflict a moment to actually commit before
// the loser tries again -- retrying with no pause at all would mostly just
// reproduce the identical conflict immediately. It is deliberately small
// and fixed, not exponential: these calls sit on request-serving paths, and
// a conflict that is already instant (a reader refused during a writer's
// commit window, the shape where the wait is not the busy handler's but an
// immediate refusal) is exactly what a short pause lets the next attempt
// ride out.
const txRetryBackoff = 5 * time.Millisecond

// withConflictRetry runs op up to txRetryBudget times, retrying only when
// op's error is one dbkit.IsRetryableConflict classifies as transient
// contention. Any other error is returned immediately, unretried and
// unwrapped, so a real failure is never masked behind repeated attempts.
// Exhausting the budget returns the last conflict error raw -- see
// txRetryBudget's own doc comment for why that is the deliberate answer.
//
// op must be one self-contained store call that carries no state forward
// from a failed attempt: it re-reads whatever it needs and re-executes its
// statement from scratch, exactly as it would on a first call (the guard
// statements among this module's writes re-evaluate their WHERE clauses at
// execution time, so a retried guarded write acts on the row's CURRENT
// state, not on a stale read). Every attempt is a whole repository call,
// never a fragment of one.
//
// withConflictRetry is this module's one retry envelope, and its call sites
// are the module's answer to which calls retry, deliberately not an
// enumeration a doc comment keeps in step: grep this module's non-test
// files for withConflictRetry( (or withInsertConflictRetry() to see every
// wrapped call. Which calls are wrapped is the read and write statements
// of the client-visible sign-in and preferences surfaces -- the statements
// whose raw conflict currently surfaces as an internal error on those
// endpoints; a new store call on the same kind of request-serving path
// wraps itself the same way.
func withConflictRetry(op func() error) error {
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

// withInsertConflictRetry is withConflictRetry for a single-statement INSERT
// of a row whose primary key the calling operation minted itself (newID's
// application-generated UUID), which leaves one residue question the plain
// envelope cannot answer. The classifier is deliberately blind to WHEN a
// conflict struck (dbkit/retry.go's doc comment names that blindness and
// the obligation it puts on a retrying caller): after a conflict reported
// by the transaction's own Commit the earlier attempt's outcome is not
// knowable from the error, and while every classified conflict is expected
// to mean the attempt did not commit, an operation retried after one must
// be idempotent over its own residue rather than assumed residue-free.
//
// insert is that operation; rowExists answers "is the row there NOW". A
// duplicate-key refusal raised by a RETRY (attempt > 0) means the row's key
// -- freshly minted by this very call, so no other writer of it exists --
// collided with the earlier attempt's own write; rowExists then confirms
// the row is genuinely present and the retry reports completion. If it is
// not present, some other uniqueness constraint refused the insert and that
// error is returned unchanged. A duplicate on the FIRST attempt is no
// residue at all (nothing of this call ran before it) and is returned
// immediately, exactly as it always was.
func withInsertConflictRetry(insert func() error, rowExists func() (bool, error)) error {
	var err error
	for attempt := 0; attempt < txRetryBudget; attempt++ {
		err = insert()
		if err == nil {
			return nil
		}
		if attempt > 0 && errors.Is(err, gorm.ErrDuplicatedKey) {
			exists, verifyErr := rowExists()
			if verifyErr == nil && exists {
				return nil
			}
			return err
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
