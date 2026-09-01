package testutil

import (
	"embed"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// widgetSQLiteMigration embeds the SQLite dialect of Widget's migration, so
// NewTestSQLite never depends on a working directory or a path relative to
// the test binary.
//
//go:embed migrations/sqlite/0001_create_widgets.sql
var widgetSQLiteMigration embed.FS

// testDBSeq makes every NewTestSQLite call use a distinct in-memory
// database name, even when a single test calls it more than once.
var testDBSeq atomic.Uint64

// NewTestSQLite opens a private in-memory SQLite database, applies the
// Widget migration, and returns a ready-to-use *gorm.DB.
//
// Every call gets its own database, so two calls never share state, whether
// they come from the same test, from parallel tests, or from subtests; the
// connection pool is pinned to a single connection so SQLite's shared
// in-memory cache is never torn down between queries (a second pooled
// connection to a fresh "cache=shared" in-memory database only sees the
// first connection's tables while a connection to it stays open
// continuously).
//
// This is a plain gorm.Open, not yet routed through dbkit.Open, which does
// not exist at this point in dbkit's build-out. Callers that need dbkit's
// own wiring — the tenant-scoping plugin, the encryption serializer, and so
// on — can layer it on top of the *gorm.DB returned here, or open their own
// connection directly once dbkit.Open exists.
func NewTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()

	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", name, testDBSeq.Add(1))

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("testutil: open in-memory sqlite: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("testutil: get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	migration, err := widgetSQLiteMigration.ReadFile("migrations/sqlite/0001_create_widgets.sql")
	if err != nil {
		t.Fatalf("testutil: read widget migration: %v", err)
	}
	if err := db.Exec(string(migration)).Error; err != nil {
		t.Fatalf("testutil: apply widget migration: %v", err)
	}

	return db
}
