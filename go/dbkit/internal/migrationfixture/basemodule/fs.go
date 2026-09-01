// Package basemodule embeds the SQL migration fixtures for migrations_test.go's
// "base" fake pkgcore.Module: a small, self-contained module with no
// dependencies, used to exercise MigrationRegistry.Apply's per-module
// transaction, idempotency and filename-ordering behavior.
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside migrations_test.go, because a //go:embed directive's
// patterns are resolved relative to the directory of the .go file that
// carries it. For Migrations to expose "postgres" and "sqlite" at the root
// of its embed.FS -- the layout every real pkgcore.Module's Migrations() is
// expected to have, per the convention already established by
// internal/testutil's fixture -- the embedding file has to live in the one
// directory where those two names are its own immediate children.
package basemodule

import "embed"

// Migrations embeds base's postgres/*.sql and sqlite/*.sql fixture files.
//
//go:embed postgres sqlite
var Migrations embed.FS
