// Package dbsuite hands the migration tests one database per dialect, so the
// same cases run against every engine an implementation subpackage supports.
//
// It does not import pkg/db. The migration engine's entry point is unexported,
// so the cases that drive it live in pkg/db's own test package, and a package
// those tests import cannot import pkg/db back. That is why a fixture names its
// dialect with a plain string rather than with db.Dialect; the value is the
// same one, and it is also the migration subdirectory name.
//
// A dialect that needs more than a file on disk contributes its fixture from
// its own file here through Contribute, rather than by editing this one.
package dbsuite

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Fixture is one dialect's test environment.
type Fixture struct {
	// Dialect is the dialect's value, which is also the name of the
	// subdirectory a migration set keeps that engine's files in.
	Dialect string
	// Open hands back a handle on a database with nothing in it. Every call
	// gets its own database: the cases assert on which tables exist and on
	// what the record table holds, and neither survives sharing.
	Open func(t *testing.T) *gorm.DB
}

// contributed holds the fixtures other files in this package add.
var contributed []func(t *testing.T) (Fixture, bool)

// Contribute registers a fixture, normally from an init function. The second
// result reports whether the dialect can run on this machine; a fixture that
// needs a container reports false when there is none.
func Contribute(fixture func(t *testing.T) (Fixture, bool)) {
	contributed = append(contributed, fixture)
}

// Fixtures gives every dialect available on this machine. SQLite is always
// there: it needs a temporary file and nothing else.
func Fixtures(t *testing.T) []Fixture {
	t.Helper()
	out := []Fixture{{Dialect: "sqlite", Open: OpenSQLite}}
	for _, contribute := range contributed {
		if fixture, available := contribute(t); available {
			out = append(out, fixture)
		}
	}
	return out
}

// OpenSQLite opens a handle on an empty SQLite database in a temporary
// directory, and closes it when the test ends.
//
// It is a file rather than an in-memory database because an in-memory one
// belongs to a single connection, and the pool would hand different cases
// different databases under the same handle.
//
// The pool is held to one connection. SQLite takes a write lock on the whole
// file, so a second connection reaching a migration's transaction would report
// a locked database instead of what the case is about.
//
// The GORM logger is discarded: several cases fail a migration on purpose, and
// the default logger would print each one as an error line that reads like a
// test failure. Nothing is lost, because the error itself travels back through
// the call and the cases assert on it.
func OpenSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migrations.db")
	handle, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("opening the SQLite fixture at %s: %v", path, err)
	}
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the SQLite fixture's connection pool: %v", err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the SQLite fixture: %v", err)
		}
	})
	return handle
}
