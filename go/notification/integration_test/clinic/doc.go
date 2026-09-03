// Package clinic is the fixture module of go/notification's integration
// tier.
//
// The module exists to give the tier's Redis leg a declaring business
// module to deliver for: notification's own catalog (locales/) ships only
// error codes and verification-code templates, and the delivery pipeline
// renders from the host's merged catalog built from every module's bundles.
// The Redis leg therefore bootstraps this fixture alongside the real
// module, so the pipeline renders a real appointment-reminder type from
// real embedded locale files -- the only shape pkgcore.Module.Locales can
// return -- with the fixture's English copy kept byte-identical across the
// two language files (the tier asserts on rendered text, and this is a test
// fixture, not product copy).
//
// The package is loaded by the integration tier only:
// go test -tags=integration ./integration_test/... from the module
// directory. Without that tag every source file here is excluded, and this
// doc.go keeps the directory a valid (empty) package for the untagged
// unit-suite forms, the same exclusion pattern the tier's _test.go files
// rely on.
package clinic
