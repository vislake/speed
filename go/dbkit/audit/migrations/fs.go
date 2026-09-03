// Package migrations embeds the audit package's versioned SQL migration
// files, one subdirectory per SQL dialect, for the audit persister
// module's Migrations() method (that module lands alongside Emit and the
// GORM write-capture plugin, in this same round -- see the parent
// package's doc.go for the current split).
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside a sibling .go file, because a //go:embed directive's
// patterns are resolved relative to the directory of the .go file that
// carries it. For FS to expose "postgres" and "sqlite" at the root of its
// embed.FS -- the layout dbkit.MigrationRegistry.Apply expects from every
// module -- the embedding file has to live in the one directory where
// those two names are its own immediate children. This mirrors
// go/config/migrations and examples/reference-app/internal/notes/
// migrations; see go/dbkit/AGENTS.md's "Migrations" section.
package migrations

import "embed"

// FS embeds the audit package's postgres/*.sql and sqlite/*.sql migration
// files.
//
//go:embed postgres sqlite
var FS embed.FS
