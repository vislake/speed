package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/db"
)

// migrationLockKey is the advisory lock key the whole migration run is held
// under. It is the FNV-1a 64-bit hash of "speed:db:migrations", written out so
// that it cannot drift: every replica has to compute the same number, and a
// number derived at run time from anything that varies between builds would
// leave two replicas holding two different locks and neither of them waiting.
//
// PostgreSQL scopes advisory locks to a database, so this one key serves every
// deployment: two hosts on two databases of one cluster do not block each
// other, and two replicas on one database do.
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

// NewMigrationLock returns the cross-process mutex the migration run against
// this handle's database is held under, giving up after timeout.
//
// What it defends against never appears in one process. Several replicas start
// at once against an empty database, each reads the record table and finds
// nothing applied, each runs the same CREATE TABLE, and every replica after the
// first fails on an object that already exists. Under the mutex they are a
// queue: the second one waits, and by the time it looks, the record table is
// complete and it has nothing to do.
func NewMigrationLock(handle *gorm.DB, timeout time.Duration) (db.MigrationLock, error) {
	pool, err := handle.DB()
	if err != nil {
		return nil, fmt.Errorf(
			"postgres: the handle has no connection pool to take the migration mutex on: %w", err)
	}
	return &migrationLock{pool: pool, timeout: timeout}, nil
}

// migrationLock holds the mutex on one pinned connection.
//
// The connection is pinned rather than taken from the pool per statement
// because a PostgreSQL advisory lock belongs to the session that took it. A
// second statement arriving on a different pooled connection is a different
// session, and would neither see the lock nor be able to give it up.
type migrationLock struct {
	pool    *sql.DB
	timeout time.Duration
	// conn is the session holding the lock, nil while it is not held. Only
	// the lifecycle's single goroutine touches it: the migration run is
	// driven from there, before anything else in the process works.
	conn *sql.Conn
}

// Acquire takes the mutex, waiting for whichever replica holds it.
func (l *migrationLock) Acquire(ctx context.Context) error {
	if l.conn != nil {
		return errors.New("postgres: the migration mutex is already held by this process")
	}
	conn, err := l.pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("postgres: pinning a connection for the migration mutex failed: %w", err)
	}
	deadline := time.Now().Add(l.timeout)
	for {
		var taken bool
		if err := conn.QueryRowContext(ctx,
			"SELECT pg_try_advisory_lock($1)", migrationLockKey).Scan(&taken); err != nil {
			return errors.Join(
				fmt.Errorf("postgres: asking for the migration mutex failed: %w", err), conn.Close())
		}
		if taken {
			l.conn = conn
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.Join(l.timedOut(ctx, conn), conn.Close())
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf(
				"postgres: waiting for the migration mutex was cut short: %w", ctx.Err()), conn.Close())
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

// Release gives the mutex up, on the failing path as well as the succeeding
// one: a migration that would not apply must not strand the other replicas
// behind a mutex nobody is going to release.
func (l *migrationLock) Release(ctx context.Context) error {
	if l.conn == nil {
		return nil
	}
	conn := l.conn
	l.conn = nil
	// The unlock is issued on a context detached from the caller's. The
	// failing path arrives here with the run's context often already
	// cancelled, and an unlock skipped for that reason would leave the mutex
	// held: the lock belongs to this session, and handing the connection
	// back to the pool ends no session. The other replicas would wait out
	// their whole allowance against a process that is not doing anything.
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRowContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockKey).Scan(&released)
	switch {
	case err != nil:
		return errors.Join(
			fmt.Errorf("postgres: releasing the migration mutex failed: %w", err), discard(conn))
	case !released:
		return errors.Join(errors.New(
			"postgres: the migration mutex was not held by this connection when it was given up, so another "+
				"replica may have been applying migrations alongside this one"), discard(conn))
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("postgres: returning the migration mutex's connection to the pool failed: %w", err)
	}
	return nil
}

// discard throws the pinned connection away instead of returning it to the
// pool.
//
// It is the fallback for an unlock that did not go through. The lock belongs to
// the session, so ending the session is the one remaining way to release it —
// returning the connection to the pool would keep the session alive, with the
// lock on it, and hand it out to the next caller in that state.
func discard(conn *sql.Conn) error {
	// Raw reports the error the callback returned; ErrBadConn is what asks
	// database/sql to drop the underlying connection rather than reuse it,
	// so seeing it back is the success case. ErrConnDone is the other
	// success case: database/sql has already thrown the connection away,
	// which is the same end — the session is over and the lock with it.
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("postgres: dropping the migration mutex's connection failed, and the mutex stays "+
			"held until the server drops that session: %w", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("postgres: closing the migration mutex's connection failed: %w", err)
	}
	return nil
}
