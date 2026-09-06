// Package migrations embeds this package's versioned SQL migration files for
// EnsureSchema (see the parent package's schema.go) and for any host that
// would rather apply them through its own dbkit.MigrationRegistry wiring
// instead.
//
// It carries only a "postgres" subdirectory, never a "sqlite" one. Every
// other module's migrations package in this codebase embeds both, following
// dbkit.MigrationRegistry.Apply's dual-dialect expectation -- but that
// expectation is about business-module tables, which the standalone
// deployment mode must run on SQLite. This mechanism has no standalone-mode
// existence at all: pkgcore.NewMemoryKVStore is what standalone mode runs,
// with no database of its own, so there is no dialect for a "sqlite"
// migration to target. Precedent: go/pkgcore/eventbus/postgres/migrations
// carries the identical shape for the identical reason (a PostgreSQL-only
// mechanism ships PostgreSQL-only migrations), and go/pki's signer/vault and
// signer/kmsaws subpackages ship no migrations directory whatsoever for the
// same underlying reason -- a missing SQLite counterpart is the correct
// shape here, not a gap. dbkit.MigrationRegistry.Apply itself only ever
// reads the one subdirectory matching the Dialect it was called with, so a
// host that never calls Apply(..., dbkit.DialectSQLite) against this FS
// never notices "sqlite" is absent; one who did gets an honest
// directory-not-found failure naming exactly what is missing, rather than a
// silently-empty no-op migration.
//
// This is its own tiny leaf package, rather than a var declared alongside
// the rest of the package's code, because a //go:embed directive's patterns
// resolve relative to the directory of the .go file carrying it -- see
// go/dbkit/audit/migrations/fs.go's identical reasoning, which this file
// mirrors line for line.
package migrations

import "embed"

// FS embeds this package's postgres/*.sql migration files.
//
//go:embed postgres
var FS embed.FS
