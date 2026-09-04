package postgres_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Example demonstrates the blank-import-then-Open usage this package
// exists for: importing it purely for its init side effect registers
// dbkit.DialectPostgres with dbkit's dialect registry, after which
// dbkit.Open builds and dials a PostgreSQL connection.
//
// This example points at a DSN with no real server behind it (a real
// PostgreSQL instance is exercised in dbkit/dbtest.NewPostgres and
// dbkit's own integration_test/ tier, neither of which this package's
// unit suite runs), so Open fails on the connection attempt -- but that
// failure is dbkit.connect_failed, never dbkit.invalid_dialect, which is
// exactly what proves this package's blank import registered the driver.
func Example() {
	ctx := context.Background()

	_, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectPostgres,
		DSN:     "postgres://speed:speed@127.0.0.1:1/nonexistent?sslmode=disable&connect_timeout=1",
	})

	appErr, ok := apperr.As(err)
	if !ok {
		fmt.Println("unexpected error type")
		return
	}

	fmt.Println(appErr.Code)
	// Output: dbkit.connect_failed
}
