// Package migrations embeds the rbac module's versioned SQL migration
// files, one subdirectory per SQL dialect, for rbac.Module's Migrations()
// method.
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside module.go, because a //go:embed directive's patterns are
// resolved relative to the directory of the .go file that carries it. For
// Migrations to expose "postgres" and "sqlite" at the root of its
// embed.FS -- the layout dbkit.MigrationRegistry.Apply expects from every
// module -- the embedding file has to live in the one directory where
// those two names are its own immediate children. This mirrors
// go/config/migrations and examples/reference-app/internal/notes/
// migrations; see go/dbkit/AGENTS.md's "Migrations" section.
package migrations

import "embed"

// FS embeds the rbac module's postgres/*.sql and sqlite/*.sql migration
// files.
//
//go:embed postgres sqlite
var FS embed.FS
