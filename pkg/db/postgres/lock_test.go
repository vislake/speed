package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/db"
	"github.com/vislake/speed/pkg/db/internal/pgtest"
	"github.com/vislake/speed/pkg/db/postgres"
)

// TestLockTimeoutIsErrMigrationLockTimeout holds the bounded wait to its
// sentinel, and to the response that sentinel exists to direct.
//
// Nothing here is a race: one replica takes the mutex and keeps it, the other
// is given an allowance of a second, and the outcome is the same on every run.
func TestLockTimeoutIsErrMigrationLockTimeout(t *testing.T) {
	dsn := pgtest.Acquire(t)
	ctx := t.Context()

	holder := replica(t, dsn, time.Minute)
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("the first replica could not take the mutex: %v", err)
	}
	t.Cleanup(func() {
		if err := holder.Release(context.Background()); err != nil {
			t.Errorf("releasing the mutex the first replica held: %v", err)
		}
	})

	waiter := replica(t, dsn, time.Second)
	start := time.Now()
	err := waiter.Acquire(ctx)
	waited := time.Since(start)

	if !errors.Is(err, db.ErrMigrationLockTimeout) {
		t.Fatalf("waiting out the allowance reported %v, and the host classifies it with "+
			"ErrMigrationLockTimeout", err)
	}
	if errors.Is(err, db.ErrMigrationFailed) {
		// Both sentinels matching would send the host looking through the
		// migration files, when what it has to look at is the replica
		// holding the mutex.
		t.Error("the timeout also matches ErrMigrationFailed, which points the host at the migrations " +
			"instead of at the other replica")
	}
	if waited < time.Second {
		t.Errorf("the wait gave up after %s, short of the whole second it was allowed", waited)
	}
	text := err.Error()
	if item := postgres.ConfigNamespace + "." + postgres.MigrationLockTimeoutKey; !strings.Contains(text, item) {
		t.Errorf("the message does not name %s, which is the dial the reader may want to raise: %s", item, text)
	}
	if !strings.Contains(text, "replica") {
		t.Errorf("the message does not point at the other replica: %s", text)
	}
}

