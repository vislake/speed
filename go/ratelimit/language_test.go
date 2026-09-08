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

// TestModuleFiles_ContainNoCJKCharacters is a regression guard for the
// repository's Language Rule: docs/internal/** is written in Chinese, and
// everything else -- code comments, godoc/TSDoc, module docs, per-module
// AGENTS.md files -- is English, with CI failing on CJK characters found
// outside docs/internal/. This module enforces that rule on itself by
// walking every .go file's comments (via go/parser, never string literals
// or other tokens) and every Markdown file's full text under this module's
// own directory. English commentary paraphrases a referenced section; it
// never quotes the source heading's own Chinese characters.
//
// Comments-only for Go files mirrors the carve-out dbkit's own CJK test
// fixture documents: string literals and test data are exempt from the
// comments-and-docs-only CJK-language rule. A fixture needing that
// exemption here would require the same carve-out added to this test
// rather than a blanket loosening of it.
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
