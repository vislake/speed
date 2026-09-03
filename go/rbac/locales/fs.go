// Package locales embeds rbac's zh-CN and en-US translation resources
// (root CLAUDE.md's internationalization rule; backend coding standard
// §12; docs/internal/11-cross-cutting.md), for rbac.Module's Locales()
// method.
//
// It exists as its own tiny leaf package for the same //go:embed
// directory-resolution reason as the sibling migrations package: the
// embedding file must live alongside the files it embeds.
package locales

import "embed"

// FS embeds rbac's zh-CN.toml and en-US.toml locale resources.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
