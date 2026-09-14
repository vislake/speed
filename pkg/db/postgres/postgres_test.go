package postgres_test

import (
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vislake/speed/pkg/db"
	"github.com/vislake/speed/pkg/db/internal/pgtest"
	"github.com/vislake/speed/pkg/db/postgres"
)

// TestPostgresPutsDDLInsideATransaction holds this engine to the first
// admission condition the design sets for an implementation subpackage.
//
// A migration's execution and its record are written in one transaction, so
// that "executed but not recorded" cannot happen. That rests entirely on the
// engine rolling DDL back with the rest of the transaction. On an engine where
// it does not, the CREATE TABLE survives while the record rolls away, the next
// startup runs it again, and it fails on an object that already exists — one
// startup after the one that caused it.
func TestPostgresPutsDDLInsideATransaction(t *testing.T) {
	handle := open(t, pgtest.Acquire(t))

	rollback := errors.New("rolling this transaction back on purpose")
	err := handle.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`CREATE TABLE rolled_back (id INTEGER PRIMARY KEY)`).Error; err != nil {
			return err
		}
		if !tableExists(t, tx, "rolled_back") {
			// Without this the case would pass on an engine that
			// refused the statement outright, which is a different
			// thing from rolling it back.
			t.Error("the table is not there inside the transaction that created it")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("the transaction reported %v, and the rollback this case turns on did not happen", err)
	}
	if tableExists(t, handle, "rolled_back") {
		t.Error("the table created inside the rolled-back transaction is still there, so this engine " +
			"keeps DDL outside the transaction and a migration's execution and its record are not atomic")
	}
}

// TestConfigNamespaceIsThisImplementationsOwn pins the namespace apart from
// every other implementation's.
//
// The configuration manifest is collected from every registered module, this
// run's disabled ones included, so two implementations declaring input items on
// one path conflict the moment a host imports both. That conflict is raised
// during collection, before exclusivity is resolved, so the resolution that
// would have stood one of them down never gets to run: a host importing
// PostgreSQL and SQLite would fail to start whichever one it configured. An
// implementation that mounted its items on "db" would do exactly that, and this
// is the assertion that would go red.
func TestConfigNamespaceIsThisImplementationsOwn(t *testing.T) {
	if postgres.ConfigNamespace != "db.postgres" {
		t.Errorf("the config namespace is %q, and the design names it db.postgres", postgres.ConfigNamespace)
	}
	if postgres.ConfigNamespace != postgres.ModuleName {
		t.Errorf("the config namespace %q and the module name %q differ, and the design has a module's "+
			"name and its configuration section be the same", postgres.ConfigNamespace, postgres.ModuleName)
	}
	prefix, own, found := strings.Cut(postgres.ConfigNamespace, ".")
	if prefix != "db" || !found || own == "" {
		t.Errorf("the config namespace %q is not a section of its own under db, so a second "+
			"implementation's items would land on the same paths", postgres.ConfigNamespace)
	}
}

// TestDialectIsPostgres pins the dialect this subpackage binds. The value is
// also the migration subdirectory name, so it has to stay a single path
// element: a value carrying a separator would send fs.Sub looking down a
// nested path and every migration declared for this engine would go unapplied
// without a word.
func TestDialectIsPostgres(t *testing.T) {
	if postgres.Dialect != db.Postgres {
		t.Errorf("this subpackage binds the dialect %q, and the capability names it %q",
			postgres.Dialect, db.Postgres)
	}
	if strings.ContainsAny(string(postgres.Dialect), "/\\.") {
		t.Errorf("the dialect %q is not a single path element, so it cannot name a migration "+
			"subdirectory", postgres.Dialect)
	}
}

// TestMigrationLockTimeoutIsDeclaredWithADefault holds this implementation to
// the input item its dialect owes.
//
// An implementation that offers a cross-process mutex has to bound the wait,
// and 5 minutes is the design's default. An implementation that offers no mutex
// declares no such item: there would be nothing to wait for, and a dial that
// does nothing is worse than no dial.
func TestMigrationLockTimeoutIsDeclaredWithADefault(t *testing.T) {
	if postgres.MigrationLockTimeoutKey != "migration-lock-timeout" {
		t.Errorf("the lock timeout item is keyed %q, and the design names it migration-lock-timeout",
			postgres.MigrationLockTimeoutKey)
	}
	if postgres.DefaultMigrationLockTimeout.String() != "5m0s" {
		t.Errorf("the lock timeout defaults to %s, and the design gives 5 minutes",
			postgres.DefaultMigrationLockTimeout)
	}
}

// TestPostgresDoesNotImportTheSQLiteDriver is the manual gate on the boundary
// between the two implementations. Nothing in the compiler enforces it: both
// subpackages live in one Go module, so an import of the other engine's driver
// builds cleanly and lands that driver in every host that deploys on this one.
func TestPostgresDoesNotImportTheSQLiteDriver(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	var sawOwnDriver bool
	for _, line := range deps {
		pkg := strings.TrimSpace(line)
		switch {
		case pkg == "gorm.io/driver/postgres":
			sawOwnDriver = true
		case strings.Contains(pkg, "sqlite"):
			t.Errorf("the PostgreSQL implementation depends on %s, so a host deploying on PostgreSQL "+
				"carries the SQLite driver too", pkg)
		case strings.HasPrefix(pkg, "gorm.io/driver/") && pkg != "gorm.io/driver/postgres":
			t.Errorf("the PostgreSQL implementation depends on %s, which is another engine's driver", pkg)
		}
	}
	if !sawOwnDriver {
		// Without this the case would pass on an empty listing, which is
		// what a mistyped argument or a failed load produces.
		t.Errorf("the dependency listing does not contain this engine's own driver, so it did not load "+
			"the package and the assertions above checked nothing (%d lines)", len(deps))
	}
}

// open opens a handle on a database this package's fixtures handed back, using
// the dialector this subpackage exports, and closes it when the test ends.
//
// The GORM logger is discarded: several cases fail a statement on purpose, and
// the default logger prints each one as an error line that reads like a test
// failure.
func open(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	handle, err := gorm.Open(postgres.Dialector(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("opening the PostgreSQL fixture: %v", err)
	}
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the fixture's connection pool: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the fixture: %v", err)
		}
	})
	return handle
}

// tableExists reports whether a table is visible to the handle.
func tableExists(t *testing.T, handle *gorm.DB, table string) bool {
	t.Helper()
	var name sql.NullString
	if err := handle.Raw(`SELECT to_regclass(?)::text`, table).Scan(&name).Error; err != nil {
		t.Fatalf("asking whether the table %q exists: %v", table, err)
	}
	return name.Valid
}
