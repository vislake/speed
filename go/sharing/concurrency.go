package sharing

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// txRetryBudget bounds how many times one of this module's guarded,
// database-arbitrated writes -- ShareRepository's tryRecordView,
// tryIncrementView and markRevoked -- retries after a transient,
// contention-only database failure: SQLite's SQLITE_BUSY, or PostgreSQL's
// detected deadlock or serialization failure.
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
// writes follow the identical shape, with the retry living around each
// attempt's one WithTenantSession. An attempt needs no re-reading on a
// retry: each statement's WHERE clause re-evaluates the row's state at
// execution time on every attempt, which is precisely what keeps a retried
// tryRecordView from double-counting (a view-count increment committed by
// someone else between attempts makes the retried attempt affect zero rows
// and report won == false -- and its granted log row is then not inserted,
// so no duplicate trail survives either -- exactly as a first attempt would
// have).
//
// # Why this module's own statements order themselves in-process on SQLite
//
// The retry above is what a guarded write does when it genuinely loses a
// race against ANOTHER connection -- one whose statements this process
// cannot see or order. Between this module's OWN statements over one
// SQLite file the retry is not the mechanism that keeps the outcome
// deterministic: SQLite serializes the file's writers, so a convoy of this
// module's own statements keeps the file's write lock held almost
// continuously, and a statement waiting on that convoy only ever gets
// scheduled to re-check the lock at instants the lock is held -- it can
// wait out dbkit's full busy_timeout attempt after attempt without ever
// winning. A READ is the sharper case: under SQLite's rollback-journal
// protocol a reader that requests its SHARED lock while a writer holds
// PENDING -- the writer's commit, waiting for the readers then standing to
// drain -- is refused into the busy handler, so an ordinary SELECT can
// burn the whole busy_timeout and surface SQLITE_BUSY even though nothing
// about the read itself was contended. sqliteGate below therefore orders
// every one of this module's own statements over one SQLite file: writes
// take its write side, reads its read side, so a write of this module
// never overlaps a read or a write of this module at all -- the file lock
// is taken in turn with no contention, a reader never meets a committing
// writer, and the only SQLITE_BUSY left for the retry above is the genuine
// one: a writer outside this module (another process on the same file).
//
// On PostgreSQL the gate is not taken (the repository's gate is nil):
// SQLite's single-writer premise -- the one thing the gate exists to make
// orderly -- does not exist there. PostgreSQL arbitrates concurrent
// writers with its own row locks, lets concurrent readers proceed
// untouched, and the bounded conflict retry above is the honest mechanism
// for the rare deadlock/serialization failure that genuine row-lock
// contention can still produce. An in-process gate around whole
// transactions would add nothing PostgreSQL needs and would actively cost
// isolation: one slow statement -- a blocked UPDATE, a slow network round
// trip -- would serialize every unrelated tenant behind it, and the expiry
// sweep would take the gate once per row across a huge tenant's backlog,
// interrupting unrelated tenants between every row. The gate is a
// SQLite-file-lock accommodation, not a module-wide policy, and it stays
// confined to the dialect whose file lock it exists for. (The database
// remains the arbiter across connections the gate cannot see even on
// SQLite -- other processes on the same file -- which is what the retry is
// for; the two mechanisms are complements, not substitutes.)
//
// 5 is generous for the contention the retry actually needs to absorb once
// the gate has removed this module's own statements from the SQLite
// file-lock lottery -- a genuine transient blip from a writer outside the
// gate, or a PostgreSQL deadlock/serialization failure -- not a number
// tuned against a measured production workload. Exhausting the budget is
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

// sqliteGate is the module-wide in-process ordering gate: one reader-writer
// mutex through which every database statement of this module's own
// repositories over one SQLite file passes -- each write takes the write
// side (lockWrite), each read the read side (lockRead) -- so this module's
// own statements never meet each other on the file's single-writer lock at
// all (the reasoning section above has the failure shapes that ordering
// removes). The gate is shared: NewService builds one gate and hands the
// same pointer to both of its repositories, because the share tables and
// the access-log table live in the same file, and a repository constructed
// on its own (NewShareRepository, NewAccessLogRepository) carries a gate of
// its own -- which is the whole of its file's access in that configuration.
//
// The zero value is ready to use, and a nil *sqliteGate is a deliberate,
// fully supported value: it is what a PostgreSQL-backed repository carries
// (newSQLiteGate returns nil for every dialect but SQLite), and every
// method is a no-op on it, so the non-SQLite path pays one nil check per
// statement and takes no lock at all.
type sqliteGate struct {
	mu sync.RWMutex
}

// newSQLiteGate returns the ordering gate for db's dialect: a ready gate
// when db speaks SQLite (the single-writer dialect the gate exists for),
// nil otherwise. db may be nil -- a Service constructed only for identity
// checks (NewModule(nil)) never performs I/O -- which is treated as "not
// SQLite": construction still succeeds, and any actual I/O on such a
// Service panics exactly as it always did.
func newSQLiteGate(db *gorm.DB) *sqliteGate {
	if db != nil && db.Name() == "sqlite" {
		return &sqliteGate{}
	}
	return nil
}

// lockRead and lockWrite take the gate's read and write sides;
// unlockRead/unlockWrite release them. A caller holds the read side for the
// duration of one read statement's transaction and the write side for the
// duration of one write statement's transaction -- including the commit,
// which is the point: a commit window left open past the release would put
// the next reader back into the busy handler the gate exists to keep it
// out of.
func (g *sqliteGate) lockRead() {
	if g != nil {
		g.mu.RLock()
	}
}

// unlockRead releases the read side taken by lockRead.
func (g *sqliteGate) unlockRead() {
	if g != nil {
		g.mu.RUnlock()
	}
}

// lockWrite takes the write side; unlockWrite releases it.
func (g *sqliteGate) lockWrite() {
	if g != nil {
		g.mu.Lock()
	}
}

// unlockWrite releases the write side taken by lockWrite.
func (g *sqliteGate) unlockWrite() {
	if g != nil {
		g.mu.Unlock()
	}
}

// runGuardedWrite executes one guarded, database-arbitrated
// single-statement write -- fn is expected to run its guarded statement(s),
// each with WHERE clauses that re-evaluate the row's state -- inside a
// fresh dbkit.WithTenantSession transaction per attempt, retried through
// withTxRetry exactly as org's atomic operations retry their whole
// transactions (see txRetryBudget's own doc comment for why the retry
// exists). Each attempt is additionally ordered through the repository's
// gate by taking its write side for the attempt's whole transaction
// (sqliteGate; the gate is nil on PostgreSQL, where no in-process lock is
// taken -- see the doc comment above for why the two dialects get different
// shapes). fn reports its outcome through captured variables (the attempt's
// RowsAffected-derived won flag, for example), exactly as org's withRetry
// closures communicate their transactions' results out.
func (r *ShareRepository) runGuardedWrite(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return withTxRetry(func() error {
		r.gate.lockWrite()
		defer r.gate.unlockWrite()
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
// (repository.go), the answer a conflict that outlasts the budget deserves
// -- the same answer that write gives any conflict.
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
