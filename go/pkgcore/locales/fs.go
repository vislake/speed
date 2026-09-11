// Package locales is pkgcore's own message bundle: the locale files it
// embeds (zh-CN.toml and en-US.toml, one file per language the catalog
// serves), kept as a small canonical seed set -- one message per shape the
// catalog supports -- phrased as real sentences about pkgcore itself so the
// translations are genuine rather than lorem ipsum.
//
// # What these files are NOT
//
// Nothing in a running application reads this bundle. the assembly
// feeds the merged message catalog exclusively from the REGISTERED modules'
// Locales() embed.FS (registry.go: "each module's locale resources are
// validated and merged before the module itself registers"), and pkgcore is
// not a Module: it is the dependency floor the Module contract lives on,
// with no Locales() of its own, so its files have no seat in the feeding
// loop and never join a bootstrapped catalog. If pkgcore's seed messages
// are ever missing from a catalog, the cause is not Bootstrap's feeding
// loop (which feeds every registered module correctly) but this directory
// having no wiring into any module's Locales() -- do not look in
// the assembly for it.
//
// # What these files are FOR
//
// They are pkgcore's own test fixture and the canonical shape example.
// pkgcore's suites feed them through exactly the seams a real module's
// bundle travels: i18n's catalog tests call Builder.AddModule("pkgcore",
// locales.FS), and registry_test.go's localeBundleModule ships them through
// a bootstrapped kernel's module loop, so the machinery is exercised by
// real embedded files and the seed messages double as the shape every
// module's own bundle follows. That exercise happens in pkgcore's tests,
// not in production -- a production catalog is exercised by the modules
// that actually render content, which is the point of those modules'
// bundles.
//
// The seed messages are not pkgcore's own user-facing content: pkgcore
// renders no messages of its own. A real message pkgcore ever needs to
// render belongs in the bundle of the module that renders it, not here --
// joining this directory is not enough, since nothing reads it; the seed
// entries exist only for as long as they earn their keep as fixtures.
//
// # The file contract
//
// Every other module ships its own locale files, one <language>.toml per
// language, in its own module directory, following the flat
// one-entry-per-message contract documented in go/pkgcore/i18n.
package locales

import "embed"

// FS holds pkgcore's locale files, flat at the embed root: exactly
// zh-CN.toml and en-US.toml. It exists for pkgcore's own test suites and as
// the shape example this doc and i18n/doc.go point at -- nothing in a
// bootstrapped application feeds it, since pkgcore is not a Module and only
// registered modules' Locales() bundles reach the catalog.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
