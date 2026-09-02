package ratelimit

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestModuleFiles_ContainNoCJKCharacters is a regression guard for the root
// CLAUDE.md Language Rule ("read this first"): "docs/internal/** is written
// in Chinese... Everything else is English: code comments, godoc/TSDoc,
// module docs, per-module AGENTS.md files, ..." and "CI fails on CJK
// characters found outside docs/internal/". No such check is wired into CI
// yet -- .golangci.yml is staged but, per its own header comment, not yet
// invoked by Taskfile.yml's lint task -- so until it is, this module checks
// itself.
//
// It previously failed: doc.go's package comment and a comment in
// limiter.go both quoted a docs/internal/11-cross-cutting.md heading
// verbatim in Chinese instead of paraphrasing it in English the way every
// other real module's doc.go does (e.g. go/observability/doc.go's
// "must-instrument-metrics table" or go/tenancy/AGENTS.md's "data-domain
// table" -- an English description of the section, never the source
// heading's own characters), and AGENTS.md repeated the same pattern six
// more times.
//
// This walks every .go file's *comments* (via go/parser, never string
// literals or other tokens) and every Markdown file's full text under this
// module's own directory. Comments-only for Go files mirrors the scope
// go/dbkit's own TestCipher_EncryptDecrypt_RoundTrip documents for its
// intentional CJK test fixture ("string literals/test data are exempt from
// this repo's comments-and-docs-only CJK-language rule"); ratelimit has no
// such fixture to exempt today, but a future one would need the same
// carve-out added here rather than a blanket loosening of this test.
func TestModuleFiles_ContainNoCJKCharacters(t *testing.T) {
	walkErr := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".go"):
			checkGoCommentsForCJK(t, path)
		case strings.HasSuffix(path, ".md"):
			checkTextForCJK(t, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("filepath.WalkDir(\".\") error = %v", walkErr)
	}
}

// checkGoCommentsForCJK parses path and fails t for every comment
// containing a Han-script rune. It deliberately never looks at string
// literals or other non-comment tokens -- see this file's package-level
// doc comment for why.
func checkGoCommentsForCJK(t *testing.T, path string) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parser.ParseFile(%s) error = %v", path, err)
	}

	for _, group := range file.Comments {
		for _, comment := range group.List {
			if r, ok := firstHanRune(comment.Text); ok {
				pos := fset.Position(comment.Pos())
				t.Errorf("%s:%d: comment contains CJK character %q; root CLAUDE.md's Language Rule requires English outside docs/internal/ -- paraphrase in English or cite the docs/internal file path instead of quoting the Chinese heading", path, pos.Line, r)
			}
		}
	}
}

// checkTextForCJK fails t for every line of path containing a Han-script
// rune. Unlike checkGoCommentsForCJK, this checks the whole file, since a
// Markdown design/rationale file like AGENTS.md is entirely prose with no
// string-literal/test-data carve-out to preserve.
func checkTextForCJK(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) error = %v", path, err)
	}

	for i, line := range strings.Split(string(data), "\n") {
		if r, ok := firstHanRune(line); ok {
			t.Errorf("%s:%d: contains CJK character %q; root CLAUDE.md's Language Rule requires English outside docs/internal/", path, i+1, r)
		}
	}
}

// firstHanRune returns the first Han-script (CJK ideograph) rune in s, if
// any. It uses the standard library's own unicode.Han classification rather
// than a hand-maintained code-point range.
func firstHanRune(s string) (rune, bool) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return r, true
		}
	}
	return 0, false
}
