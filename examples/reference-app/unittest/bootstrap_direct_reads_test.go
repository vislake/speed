// Package unittest carries this module's unit-tier tests that have no single
// target: shape checks over the module as a whole, of the kind the coding
// standards put in a module's dedicated unit-test directory.
package unittest

// bootstrap_direct_reads_test.go pins the bootstrap migration's core
// invariant: this app's executable code resolves its bootstrap surface through
// go/pkgcore/config's loader, never by reading the environment itself. The
// scan covers internal/app and cmd/server -- the two directories whose code
// resolves the surface -- so a new direct read (an os.Getenv, an os.LookupEnv,
// an os.Environ walk) fails here instead of quietly becoming a second,
// unchecked way for a process-start value to enter the app.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// scannedDirectories are the directories whose non-test files must not read
// the process environment directly.
var scannedDirectories = []string{
	"internal/app",
	"cmd/server",
}

// directReadFunctions are the stdlib entry points a direct environment read
// goes through.
var directReadFunctions = map[string]bool{
	"Getenv":    true,
	"LookupEnv": true,
	"Environ":   true,
}

func TestNoDirectEnvironmentReadsInExecutableCode(t *testing.T) {
	moduleDir := moduleRoot(t)

	var offenders []string
	for _, dir := range scannedDirectories {
		root := filepath.Join(moduleDir, dir)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			found, err := environmentReads(path)
			if err != nil {
				return err
			}
			offenders = append(offenders, found...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("executable code reads the process environment directly; the bootstrap surface is resolved by the loader (internal/app/bootstrap.go, whose target pins each variable):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// environmentReads reports every os.Getenv/os.LookupEnv/os.Environ call in one
// Go file, as "path:line" strings.
func environmentReads(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !directReadFunctions[selector.Sel.Name] {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		found = append(found, fset.Position(call.Pos()).String())
		return true
	})
	return found, nil
}

// moduleRoot returns the reference-app module's directory (the parent of this
// package's own directory, wherever the test binary runs from).
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module directory")
	}
	return filepath.Dir(filepath.Dir(thisFile))
}
