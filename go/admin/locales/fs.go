// Package locales embeds admin's bilingual message bundles for
// admin.Module's Locales() method.
package locales

import "embed"

// FS embeds admin's zh-CN.toml and en-US.toml files.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
