package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vislake/speed/pkg/db"
)

// migrationLockKey is the advisory lock key the whole migration run is held
// under. It is the FNV-1a 64-bit hash of "speed:db:migrations", written out so
// that it cannot drift: every replica has to compute the same number, and a
// number derived at run time from anything that varies between builds would
// leave two replicas holding two different locks and neither of them waiting.
//
// Its scope is one database, because PostgreSQL scopes advisory locks that way.
// Every process on a database shares this mutex, and processes on other
// databases of the same cluster are left alone.
//
// The key is fixed rather than derived from the assembly, and the price is a
// queue: two unrelated deployments sharing one database, separated by schema
// say, wait for each other. A migration run is a startup-time one-off, so that
// wait is paid once. Deriving the key from session state — the schema search
// path is the obvious candidate — would remove the queue and put in its place a
// mutex whose reach depends on how a connection happens to be configured, and
// two replicas configured differently would not exclude each other at all. Of
// the two, the queue is the one to live with.
const migrationLockKey int64 = 3798569497438843947

// lockPollInterval is how often a waiting replica asks again.
//
// The wait is a poll of pg_try_advisory_lock rather than a blocking
// pg_advisory_lock because the blocking form has no deadline of its own: it
// waits until the other side gives up, and a replica stuck holding the mutex
// would hang every other replica for as long as it lives, which is the
// situation migration-lock-timeout exists to end.
const lockPollInterval = 250 * time.Millisecond

// releaseTimeout bounds the unlock statement. It is short because the unlock is
// a single round trip against a server this connection is already talking to.
const releaseTimeout = 10 * time.Second

// NewMigrationLock returns the cross-process mutex a migration run on this
// engine is held under, giving up after timeout.
//
// What it defends against never appears in one process. Several replicas start
// at once against an empty database, each reads the record table and finds
// nothing applied, each runs the same CREATE TABLE, and every replica after the
// first fails on an object that already exists. Under the mutex they are a
// queue: the second one waits, and by the time it looks, the record table is
// complete and it has nothing to do.
//
// It costs no connection of its own. The run pins one connection for its whole
// length and hands it to both halves of this mutex, which is exactly what a
// session-scoped advisory lock needs: it is taken outside any transaction, it
// stays held across every BEGIN and COMMIT the run issues on that connection,
// and it is given up on the same one at the end. A migration run therefore
// fits in a pool of one connection.
func NewMigrationLock(timeout time.Duration) db.MigrationLock {
	return &migrationLock{timeout: timeout}
}

// migrationLock holds the advisory lock on the connection the run pinned.
//
// It keeps no connection of its own, and it has to keep none: an advisory lock
// belongs to the session that took it, so the one connection that may take it
// is the one carrying the statements it orders. A lock on a second connection
// would be a second session — it would not see the migrations it is meant to
// exclude, and the pool would have to be large enough to hold both.
type migrationLock struct {
	timeout time.Duration
	// held records whether this process took the lock. Only the lifecycle's
	// single goroutine touches it: the migration run is driven from there,
	// before anything else in the process works.
	held bool
}

// Acquire takes the mutex on the run's connection, waiting for whichever
// replica holds it.
func (l *migrationLock) Acquire(ctx context.Context, conn *sql.Conn) error {
	if l.held {
		return errors.New("postgres: the migration mutex is already held by this process")
	}
	deadline := time.Now().Add(l.timeout)
	for {
		var taken bool
		if err := conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&taken); err != nil {
			return fmt.Errorf("postgres: asking for the migration mutex failed: %w", err)
		}
		if taken {
			l.held = true
			return nil
		}
		if !time.Now().Before(deadline) {
			return l.timedOut(ctx, conn)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("postgres: waiting for the migration mutex was cut short: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// timedOut builds the error a replica that waited its whole allowance reports.
//
// It is ErrMigrationLockTimeout and nothing else. The response differs from
// every other migration failure: what has to be looked at is the replica
// holding the mutex, not a migration file, and folding it into
// ErrMigrationFailed would send the host to the wrong place.
func (l *migrationLock) timedOut(ctx context.Context, conn *sql.Conn) error {
	// The database name locates the contention for whoever reads this. It is
	// asked for rather than taken from the locator because the locator
	// carries credentials and this text reaches the startup diagnostics.
	database := "the configured database"
	var name string
	if err := conn.QueryRowContext(ctx, "SELECT current_database()").Scan(&name); err == nil && name != "" {
		database = fmt.Sprintf("database %q", name)
	}
	return fmt.Errorf("%w: another process held the PostgreSQL migration mutex on %s for the whole %s of "+
		"%s.%s. That replica is still applying migrations, or it stopped part-way while holding the mutex; "+
		"find out what it is doing before raising the limit",
		db.ErrMigrationLockTimeout, database, l.timeout, ConfigNamespace, MigrationLockTimeoutKey)
}

// Release gives the mutex up on the connection it was taken on, on the failing
// path as well as the succeeding one: a migration that would not apply must not
// strand the other replicas behind a mutex nobody is going to release.
//
// What becomes of the connection afterwards is the run's business. A release
// that did not go through leaves the mutex on this session, and the run throws
// the connection away rather than hand a session in that state back to the
// pool.
func (l *migrationLock) Release(ctx context.Context, conn *sql.Conn) error {
	if !l.held {
		return nil
	}
	l.held = false
	// The unlock is issued on a context detached from the caller's. The
	// failing path arrives here with the run's context often already
	// cancelled, and an unlock skipped for that reason would leave the mutex
	// held: the lock belongs to this session, and handing the connection
	// back to the pool ends no session. The other replicas would wait out
	// their whole allowance against a process that is not doing anything.
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	var released bool
	if err := conn.QueryRowContext(unlockCtx,
		"SELECT pg_advisory_unlock($1)", migrationLockKey).Scan(&released); err != nil {
		return fmt.Errorf("postgres: releasing the migration mutex failed: %w", err)
	}
	if !released {
		return errors.New(
			"postgres: the migration mutex was not held by this connection when it was given up, so " +
				"another replica may have been applying migrations alongside this one")
	}
	return nil
}
