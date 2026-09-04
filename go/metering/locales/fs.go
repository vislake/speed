// Package locales holds the metering module's message bundle: one
// <language>.toml file per language the catalog serves, embedded for
// Kernel.Bootstrap to feed to i18n.Builder.AddModule alongside every other
// module's Locales() embed.FS.
//
// What this bundle carries today is the human description of every error
// code go/metering/errors.go declares, one flat entry per code, with the
// id equal to the code itself -- the same convention org, storage,
// notification and pki's own locale bundles follow.
//
// The two files must carry identical id sets. i18n.Builder.AddModule rejects
// a module whose languages disagree (ErrParityMismatch) while the kernel
// merges the catalog, and tools/check_i18n_keys.py checks the same parity
// over the raw files in CI. There is no cross-language fallback anywhere in
// the stack: a key missing from the loaded language surfaces as an error,
// not as the other language's text.
package locales

import "embed"

// FS holds metering's locale files, flat at the embed root: exactly
// zh-CN.toml and en-US.toml.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
