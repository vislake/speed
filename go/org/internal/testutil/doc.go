// Package testutil holds the test helpers org's own test files share.
//
// It exists because Go's _test.go files are not importable across files in
// other packages, so anything more than one test file needs has to live in a
// regular .go file of its own package (backend coding standard, section 13).
// Being under internal/, it is reachable only from within the org module and
// never lands in a consumer's build.
//
// It deliberately does NOT import github.com/vislake/speed/go/org. org's own
// test files are in package org, so a helper package that imported org could
// not be imported back by them -- that is an import cycle, and it is why
// NewSQLite takes a module name and an embed.FS rather than an org.Module.
// go/dbkit/internal/testutil avoids the identical cycle the identical way.
package testutil
