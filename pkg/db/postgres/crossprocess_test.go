package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vislake/speed/pkg/db/internal/pgtest"
	"github.com/vislake/speed/pkg/db/postgres"
)

// The scenario a re-executed copy of this test binary runs instead of the
// suite, and the inputs it takes.
//
// Two real processes are not a preference here. The failure the migration mutex
// exists to prevent only happens between processes: replicas of one deployment
// start at once, each finds the record table empty, and each runs the same
// CREATE TABLE. Goroutines in one process share a connection pool and a
// PostgreSQL session can hold an advisory lock re-entrantly, so a single
// process cannot reproduce any of it — an implementation with no mutex at all
// would pass a concurrent-goroutine case.
const (
	scenarioEnv           = "DB_POSTGRES_SCENARIO"
	scenarioDSNEnv        = "DB_POSTGRES_SCENARIO_DSN"
	scenarioRendezvousEnv = "DB_POSTGRES_SCENARIO_RENDEZVOUS"
	scenarioRoleEnv       = "DB_POSTGRES_SCENARIO_ROLE"

	// scenarioFirstMigration is one replica's first start against an empty
	// database.
	scenarioFirstMigration = "first-migration"
)

const (
	// scenarioReplicas is how many processes the scenario starts.
	scenarioReplicas = 2
	// scenarioApplyDuration is how long the applying replica stays inside
	// the migration's transaction. It is what gives the other replica time
	// to reach its own acquisition while the first is still working, which
	// is the whole of what this case needs to happen.
	scenarioApplyDuration = 3 * time.Second
	// scenarioLockTimeout is each replica's allowance. It is far above
	// scenarioApplyDuration: a replica giving up here would be this case
	// failing, not a timeout being exercised.
	scenarioLockTimeout = 60 * time.Second
	// scenarioRendezvousTimeout bounds the wait for the other replica to
	// arrive, so a replica that never started fails the run instead of
	// hanging it.
	scenarioRendezvousTimeout = 60 * time.Second
	// scenarioRunTimeout bounds one replica's whole run.
	scenarioRunTimeout = 3 * time.Minute
)

// The migration the scenario applies, and the record table it is recorded in.
//
// The statements are written out here rather than driven through the migration
// engine because the engine's entry point is unexported and takes a registry:
// reaching it would mean assembling one, and what this case is about is the
// mutex the engine takes at the two ends of that run, not the run itself. These
// are the same three statements the engine issues against an empty database.
const (
	probeModule       = "probe"
	probeFile         = "0001_widgets.sql"
	createRecordTable = `CREATE TABLE IF NOT EXISTS db_migrations (
	module TEXT NOT NULL,
	file TEXT NOT NULL,
	applied_at TIMESTAMP NOT NULL,
	PRIMARY KEY (module, file)
)`
	countRecord   = `SELECT count(*) FROM db_migrations WHERE module = $1 AND file = $2`
	insertRecord  = `INSERT INTO db_migrations (module, file, applied_at) VALUES ($1, $2, $3)`
	createWidgets = `CREATE TABLE widgets (id INTEGER PRIMARY KEY)`
)

// The markers a replica prints, each with the moment it reached that point.
const (
	markStart   = "start"
	markLocked  = "locked"
	markApplied = "applied"
	markNoop    = "noop"
)

