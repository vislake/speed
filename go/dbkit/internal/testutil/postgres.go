package testutil

import (
	"embed"
	"testing"
)

// widgetPostgresMigration embeds the PostgreSQL dialect of Widget's
// migration, mirroring widgetSQLiteMigration in sqlite.go. There is no
// NewTestPostgres alongside NewTestSQLite here: opening a real PostgreSQL
// server needs a running instance (a testcontainer, in practice), which is
// exactly the kind of dependency internal/testutil is not allowed to pull
// in — it is imported by dbkit's plain, no-build-tag unit tests too (see
// this package's own doc comment), and those must stay hermetic and fast.
// So this file only exposes the migration SQL text; callers that need it
// against a real PostgreSQL connection — in particular the integration_test
// tier — apply it themselves against their own *gorm.DB or *sql.DB.
//
//go:embed migrations/postgres/0001_create_widgets.sql
var widgetPostgresMigration embed.FS

// WidgetPostgresMigrationSQL returns the raw SQL that creates the widgets
// table under the PostgreSQL dialect (migrations/postgres/0001_create_widgets.sql),
// the same fixture schema NewTestSQLite applies for SQLite. It fails t if
// the embedded file cannot be read, which would only happen if the file
// were deleted or renamed out from under this package.
func WidgetPostgresMigrationSQL(t *testing.T) string {
	t.Helper()
	migration, err := widgetPostgresMigration.ReadFile("migrations/postgres/0001_create_widgets.sql")
	if err != nil {
		t.Fatalf("testutil: read widget postgres migration: %v", err)
	}
	return string(migration)
}
