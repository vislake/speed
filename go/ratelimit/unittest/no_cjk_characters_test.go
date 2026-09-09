package unittest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
)

// This suite lives in package unittest — this module's dedicated unit-test
// directory for unit-tier checks with no single source file as their target
// (the backend coding standard's testing-layout rule); a module-shape check
// is such a suite. It tests the module black-box, from outside package
// ratelimit: it guards the repository's Language Rule over this module's
// own tree, not any ratelimit symbol.
//
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
//
// The walk starts at the module root, not at this file's own directory:
// this suite lives in go/ratelimit/unittest/, one level below the module
// root, and the scan must cover the whole module tree. The root is found
// by walking up from this file to the directory holding go.mod.
func TestModuleFiles_ContainNoCJKCharacters(t *testing.T) {
	moduleRoot := moduleRootOf(t)
	walkErr := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, err error) error {
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
		t.Fatalf("filepath.WalkDir(%q) error = %v", moduleRoot, walkErr)
	}
}

// moduleRootOf walks upward from this test file's compiled location until
// it finds the directory holding the module's go.mod -- the module root
// whose tree the CJK scan must cover.
func moduleRootOf(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) did not report this file's own path")
	}
	dir, err := filepath.Abs(filepath.Dir(thisFile))
	if err != nil {
		t.Fatalf("resolve directory of %s: %v", thisFile, err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", filepath.Join(dir, "go.mod"), err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s: this test file must live inside a Go module", thisFile)
		}
		dir = parent
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
