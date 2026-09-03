// Package template embeds the project skeleton `saasctl new` materializes
// into a new consumer project.
//
// The embedded tree under project/ mirrors the reference app's
// cmd/server shape -- package main under cmd/server/, one go.mod at the
// project root -- with every demo-specific piece removed: no notes module,
// no demo tenants, grants, membership store or subject resolver, no seeded
// data of any kind. A generated project is an honest skeleton whose host
// seams (authn's MembershipReader, org's SubjectResolver, the config
// resolver) are left unwired and fail closed per each module's contract,
// each named by a doc comment in the generated files as the owner's first
// task.
//
// Every embedded .go file starts with BuildIgnoreLine so the tree never
// compiles as part of this module (the materialized project is proven to
// build by real materialization, never by compiling the assets in place);
// `saasctl new` strips exactly that one line. The generated project is
// shared across every selection except for the two files a selection
// drives (docs/internal/02-repo-and-release.md's A5 adjudication: the
// go.mod require set and the server.go import/module/middleware set):
// project/.gitignore, project/README.md, project/cmd/server/main.go and
// project/cmd/server/config.go are shared verbatim, while
// project/selection/<key>/go.mod.txt and project/selection/<key>/server.go are
// chosen by <key>, SelectionKey's canonical rendering of the --with module
// set. The go.mod document is stored under the inert name go.mod.txt for a
// mechanical reason: go:embed's directory scan refuses to descend into a
// subdirectory that contains a file named go.mod -- such a directory reads
// as a nested module root -- so a go.mod named as such would silently
// vanish from the embedded tree. new renames go.mod.txt to go.mod when it
// materializes. config.go still parses the full uniform env surface of a generated
// project -- SPEED_CONFIG_KEY, SPEED_ORG_INDEX_KEY, SPEED_DB_PATH,
// SPEED_DEPLOYMENT_MODE, PORT -- so a consumer's bootstrap contract never
// changes with the selection; the files' doc comments say which envs the
// selected modules actually consume.
//
// Two tokens are substituted by new at materialization time, and must never
// appear in a materialized project: TokenAppName (the module path, derived
// from the target directory name, used verbatim in the go.mod module line
// and in the generated server's own name) and TokenSpeedRoot (the absolute
// path of the speed checkout the go.mod replace directives point at).
package template

import (
	"bytes"
	"embed"
	"fmt"
)

// BuildIgnoreLine is the mandatory first line of every embedded .go
// template file. new strips exactly this line when materializing; the
// embed_test.go invariants and new's own generator both refuse a template
// file that does not carry it, so a future template edit that forgets the
// line fails loudly in two places instead of silently compiling the
// skeleton into this module.
const BuildIgnoreLine = "//go:build ignore"

// TokenAppName is the placeholder for a generated project's module path,
// derived at `saasctl new` time from the target directory's base name and
// substituted wherever a materialized file names the project itself (see
// the package doc comment).
//
// #nosec G101 -- this is a materialization TOKEN marker, not a credential.
// gosec's heuristic fires on any string constant whose identifier contains
// "Token"; the names are public API pinned by this package's tests, and
// the values are inert placeholders that must never survive into a
// materialized project.
const TokenAppName = "__APP_NAME__"

// TokenSpeedRoot is the placeholder for the resolved speed checkout path,
// which a generated go.mod's replace directives point at.
//
// #nosec G101 -- a materialization token marker, for the reason above.
const TokenSpeedRoot = "__SPEED_ROOT__"

//go:embed all:project
var Project embed.FS

// ProjectRoot is the directory inside Project that holds the skeleton tree.
const ProjectRoot = "project"

// SharedFiles lists the embedded files that every selection materializes
// verbatim, keyed by their path inside ProjectRoot. The two .go files carry
// the BuildIgnoreLine like any template .go file; README.md and .gitignore
// are copied byte for byte.
var SharedFiles = []string{".gitignore", "README.md", "cmd/server/main.go", "cmd/server/config.go"}

// SelectionKey renders a validated --with module set as the embedded
// selection directory name: the modules sorted canonically and joined with
// '+', or "none" for the empty set. The caller validates the set first
// (SelectionKey itself does not check that the modules are known or that
// the closure rule holds -- it is a pure rendering of names).
func SelectionKey(with []string) string {
	if len(with) == 0 {
		return "none"
	}
	names := append([]string(nil), with...)
	sortStrings(names)
	key := ""
	for i, name := range names {
		if i > 0 {
			key += "+"
		}
		key += name
	}
	return key
}

// StripBuildIgnore removes BuildIgnoreLine plus the single blank line that
// follows it -- the exact layout this package's template convention
// requires (every template .go file opens with the marker line, then one
// blank line, then the package clause or its doc comment), asserted for
// every embedded file by embed_test.go's round-trip test. The returned
// file starts at the first real content line, with no leading blank line.
// new applies it to every embedded .go file at materialization time, and
// the error path doubles as new's generator guard: a template file that
// lost its marker fails here rather than silently compiling the skeleton
// into a generated project's build with a dangling build constraint.
func StripBuildIgnore(content []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(content, []byte(BuildIgnoreLine+"\n\n"))
	if !ok {
		return nil, fmt.Errorf("embedded file does not start with %q followed by a blank line", BuildIgnoreLine)
	}
	return rest, nil
}

// sortStrings sorts names in place.
func sortStrings(names []string) {
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
}
