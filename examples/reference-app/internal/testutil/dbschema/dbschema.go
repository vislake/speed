// Package dbschema carries the schema-introspection helpers this app's
// migration suites share: the case, smilesim and attestation migration
// packages each compare their upgrade path's schema against a from-zero
// migration, and one implementation keeps the three readings identical.
//
// It lives as its own leaf package under internal/testutil, rather than in
// internal/testutil itself, because a migrations package's test binary can
// only import a helper that does not reach back into the package whose
// migrations it exercises: internal/testutil imports internal/app/demo,
// which imports internal/smilesim, which imports its own migrations leaf --
// an import cycle for the smilesim migrations suite. This package imports
// nothing beyond the standard library and gorm, so every migration suite
// can use it.
package dbschema

import (
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// SchemaSnapshot returns db's schema as a stable, sorted list of one line
// per table and index -- "<type> <name>: <sql>" from SQLite's own
// sqlite_master catalog, the stored statement text with its whitespace
// collapsed so two databases that differ only in how their SQL was
// pretty-printed compare equal. It is the whole-shape reading the case,
// smilesim and attestation migration suites compare their upgrade path
// against: sqlite_master's stored SQL carries the column set, the
// nullability, the primary key and an index's uniqueness and column list
// alike, so two equal snapshots mean two databases are schema-identical,
// not merely same-tabled. SQLite's own internal objects (the sqlite_%
// catalog names, auto-indexes included) are left out: they are engine
// bookkeeping, not schema this app declares.
func SchemaSnapshot(t *testing.T, db *gorm.DB) []string {
	t.Helper()

	var rows []struct {
		Type string
		Name string
		SQL  string
	}
	err := db.Raw(`SELECT type, name, COALESCE(sql, '') AS sql
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`).Scan(&rows).Error
	if err != nil {
		t.Fatalf("read the sqlite_master schema snapshot: %v", err)
	}

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, row.Type+" "+row.Name+": "+strings.Join(strings.Fields(row.SQL), " "))
	}
	return lines
}

// AssertSameSchema fails t when two SchemaSnapshot readings differ, naming
// what was compared and showing both sides -- the migration suites' proof
// that a database moved by the migration set carries exactly the schema the
// other path produces.
func AssertSameSchema(t *testing.T, got, want []string, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: schemas differ\n got:\n  %s\n want:\n  %s",
			what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
