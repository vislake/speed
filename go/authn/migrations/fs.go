// Package migrations embeds authn's versioned SQL migration files, one
// subdirectory per SQL dialect, for authn.Module's Migrations() method.
//
// It exists as its own leaf package, rather than as a var declared directly
// in module.go, because a //go:embed directive's patterns resolve relative
// to the directory of the .go file carrying it. For Migrations to expose
// "postgres" and "sqlite" at the root of its embed.FS -- the layout
// dbkit.MigrationRegistry.Apply expects from every module -- the embedding
// file has to live in the one directory where those two names are its own
// immediate children.
package migrations

import "embed"

// FS embeds authn's postgres/*.sql and sqlite/*.sql migration files.
//
//go:embed postgres sqlite
var FS embed.FS
