package dbtest

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// NewSQLite returns a *gorm.DB (via dbkit.Open) backed by a private,
// per-call temp-file SQLite database, for tests that don't need
// PostgreSQL-specific behavior. The database is automatically cleaned up
// via t.Cleanup.
//
// The database file lives under t.TempDir(), so every call — whether from
// the same test, from parallel tests, or from repeated calls within one
// test — gets its own file that can never collide with another's; t's own
// cleanup removes the directory (and the file in it) when the test ends.
// This intentionally differs from dbkit's own internal/testutil.NewTestSQLite,
// which uses a "cache=shared" in-memory database pinned to a single pooled
// connection: a real on-disk file has no equivalent shared-cache lifetime
// hazard, so it needs no such pinning and can safely use dbkit.Open's
// ordinary connection-pool defaults (open.go's defaultMaxOpenConns and
// friends) unmodified, exactly like production code would.
//
// NewSQLite applies no migration; see this package's doc comment for why,
// and for the pattern a caller uses to apply its own.
func NewSQLite(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), "dbtest.sqlite")

	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbtest: open temp-file sqlite database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return
		}
		_ = sqlDB.Close()
	})

	return db
}
