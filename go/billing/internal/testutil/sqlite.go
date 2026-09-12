package testutil

import (
	"testing"

	"gorm.io/gorm"
)

// PinSingleConnection caps db's pool at one physical connection, for test
// hosts whose assertions do not depend on how many connections carried
// their statements.
//
// On one SQLite file, a multi-connection pool makes SQLITE_BUSY reachable
// under scheduling load: SQLite serializes writers, and a losing statement
// waits only inside its bounded busy_timeout -- a total budget with no
// fairness guarantee, so under contention a statement can expire it while
// the file is otherwise making progress (a saturated runner's slow commits
// widen the windows, and reads contend with writers' commit windows too,
// not only write-vs-write). One connection moves every wait into
// database/sql's own pool queue, an ordinary FIFO with no timeout, so no
// lock error can surface from the host's own statements. The concurrency a
// suite drives is unaffected: goroutines still race for the pool and their
// statements still interleave in varying orders, just never two physical
// connections deep -- the mechanism under test does not care how many
// connections carried it.
func PinSingleConnection(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("testutil: reaching the database's pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
}