// TestMain runs the scenario when the environment selects it, and the ordinary
// suite otherwise. A re-executed copy of this binary is how the case gets a
// second process without building a separate host command.
func TestMain(m *testing.M) {
	if scenario, chosen := os.LookupEnv(scenarioEnv); chosen {
		if err := runScenario(scenario); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestConcurrentFirstMigration is the hard gate on the cross-process mutex.
//
// Two processes meet on a rendezvous file and then go for the same empty
// database at the same moment. Exactly one has to apply the migration, and the
// other has to find the work already done — not fail on an object that already
// exists, which is what happens with no mutex and is the failure a real
// deployment hits on its first multi-replica start.
func TestConcurrentFirstMigration(t *testing.T) {
	dsn := pgtest.Acquire(t)
	rendezvous := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating this test binary to re-execute it: %v", err)
	}

	results := make([]replicaRun, scenarioReplicas)
	var running sync.WaitGroup
	for index := range results {
		running.Add(1)
		go func() {
			defer running.Done()
			results[index] = runReplica(binary, dsn, rendezvous, fmt.Sprintf("replica-%d", index))
		}()
	}
	running.Wait()

	for _, result := range results {
		if result.startErr != nil {
			t.Fatalf("%s could not be started: %v", result.role, result.startErr)
		}
		t.Logf("%s exited %d\nstdout:\n%s\nstderr:\n%s", result.role, result.exit, result.stdout, result.stderr)
	}

	t.Run("elects one applier", func(t *testing.T) {
		var appliers, noops []replicaRun
		for _, result := range results {
			if result.exit != 0 {
				t.Errorf("%s exited %d, and both replicas have to start successfully: %s",
					result.role, result.exit, result.stderr)
			}
			if strings.Contains(result.stderr, "already exists") {
				t.Errorf("%s failed on an object that already exists, which is the collision the "+
					"mutex is there to prevent: %s", result.role, result.stderr)
			}
			switch {
			case result.marked(markApplied):
				appliers = append(appliers, result)
			case result.marked(markNoop):
				noops = append(noops, result)
			}
		}
		if len(appliers) != 1 || len(noops) != 1 {
			t.Fatalf("%d replicas applied the migration and %d found it already applied, and exactly "+
				"one of each is what the mutex has to produce", len(appliers), len(noops))
		}
	})

	t.Run("proves the window overlapped", func(t *testing.T) {
		applier, noop := results[0], results[1]
		if noop.marked(markApplied) {
			applier, noop = noop, applier
		}
		if !applier.marked(markApplied) || !noop.marked(markNoop) {
			t.Skip("the replicas did not divide into one applier and one no-op, which the case above reports")
		}
		// Without this the gate is empty rather than merely weak. The
		// fixture only guarantees that the applier is slow; nothing
		// guarantees that the other replica was there while it was
		// working. A cold container, the second test leg, a slow exec —
		// any of them make the second replica late, and a late replica
		// reads a complete record table and prints noop whether there is
		// a mutex or not.
		startedWaiting, err := noop.at(markStart)
		if err != nil {
			t.Fatalf("reading when %s reached the mutex: %v", noop.role, err)
		}
		finishedApplying, err := applier.at(markApplied)
		if err != nil {
			t.Fatalf("reading when %s finished applying: %v", applier.role, err)
		}
		if !startedWaiting.Before(finishedApplying) {
			t.Fatalf("%s only reached the mutex at %s, after %s had finished applying at %s: the two "+
				"never overlapped, so this run proves nothing about the mutex",
				noop.role, startedWaiting.Format(time.RFC3339Nano),
				applier.role, finishedApplying.Format(time.RFC3339Nano))
		}
	})
}

// replicaRun is what one replica process did.
type replicaRun struct {
	role     string
	exit     int
	stdout   string
	stderr   string
	startErr error
}

// marked reports whether the replica printed a marker.
func (r replicaRun) marked(name string) bool {
	_, err := r.at(name)
	return err == nil
}

// at reads the moment the replica reached a marker.
func (r replicaRun) at(name string) (time.Time, error) {
	for _, line := range strings.Split(r.stdout, "\n") {
		marker, stamp, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || marker != name {
			continue
		}
		moment, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return time.Time{}, fmt.Errorf("the %q marker carries %q, which is not a timestamp: %w",
				name, stamp, err)
		}
		return moment, nil
	}
	return time.Time{}, fmt.Errorf("%s printed no %q marker", r.role, name)
}

// runReplica starts one replica as a separate process and collects what it did.
func runReplica(binary, dsn, rendezvous, role string) replicaRun {
	ctx, cancel := context.WithTimeout(context.Background(), scenarioRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(),
		scenarioEnv+"="+scenarioFirstMigration,
		scenarioDSNEnv+"="+dsn,
		scenarioRendezvousEnv+"="+rendezvous,
		scenarioRoleEnv+"="+role,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	run := replicaRun{role: role}
	err := cmd.Run()
	run.stdout, run.stderr = stdout.String(), stderr.String()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		run.exit = exit.ExitCode()
	default:
		run.startErr = err
	}
	return run
}

// runScenario is the body of a replica process.
func runScenario(scenario string) error {
	if scenario != scenarioFirstMigration {
		return fmt.Errorf("no scenario named %q", scenario)
	}
	return firstMigration(os.Getenv(scenarioDSNEnv), os.Getenv(scenarioRendezvousEnv), os.Getenv(scenarioRoleEnv))
}