// TestTheMutexIsReleasedAfterARun checks the ordinary path: the replica that
// applied the migrations lets the next one in.
//
// The second replica is given a one-second allowance, so a mutex that was not
// given up fails this case rather than slowing it down.
func TestTheMutexIsReleasedAfterARun(t *testing.T) {
	dsn := pgtest.Acquire(t)
	ctx := t.Context()

	first := replica(t, dsn, time.Second)
	if err := first.Acquire(ctx); err != nil {
		t.Fatalf("the first replica could not take the mutex: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("the first replica could not give the mutex up: %v", err)
	}

	second := replica(t, dsn, time.Second)
	if err := second.Acquire(ctx); err != nil {
		t.Fatalf("the mutex was still held after the first replica released it: %v", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Errorf("the second replica could not give the mutex up: %v", err)
	}
}

// TestTheMutexIsReleasedAfterARunThatWasCutShort is the failing path, and it
// turns on the detail that path is easiest to get wrong.
//
// A migration run that failed arrives at the release with its context already
// cancelled. An unlock issued on that context does not reach the server, and
// handing the pinned connection back to the pool ends no session — so the lock
// stays held, and every other replica waits out its whole allowance against a
// process that has already given up. An implementation that passed the caller's
// context straight through fails here and nowhere else: the ordinary path above
// stays green.
func TestTheMutexIsReleasedAfterARunThatWasCutShort(t *testing.T) {
	dsn := pgtest.Acquire(t)
	ctx := t.Context()

	runCtx, abandon := context.WithCancel(ctx)
	first := replica(t, dsn, time.Second)
	if err := first.Acquire(runCtx); err != nil {
		t.Fatalf("the first replica could not take the mutex: %v", err)
	}
	abandon()

	if err := first.Release(runCtx); err != nil {
		t.Fatalf("giving the mutex up after the run was cut short: %v", err)
	}
	second := replica(t, dsn, time.Second)
	if err := second.Acquire(ctx); err != nil {
		t.Fatalf("a run that was cut short left the mutex held, so every other replica waits out its "+
			"whole allowance against a process that has already given up: %v", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Errorf("the second replica could not give the mutex up: %v", err)
	}
}

// TestReleasingAMutexWhoseSessionDiedReportsIt covers the one path on which
// the unlock cannot go through: the server dropped the session holding the
// lock, so there is nothing left to unlock.
//
// It has to be reported rather than swallowed. Silently returning nil would
// read, to everything upstream, exactly like a mutex that was given up
// normally, and the connection carrying that dead session must not go back into
// the pool for the next caller to pick up.
func TestReleasingAMutexWhoseSessionDiedReportsIt(t *testing.T) {
	dsn := pgtest.Acquire(t)
	ctx := t.Context()

	holder := replica(t, dsn, time.Second)
	if err := holder.Acquire(ctx); err != nil {
		t.Fatalf("taking the mutex: %v", err)
	}
	executioner := open(t, dsn)
	if err := executioner.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`).Error; err != nil {
		t.Fatalf("dropping the session holding the mutex: %v", err)
	}

	err := holder.Release(ctx)
	if err == nil {
		t.Error("giving up a mutex whose session is gone reported nothing, which upstream cannot tell " +
			"apart from a mutex that was released normally")
	} else if !strings.Contains(err.Error(), "migration mutex") {
		t.Errorf("the message does not say what failed: %v", err)
	}

	// The server released the lock when it dropped the session, so the next
	// replica gets in. This is here so that the case above cannot be read as
	// the mutex having been stranded.
	next := replica(t, dsn, time.Second)
	if err := next.Acquire(ctx); err != nil {
		t.Fatalf("the mutex was still held after its session was dropped: %v", err)
	}
	if err := next.Release(ctx); err != nil {
		t.Errorf("the next replica could not give the mutex up: %v", err)
	}
}

// TestReleasingAMutexThatWasNeverTakenIsNotAnError covers the path a startup
// that failed before the migration run takes. Acquire may never have run, and a
// release that objected to that would replace the real startup failure with
// noise about a mutex.
func TestReleasingAMutexThatWasNeverTakenIsNotAnError(t *testing.T) {
	lock := replica(t, pgtest.Acquire(t), time.Second)
	if err := lock.Release(t.Context()); err != nil {
		t.Errorf("releasing a mutex that was never taken reported %v", err)
	}
}

// TestTakingTheMutexTwiceIsRefused pins the one case a single process can get
// into on its own. The second acquisition would overwrite the pinned
// connection, leaving the first session holding the lock with nothing left that
// could give it up — a lock leaked for the lifetime of the process.
func TestTakingTheMutexTwiceIsRefused(t *testing.T) {
	ctx := t.Context()
	lock := replica(t, pgtest.Acquire(t), time.Second)
	if err := lock.Acquire(ctx); err != nil {
		t.Fatalf("taking the mutex: %v", err)
	}
	t.Cleanup(func() {
		if err := lock.Release(context.Background()); err != nil {
			t.Errorf("releasing the mutex: %v", err)
		}
	})
	if err := lock.Acquire(ctx); err == nil {
		t.Error("taking the mutex twice was allowed, which strands the first session's lock")
	}
}

// TestTwoDatabasesDoNotBlockEachOther pins the scope the key relies on.
//
// PostgreSQL scopes an advisory lock to a database, so one fixed key serves
// every deployment. Were it cluster-wide, two unrelated hosts on two databases
// of one server would queue behind each other's migrations, and the fixed key
// would have to become a derived one.
func TestTwoDatabasesDoNotBlockEachOther(t *testing.T) {
	ctx := t.Context()
	here := replica(t, pgtest.Acquire(t), time.Second)
	there := replica(t, pgtest.Acquire(t), time.Second)

	if err := here.Acquire(ctx); err != nil {
		t.Fatalf("taking the mutex on the first database: %v", err)
	}
	t.Cleanup(func() {
		if err := here.Release(context.Background()); err != nil {
			t.Errorf("releasing the mutex on the first database: %v", err)
		}
	})
	if err := there.Acquire(ctx); err != nil {
		t.Fatalf("a mutex held on one database blocked the mutex on another: %v", err)
	}
	if err := there.Release(ctx); err != nil {
		t.Errorf("releasing the mutex on the second database: %v", err)
	}
}

// replica hands back a migration mutex on a handle of its own, standing for one
// replica of a multi-replica deployment. Each one is a separate connection
// pool, which is what a separate process would be.
func replica(t *testing.T, dsn string, timeout time.Duration) db.MigrationLock {
	t.Helper()
	lock, err := postgres.NewMigrationLock(open(t, dsn), timeout)
	if err != nil {
		t.Fatalf("building a migration mutex: %v", err)
	}
	return lock
}
