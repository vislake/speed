package dbsuite

import (
	"os"
	"testing"

	pgdriver "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/vislake/speed/pkg/db/internal/pgtest"
)

// init contributes the PostgreSQL fixture, so that every dialect-neutral
// migration case runs against this engine too without any of them being
// touched.
//
// The fixture is unavailable on a machine with no docker, and the suite then
// runs on SQLite alone. Under CI that is a failure instead: a gate that is
// silently skipped wherever it is meant to run is not a gate, and the
// cross-process behaviour PostgreSQL carries has no other cover.
func init() {
	Contribute(func(t *testing.T) (Fixture, bool) {
		t.Helper()
		if reason := pgtest.Skip(); reason != "" {
			if _, underCI := os.LookupEnv("CI"); underCI {
				t.Fatalf("CI is set, so running the migration suite without PostgreSQL would leave "+
					"this engine unchecked: %s", reason)
			}
			t.Logf("the PostgreSQL fixture is unavailable, so the suite runs without it: %s", reason)
			return Fixture{}, false
		}
		return Fixture{Dialect: "postgres", Open: OpenPostgres}, true
	})
}

// OpenPostgres opens a handle on an empty PostgreSQL database, and closes it
// when the test ends.
//
// Every call gets a database of its own on the shared container. The cases
// assert on which tables exist and on what the record table holds, and neither
// survives being shared with the packages go test runs in parallel or with make
// check's second test leg.
//
// The GORM logger is discarded for the reason the SQLite fixture discards it:
// several cases fail a migration on purpose, and the default logger prints each
// one as an error line that reads like a test failure.
func OpenPostgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := pgtest.Acquire(t)
	handle, err := gorm.Open(pgdriver.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("opening the PostgreSQL fixture: %v", err)
	}
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the PostgreSQL fixture's connection pool: %v", err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the PostgreSQL fixture: %v", err)
		}
	})
	return handle
}
