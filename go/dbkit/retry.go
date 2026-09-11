package dbkit

import "strings"

// IsRetryableConflict reports whether err is a transient, contention-only
// database failure that a caller structuring a multi-statement, atomic
// operation as a bounded retry loop should retry rather than surface as a
// real failure: on SQLite, a write that could not proceed because another
// connection holds the file's write lock right now (SQLITE_BUSY, "database
// is locked" -- including the read-then-write lock-upgrade refusal, which
// the ordinary busy_timeout wait never resolves); on PostgreSQL, a detected
// deadlock or a serialization failure surfaced by concurrent row-level
// locking. Both are
// the database correctly refusing to let two overlapping writers proceed at
// once rather than a real bug, and the correct response on both dialects is
// the same: retry the whole transaction from a fresh read, since the
// transaction that lost the race is exactly as valid as one that had simply
// started a little later.
//
// # What this classifier does NOT distinguish: when the conflict struck
//
// The substring match is against the driver's wording for the failure,
// never its timing, and the two timings of a SQLITE_BUSY carry different
// retry premises. A BUSY can surface from a statement early in the
// transaction -- pre-commit: nothing of the attempt has taken effect, and
// retrying from a fresh read is unconditionally safe -- or from the
// transaction's own Commit call -- commit-time: the attempt's outcome is
// unknown, because database/sql has already marked the transaction done
// and, per WithTenantSession's own doc comment, fn's writes may be durably
// committed, still visible on the connection that attempted them, or
// rolled back. Both timings match this classifier (the wording is the
// same either way), and both should be retried -- a commit-time conflict
// is as transient as a pre-commit one -- but after a commit-time
// classification the retrying caller may NOT assume the first attempt
// recorded nothing. Discharging that assumption is the caller's
// obligation, stated in full on WithTenantSession's own doc comment: an
// operation retried after a conflict must be idempotent over its own
// residue, or must re-verify the row state its earlier attempt may already
// have changed. A classifier that could tell the timings apart would not
// remove that obligation -- at commit time the outcome is genuinely
// unknowable -- but naming the blindness here is what keeps a retrying
// caller from reading "retryable" as "the first attempt had no effect".
//
// # Why this is a dbkit function, and why it is a string match
//
// A caller with such an operation cannot classify these errors itself
// without importing the concrete driver package whose error type carries the
// answer (github.com/glebarez/sqlite / modernc.org/sqlite, or
// gorm.io/driver/postgres / github.com/jackc/pgx) -- which is exactly the
// boundary business code respects: it may depend on dbkit, never on the SQL
// driver dbkit itself resolves per dialect.
//
// dbkit could satisfy that by asserting against both drivers' own error
// types instead of matching strings, but doing so from this package's own
// root would reintroduce, for every dbkit consumer regardless of which
// single dialect they actually use, the exact dependency this package's
// dialect/sqlite and dialect/postgres split exists to avoid -- a
// PostgreSQL-only consumer would gain a transitive import of
// modernc.org/sqlite (and vice
// versa) purely so this one predicate could do a type assertion. A substring
// match against err.Error() needs neither import and keeps that split
// intact.
//
// The substrings below are each driver's own stable wording, not a guess:
// modernc.org/sqlite's *Error.Error() always includes the literal
// result-code name in parentheses (confirmed against a real SQLITE_BUSY in
// retry_sqlite_test.go, forced from two genuine connections contending on
// one file exactly as go/dbkit/dialect/sqlite/dialect_sqlite_test.go's own
// rig does), and PostgreSQL's wire-protocol error text for the
// serialization_failure (40001) and deadlock_detected (40P01) SQLSTATEs is
// standard, driver-independent server-generated text (confirmed against a
// real deadlock in integration_test/postgres_retry_test.go). Neither
// dialect's message text is expected to change across the pinned driver
// versions this module's go.mod carries, and a change that did would only
// widen the miss (a real conflict stops being retried and surfaces instead),
// never silently misclassify an unrelated failure as retryable.
func IsRetryableConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range retryableConflictSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryableConflictSubstrings is the fixed set IsRetryableConflict matches
// against. See that function's own doc comment for what each one proves and
// against which real driver.
var retryableConflictSubstrings = []string{
	// modernc.org/sqlite's SQLITE_BUSY, in both the plain contending-writer
	// shape and the read-then-write lock-upgrade refusal shape -- both
	// render this exact parenthesized code name.
	"SQLITE_BUSY",
	// The human-readable half of the same SQLite error, kept as a second,
	// independent match in case a future driver version ever renders the
	// code name differently but keeps this wording (both are matched, not
	// substituted, so either alone still triggers retry).
	"database is locked",
	// PostgreSQL SQLSTATE 40P01: two transactions each hold a lock the
	// other one is waiting for. PostgreSQL breaks the cycle itself by
	// aborting one side with this message; the aborted side is exactly as
	// valid as its rival and should simply run again.
	"deadlock detected",
	// PostgreSQL SQLSTATE 40001: a serializable (or, for some statement
	// shapes, read committed) transaction was aborted because a concurrent
	// transaction it overlapped with committed first. The standard
	// PostgreSQL wording always includes this phrase.
	"could not serialize access",
}
