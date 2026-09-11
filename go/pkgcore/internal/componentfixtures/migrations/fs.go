// Package migrations embeds the well-formed migration fixture set the
// component assembly tests validate: one dialect directory per supported
// dialect, each holding a numbered .sql file.
package migrations

import "embed"

// FS embeds the postgres/ and sqlite/ fixture migration sets.
//
//go:embed postgres sqlite
var FS embed.FS
