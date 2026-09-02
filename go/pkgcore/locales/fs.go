// Package locales is pkgcore's own message bundle: the locale files it
// embeds (zh-CN.toml and en-US.toml in M0, one file per language the
// catalog serves), embedded for Kernel.Bootstrap to feed to
// i18n.Builder.AddModule alongside every other module's Locales()
// embed.FS.
//
// # What these files honestly are
//
// pkgcore itself renders no user-facing content in M0 -- the consumers of
// the message catalog are modules that generate emails, invoices or webhook
// text, and none of those exist yet. What this bundle carries instead is a
// small canonical seed set, one message per shape the catalog supports, so
// the machinery is exercised by real embedded files from day one:
//
//   - pkgcore.seed.plain  -- a single-form message with no parameters;
//   - pkgcore.seed.params -- a parameterized message;
//   - pkgcore.seed.plural -- a plural message (one/other in en-US, the
//     single other form zh-CN needs).
//
// Each seed message is phrased as a real sentence about pkgcore itself, so
// the translations are genuine rather than lorem ipsum; when pkgcore gains a
// real rendered message it simply joins this set, and the seed entries can
// be dropped. Every other module ships its own locale files, one
// <language>.toml per language, in its own module directory, following the
// flat one-entry-per-message contract documented in go/pkgcore/i18n.
package locales

import "embed"

// FS holds pkgcore's locale files, flat at the embed root: exactly
// zh-CN.toml and en-US.toml.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
