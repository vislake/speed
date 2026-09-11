// Package alpha is a synthetic locale-asset fixture for the reference app's
// host-catalog assembly test: a pair of locale resources whose ids carry the
// "alpha" prefix a component registered under that name must ship.
//
// It exists as its own tiny leaf package for the //go:embed
// directory-resolution reason the app's real locales packages document: the
// embedding file must live alongside the files it embeds, and the resource
// directory's files must sit flat at the embedded root.
package alpha

import "embed"

// FS embeds alpha's zh-CN.toml and en-US.toml locale resources.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
