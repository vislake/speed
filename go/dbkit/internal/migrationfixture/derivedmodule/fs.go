// Package derivedmodule embeds the SQL migration fixtures for
// migrations_test.go's "derived" fake pkgcore.Module, which declares
// DependsOn: []string{"base"}. Its migrations deliberately write into a
// table basemodule's migrations own, so that MigrationRegistry.Apply
// applying "base" before "derived" is something a test can observe as a
// concrete side effect, not just infer from the absence of an error. See
// the comment on the "0002_seed_from_derived.sql" fixture files for exactly
// how.
//
// It exists as its own tiny leaf package for the same reason as
// basemodule: a //go:embed directive's patterns resolve relative to the
// directory of the .go file that carries it, so "postgres" and "sqlite"
// can only appear at the root of the resulting embed.FS if that file lives
// in the one directory where those two names are its own immediate
// children.
package derivedmodule

import "embed"

// Migrations embeds derived's postgres/*.sql and sqlite/*.sql fixture files.
//
//go:embed postgres sqlite
var Migrations embed.FS