// firstMigration is one replica starting against the shared database: meet the
// other replica, take the mutex, apply what is not applied, give the mutex up.
//
// One connection is borrowed and everything travels on it, which is what the
// migration run does. The pool is held to that one connection, so a replica
// that needed a second one for any part of this would stop here rather than
// pass quietly — the whole run has to fit in the smallest pool a host can
// configure.
func firstMigration(dsn, rendezvous, role string) error {
	handle, err := gorm.Open(postgres.Dialector(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	pool, err := handle.DB()
	if err != nil {
		return fmt.Errorf("reaching the connection pool: %w", err)
	}
	pool.SetMaxOpenConns(1)
	defer func() { _ = pool.Close() }()

	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("borrowing the connection to run on: %w", err)
	}
	defer func() { _ = conn.Close() }()

	lock := postgres.NewMigrationLock(scenarioLockTimeout)
	if err := meetOtherReplicas(rendezvous, role); err != nil {
		return err
	}

	mark(markStart)
	if err := lock.Acquire(ctx, conn); err != nil {
		return fmt.Errorf("taking the migration mutex: %w", err)
	}
	mark(markLocked)
	applyErr := applyFirstMigration(ctx, conn)
	releaseErr := lock.Release(ctx, conn)
	if applyErr != nil {
		return applyErr
	}
	if releaseErr != nil {
		return fmt.Errorf("giving the migration mutex up: %w", releaseErr)
	}
	return nil
}

// applyFirstMigration does what the migration engine does on a first start:
// create the record table, read what it lists, and apply and record what it
// does not. Every statement goes on the connection the mutex was taken on,
// which is what keeps the mutex held for the length of the run.
func applyFirstMigration(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, createRecordTable); err != nil {
		return fmt.Errorf("creating the migration record table: %w", err)
	}
	var applied int64
	if err := conn.QueryRowContext(ctx, countRecord, probeModule, probeFile).Scan(&applied); err != nil {
		return fmt.Errorf("reading the migration record table: %w", err)
	}
	if applied > 0 {
		mark(markNoop)
		return nil
	}
	err := inTransaction(ctx, conn, func(tx *sql.Tx) error {
		// The sleep is inside the transaction, so the window this
		// replica holds the mutex for is a window in which the record
		// table is still empty to anyone who could read it.
		if _, err := tx.ExecContext(ctx, `SELECT pg_sleep($1)`, scenarioApplyDuration.Seconds()); err != nil {
			return fmt.Errorf("widening the window: %w", err)
		}
		if _, err := tx.ExecContext(ctx, createWidgets); err != nil {
			return fmt.Errorf("executing it failed: %w", err)
		}
		if _, err := tx.ExecContext(ctx, insertRecord, probeModule, probeFile, time.Now().UTC()); err != nil {
			return fmt.Errorf("recording it failed: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("migration %q declared by module %q: %w", probeFile, probeModule, err)
	}
	mark(markApplied)
	return nil
}

// inTransaction runs body in one transaction on the borrowed connection, the
// way a migration's execution and its record are held together.
func inTransaction(ctx context.Context, conn *sql.Conn, body func(tx *sql.Tx) error) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning the transaction: %w", err)
	}
	if err := body(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// meetOtherReplicas blocks until every replica has reached this point, so that
// they go for the mutex together rather than one after the other.
func meetOtherReplicas(rendezvous, role string) error {
	if err := os.WriteFile(filepath.Join(rendezvous, "ready-"+role), nil, 0o600); err != nil {
		return fmt.Errorf("announcing this replica at the rendezvous: %w", err)
	}
	deadline := time.Now().Add(scenarioRendezvousTimeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(rendezvous)
		if err != nil {
			return fmt.Errorf("reading the rendezvous: %w", err)
		}
		arrived := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ready-") {
				arrived++
			}
		}
		if arrived >= scenarioReplicas {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("only some of the %d replicas reached the rendezvous within %s",
		scenarioReplicas, scenarioRendezvousTimeout)
}

// mark prints a marker and the moment this replica reached it. The timestamps
// are what the overlap assertion reads: without them a run where one replica
// arrived after the other had finished is indistinguishable from one where the
// mutex did its work.
func mark(name string) {
	fmt.Printf("%s\t%s\n", name, time.Now().UTC().Format(time.RFC3339Nano))
}
