// Package migrations embeds the admin module's versioned SQL migration
// files, one subdirectory per SQL dialect, for admin.Module's Migrations()
// method. See go/pki/migrations/fs.go's identical doc comment for why this
// lives in its own tiny leaf package rather than as a var declared
// directly inside module.go.
package migrations

import "embed"

// FS embeds admin's postgres/*.sql and sqlite/*.sql migration files.
//
//go:embed postgres sqlite
var FS embed.FS
