// Package locales embeds the well-formed locale fixture pair the component
// assembly tests validate: two languages, one identical message-id set,
// every id prefixed with the fixture component's name. The pair uses
// languages outside the zh-CN/en-US repository pair on purpose, so the
// fixture stays invisible to tools/check_i18n_keys.py discovery, which keys
// on those two file names.
package locales

import "embed"

// FS embeds the de-DE.toml and fr-FR.toml fixture pair.
//
//go:embed de-DE.toml fr-FR.toml
var FS embed.FS
