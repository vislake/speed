// Package migrations embeds the ai-gateway module's versioned SQL migration
// files, one subdirectory per SQL dialect, for aigateway.Module's
// Migrations() method.
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside module.go, because a //go:embed directive's patterns are
// resolved relative to the directory of the .go file that carries it. For
// Migrations to expose "postgres" and "sqlite" at the root of its
// embed.FS -- the layout dbkit.MigrationRegistry.Apply expects from every
// module -- the embedding file has to live in the one directory where
// those two names are its own immediate children, mirroring
// go/config/migrations and go/pki/migrations.
package migrations

import "embed"

// FS embeds the ai-gateway module's postgres/*.sql and sqlite/*.sql
// migration files.
//
//go:embed postgres sqlite
var FS embed.FS
