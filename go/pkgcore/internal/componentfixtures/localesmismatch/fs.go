// Package localesmismatch embeds the parity-violating locale fixture pair
// the component assembly tests require to be refused: fr-FR is missing one
// of de-DE's message ids, the break the catalog's key-set parity contract
// forbids.
package localesmismatch

import "embed"

// FS embeds the mismatched de-DE.toml and fr-FR.toml fixture pair.
//
//go:embed de-DE.toml fr-FR.toml
var FS embed.FS
