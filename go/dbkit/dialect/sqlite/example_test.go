package sqlite_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
)

// Example demonstrates the blank-import-then-Open usage this package
// exists for: importing it purely for its init side effect registers
// dbkit.DialectSQLite with dbkit's dialect registry, after which
// dbkit.Open can build a SQLite connection.
func Example() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file::memory:?cache=shared",
	})
	if err != nil {
		fmt.Println("open failed:", err)
		return
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Println("db failed:", err)
		return
	}
	defer sqlDB.Close()

	fmt.Println("opened")
	// Output: opened
}
