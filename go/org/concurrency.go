package org

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// txRetryBudget bounds how many times a transaction-shaped write that
// runs through withRetry (below) retries its whole transaction after a
// transient, contention-only failure: SQLite's SQLITE_BUSY, or
// PostgreSQL's detected deadlock or serialization failure (see
// dbkit.IsRetryableConflict's own doc comment for exactly what each of those
// is and why dbkit, not org, is what recognizes them). Neither is data
// corruption -- both are the database correctly refusing to let two
// overlapping writers proceed at once -- and the correct response on both
// dialects is the same: retry from a clean read, since the attempt that lost
// the race is exactly as valid as one that had simply started a little
// later.
//
// 5 is generous for the shape of the contention the retried writes actually
// produce (two callers touching the same node, or two callers whose moved
// subtrees genuinely overlap -- see tree.go's Move doc comment), not a
// number tuned against a measured production workload. Exhausting the
// budget reports ErrConcurrentUpdate rather than looping forever, so a
// caller under truly pathological contention gets a clear, coded answer
// instead of an HTTP request that hangs.
const txRetryBudget = 5

// txRetryBackoff is the fixed pause between retry attempts. It exists only
// to give the winner of a real conflict a moment to actually commit before
// the loser tries again -- retrying with no pause at all would mostly just
// reproduce the identical conflict immediately. It is deliberately small and
// fixed, not exponential: these operations are synchronous, request-serving
// calls, not a background job with room for a real backoff curve, and
// txRetryBudget's small attempt count means the worst case (every attempt
// conflicts) adds at most a few milliseconds, not seconds.
const txRetryBackoff = 5 * time.Millisecond

// withRetry runs op -- expected to open exactly one fresh
// dbkit.WithTenantSession transaction per call and read whatever it needs
// from scratch inside it, never carrying state forward from a failed
// attempt -- up to txRetryBudget times, retrying only when op's error is one
// dbkit.IsRetryableConflict classifies as transient contention. Any other
// error is returned immediately, unretried; exhausting the budget on a
// retryable error returns ErrConcurrentUpdate instead of the raw, dialect-
// specific conflict error, so a caller never has to know what SQLITE_BUSY or
// a PostgreSQL SQLSTATE means.
//
// withRetry is this module's one retry envelope, and the module's answer to
// "which operations retry" is its call sites, deliberately not an
// enumeration a doc comment would have to keep in step: every
// transaction-shaped write that must re-run from a clean read on
// contention wraps itself here, and grepping this module's non-test files
// for withRetry( is exact. A new operation of that shape wraps its own
// transaction here. MemberService.Remove and InviteService.Invite are the
// two standing exceptions: each rewrite is a single, database-arbitrated
// transaction whose first statement takes every lock it will ever hold,
// the shape that cannot produce the
// overlapping-lock-order deadlock this envelope exists to retry.
func withRetry(op func() error) error {
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
	return ErrConcurrentUpdate.WithCause(err)
}
