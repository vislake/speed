// Package locales embeds authn's zh-CN and en-US translation resources, for
// authn.Module's Locales() method.
//
// It exists as its own leaf package for the same //go:embed
// directory-resolution reason as the sibling migrations package: the
// embedding file must live alongside the files it embeds.
package locales

import "embed"

// FS embeds authn's zh-CN.toml and en-US.toml locale resources.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
