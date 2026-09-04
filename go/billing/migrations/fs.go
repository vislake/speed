// Package migrations embeds billing's versioned SQL migration files, one
// subdirectory per dialect (postgres/, sqlite/), for
// dbkit.MigrationRegistry to apply -- the same embed.FS contract every
// other module's migrations package follows.
package migrations

import "embed"

// FS holds every migration file under postgres/ and sqlite/.
//
//go:embed postgres sqlite
var FS embed.FS
