// Package migrationfixture embeds the SQL migration fixture dbtest's own
// tests apply: one small, self-contained module set with no dependencies,
// carrying one file per dialect.
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside a _test.go file, because a //go:embed directive's patterns
// are resolved relative to the directory of the .go file that carries it:
// for Migrations to expose "postgres" and "sqlite" at the root of its
// embed.FS -- the layout every pkgcore.Module's Migrations() is expected to
// have, and the layout dbkit.MigrationRegistry.Apply reads -- the embedding
// file has to live in the one directory where those two names are its own
// immediate children. (go/dbkit/internal/migrationfixture's basemodule and
// derivedmodule exist for the same structural reason, as the registry
// suite's own fixtures.)
package migrationfixture

import "embed"

// Migrations embeds this fixture's postgres/*.sql and sqlite/*.sql files,
// ready to be wrapped in a dbtest.Migration.
//
//go:embed postgres sqlite
var Migrations embed.FS
