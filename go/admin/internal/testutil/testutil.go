// Package testutil holds shared test helpers for go/admin's own test
// files -- a dedicated package rather than helpers scattered across test
// files.
package testutil

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"

	"github.com/vislake/speed/go/admin/migrations"
)

// NewDB returns a fresh, migrated SQLite database with every admin
// migration applied from zero.
//
// The migration set is named by hand rather than read from a module value:
// building the real admin.Module here would create an import cycle (this
// package is imported BY admin's own tests), which is exactly the case
// dbtest.Migration exists for.
func NewDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: "admin", FS: migrations.FS})
	return db
}
