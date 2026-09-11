// Package locales is pkgcore's own message bundle: the locale files it
// embeds (zh-CN.toml and en-US.toml, one file per language the catalog
// serves), kept as a small canonical seed set -- one message per shape the
// catalog supports -- phrased as real sentences about pkgcore itself so the
// translations are genuine rather than lorem ipsum.
//
// # What these files are NOT
//
// Nothing in a running application reads this bundle. The assembly
// feeds the merged message catalog exclusively from the SELECTED
// components' Locales embed.FS (component_assembly.go's catalog build hands
// each selected component's bundle to one i18n.Builder), and none of
// pkgcore's own built-in components carries a Locales bundle: pkgcore is
// the dependency floor, so its files have no seat in the feeding
// loop and never join the merged catalog. If pkgcore's seed messages
// are ever missing from a catalog, the cause is not the assembly's feeding
// loop (which feeds every selected component correctly) but this directory
// being wired into no component's Locales -- do not look in
// the assembly for it.
//
// # What these files are FOR
//
// They are pkgcore's own test fixture and the canonical shape example.
// pkgcore's suites feed them through exactly the seams a real module's
// bundle travels: i18n's catalog tests call Builder.AddModule("pkgcore",
// locales.FS), and the assembly's own tests carry them on component
// descriptors (component_assembly_test.go's locale carriers), so the
// machinery is exercised by real embedded files and the seed messages double
// as the shape every module's own bundle follows. That exercise happens in pkgcore's tests,
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
// bootstrapped application feeds it, since no pkgcore component carries a
// Locales bundle and only selected components' bundles reach the catalog.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
