// Package testutil carries the shared fixtures go/app's own tests build
// applications from: the migration set a fake module ships, so the engine's
// migration stage has real SQL to apply.
//
// It is test-support code in its own package for the same reason every
// module's migration set is: a //go:embed directive's patterns resolve
// relative to the directory of the .go file carrying them, so the embedding
// file must live where "sqlite" is its own immediate child -- the layout
// dbkit.MigrationRegistry.Apply expects. The package is under internal/ and
// is imported by tests alone.
package testutil

import "embed"

// Migrations embeds the fixtures' postgres/*.sql and sqlite/*.sql migration
// files, in the layout every pkgcore.Module's Migrations() must expose.
//
//go:embed sqlite
var Migrations embed.FS
