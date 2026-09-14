// Package http is the entry point module's interface package: the registration
// surface modules bind routes and middleware through, the resource type they
// declare their OpenAPI fragment as, and the request-handling helpers their
// handlers call.
//
// The surface is expressed in net/http's own types and in this package's
// types, and in nothing else. A routing engine is bound by an implementation
// subpackage such as http/stdmux, and its types never reach here, so a host
// that takes this package up carries no routing library in its dependency
// list.
//
// # Naming this package http
//
// This package is named http, which collides with the standard library's. The
// direction of the alias is fixed per kind of file, so that a reader never has
// to work out which http a file means:
//
//   - A production file or an internal test file, both in package http, import
//     the standard library as nethttp "net/http".
//   - An external test file in package http_test, and example_test.go with it,
//     go the other way: net/http keeps its own name and this package is
//     imported as speedhttp "github.com/vislake/speed/pkg/http".
//   - The implementation subpackages follow the external-test direction:
//     net/http keeps its name, the parent package takes the alias.
//   - A Go block in AGENTS.md carries a package clause and is compiled by
//     tools/check_markdown_examples.py, so it follows whichever of the rules
//     above its package clause selects.
package http
