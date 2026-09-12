// Package migrations embeds the attestation layer's versioned SQL migration
// files, one subdirectory per SQL dialect, for the "attestation" module's
// Migrations declaration on the host's attestation component
// (internal/app/host_wiring.go).
//
// It exists as its own tiny leaf package, rather than as a var declared
// directly inside record.go, because a //go:embed directive's patterns are
// resolved relative to the directory of the .go file that carries it. For
// Migrations to expose "postgres" and "sqlite" at the root of its
// embed.FS -- the layout dbkit.ApplyMigrations expects from every
// migration-carrying component -- the embedding file has to live in the one
// directory where those two names are its own immediate children. This
// mirrors internal/notes's own migrations leaf package exactly.
package migrations

import "embed"

// FS embeds the attestation layer's postgres/*.sql and sqlite/*.sql
// migration files.
//
//go:embed postgres sqlite
var FS embed.FS
