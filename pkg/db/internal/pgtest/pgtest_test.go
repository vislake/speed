package pgtest_test

import (
	"database/sql"
	"strings"
	"testing"

	pgdriver "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vislake/speed/pkg/db/internal/pgtest"
)

// TestEachAcquisitionGetsAnEmptyDatabase holds this package to the isolation
// every case built on it assumes.
//
// The migration cases assert on which tables exist and on what the record table
// holds. make check runs the tests twice, once with the race detector, and go
// test runs packages in parallel, so a shared database would carry one run's
// tables into the next — and the cross-process case needs a database on which
// nothing has been applied at all, or the replica that should apply the
// migration finds the work already done and the case passes without testing
// anything.
func TestEachAcquisitionGetsAnEmptyDatabase(t *testing.T) {
	first, second := pgtest.Acquire(t), pgtest.Acquire(t)
	if first == second {
		t.Fatalf("two acquisitions handed back the same locator %q, so the cases share a database", first)
	}

	firstHandle, secondHandle := open(t, first), open(t, second)
	if err := firstHandle.Exec(`CREATE TABLE only_in_the_first (id INTEGER)`).Error; err != nil {
		t.Fatalf("creating a table on the first database: %v", err)
	}
	if tableExists(t, secondHandle, "only_in_the_first") {
		t.Error("a table created on the first database is visible on the second, so the two are one database")
	}
	if !tableExists(t, firstHandle, "only_in_the_first") {
		t.Error("the table is not visible on the database it was created on, so the check above proves nothing")
	}
}

// TestAcquiredDatabaseNamesCarryTheOwningProcess pins the naming this package's
// housekeeping depends on: a run killed part-way leaves its databases on the
// shared container, and the owning process id in the name is how a later run
// tells the leftovers of a dead process from the databases a live one is using
// right now.
func TestAcquiredDatabaseNamesCarryTheOwningProcess(t *testing.T) {
	dsn := pgtest.Acquire(t)
	_, database, found := strings.Cut(strings.TrimSuffix(dsn, "?sslmode=disable"), "127.0.0.1:")
	if !found {
		t.Fatalf("the locator %q does not have the shape this assertion reads", dsn)
	}
	_, name, found := strings.Cut(database, "/")
	if !found {
		t.Fatalf("the locator %q carries no database name", dsn)
	}
	if !strings.HasPrefix(name, "pgtest_p") || !strings.Contains(name, "_n") {
		t.Errorf("the database name %q does not carry the owning process id, so a later run cannot tell "+
			"a dead run's leftovers from a live run's databases", name)
	}
}

// open opens a handle on a locator this package handed back.
func open(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	handle, err := gorm.Open(pgdriver.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("opening %q: %v", dsn, err)
	}
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the connection pool of %q: %v", dsn, err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the handle on %q: %v", dsn, err)
		}
	})
	return handle
}

// tableExists reports whether a table is in the handle's database.
func tableExists(t *testing.T, handle *gorm.DB, table string) bool {
	t.Helper()
	var name sql.NullString
	err := handle.Raw(`SELECT to_regclass(?)::text`, table).Scan(&name).Error
	if err != nil {
		t.Fatalf("asking whether the table %q exists: %v", table, err)
	}
	return name.Valid
}
