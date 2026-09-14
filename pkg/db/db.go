// Package db carries database access: the capability modules take up from each
// other, the resource types they declare migrations and plugins through, and
// the connection, plugin, encryption and migration logic the implementation
// subpackages share.
//
// This package registers no module. The implementations do: pkg/db/postgres and
// pkg/db/sqlite each register one, bind their own dialect's driver, and declare
// the same capability exclusively. A host imports the one it deploys on, or
// several, and the configuration decides which of them runs.
//
// The calling surface is GORM's *gorm.DB. This package wraps GORM rather than
// abstracting it, so query building, model mapping, association loading and
// transactions are GORM's own API and nothing here sits on top of them. GORM
// therefore reaches every dependant, which is what taking up a wrapping module
// costs.
package db

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Dialect names a database engine. The set is closed: each implementation
// subpackage fixes one value, and a new engine adds a value here alongside its
// subpackage.
//
// A value is also the name of the subdirectory a migration set keeps that
// engine's files in, so it has to stay a single path element.
type Dialect string

const (
	// Postgres is PostgreSQL, delivered by pkg/db/postgres.
	Postgres Dialect = "postgres"
	// SQLite is SQLite, delivered by pkg/db/sqlite.
	SQLite Dialect = "sqlite"
)

// Database is the capability this release unit delivers. It is an interface
// because a capability token's element type must be one, while *gorm.DB is a
// concrete type and cannot designate a capability; the handle comes out of it
// through DB.
//
// Declaring a dependency on it orders the dependant after the database module,
// and therefore after migrations have been applied.
//
// Take it up with core.Resolve[db.Database](reg), and declare the dependency as
// core.Requirement{Token: (*db.Database)(nil)}.
type Database interface {
	// DB returns the handle to the database the configuration names. Every
	// declared plugin and the encryption serializer are already installed on
	// it: the handle is delivered after assembly, never during it.
	DB() *gorm.DB
	// Dialect reports which engine is behind the handle. Callers writing
	// native SQL branch on it, because placeholder syntax and function names
	// differ between engines; GORM's query builder hides that difference and
	// statements written around it do not.
	Dialect() Dialect
	// BlindIndex returns the deterministic digest of a plaintext, for a
	// column that an equality query can match. A field encrypted with the
	// encrypted serializer cannot be queried by equality — the same
	// plaintext encrypts differently every time — so a model that has to be
	// found by such a field carries a second column holding this digest.
	//
	// Two obligations sit with the caller, and missing either one is silent:
	//
	// The blind index column is filled by the caller, not by this module. A
	// serializer acts on a single field and cannot write a second column, so
	// nothing here fills it. A row written without it is still written
	// correctly; only the equality query against that column finds nothing.
	// The failure shows up as a missing row, not as an error.
	//
	// The digest is computed over the string as given, with no normalisation
	// of any kind. Case, surrounding whitespace, the punctuation in a phone
	// number: whatever of that has to be ignored, the caller flattens the
	// same way when writing and when querying. Flattening one side only
	// yields a different digest, and again the query finds nothing rather
	// than failing.
	//
	// It is a method rather than a package-level function because it needs
	// the key, and the key arrives with the instance.
	BlindIndex(plaintext string) string
	// Open establishes an additional connection to another database, for a
	// read replica, another system's database or a one-off source. It is
	// assembled like the delivered handle — same dialect, same declared
	// plugins, same encryption serializer — and it shares the same connection
	// pool parameters.
	//
	// It carries no migrations and this module does not hold it: the caller
	// closes it with Close in its own Close stage. Leaking it leaks
	// connections, and nothing here can release them on the caller's behalf.
	Open(ctx context.Context, dsn string) (*gorm.DB, error)
}

// Close releases a handle's connection pool.
//
// It is a package-level function rather than a method on Database because
// closing a handle uses none of this module's state, and a handle from Open has
// to be closable by a caller that may not hold the instance.
//
// It is reentrant: closing an already closed handle reports no error, which is
// what lets a rollback path call it on an instance that never reached Init. A
// nil handle is likewise not an error, for the same reason — the rollback
// reaches instances that were never constructed.
func Close(handle *gorm.DB) error {
	if handle == nil {
		return nil
	}
	pool, err := handle.DB()
	if err != nil {
		return fmt.Errorf("db: the handle has no connection pool to close: %w", err)
	}
	if err := pool.Close(); err != nil {
		return fmt.Errorf("db: closing the connection pool failed: %w", err)
	}
	return nil
}
