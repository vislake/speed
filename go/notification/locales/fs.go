// Package locales holds the notification module's message bundle: one
// <language>.toml file per language the catalog serves, embedded for
// Kernel.Bootstrap to feed to i18n.Builder.AddModule alongside every other
// module's Locales() embed.FS.
//
// What this bundle will carry is the human description of every error code
// go/notification/errors.go declares, one flat entry per code, with the id
// equal to the code itself. It is empty today because errors.go declares no
// codes yet -- the module's error vocabulary ships with the producers that
// raise it, in this round's later blocks -- and a locale entry without a
// producer is dead text, exactly as a code without an entry is a blank
// message.
//
// The two files must carry identical id sets. i18n.Builder.AddModule rejects
// a module whose languages disagree (ErrParityMismatch) while the kernel
// merges the catalog, and tools/check_i18n_keys.py checks the same parity
// over the raw files in CI. There is no cross-language fallback anywhere in
// the stack: a key missing from the loaded language surfaces as an error,
// not as the other language's text.
package locales

import "embed"

// FS holds notification's locale files, flat at the embed root: exactly
// zh-CN.toml and en-US.toml.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
