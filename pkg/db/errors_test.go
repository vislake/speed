package db_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/db"
)

// sentinels is the table the design states, in the order it states it.
var sentinels = map[string]error{
	"ErrConnectFailed":        db.ErrConnectFailed,
	"ErrPluginFailed":         db.ErrPluginFailed,
	"ErrMigrationFailed":      db.ErrMigrationFailed,
	"ErrMigrationLockTimeout": db.ErrMigrationLockTimeout,
}

// TestSentinelsAreDistinct checks that the four classes are actually four. A
// sentinel defined in terms of another, or two names bound to one value, would
// collapse two classes a host is meant to tell apart — a locked migration and a
// broken one call for different responses.
func TestSentinelsAreDistinct(t *testing.T) {
	names := slices.Sorted(maps.Keys(sentinels))
	for _, a := range names {
		if sentinels[a] == nil {
			t.Errorf("sentinel %s is nil", a)
			continue
		}
		if sentinels[a].Error() == "" {
			t.Errorf("sentinel %s has an empty message", a)
		}
		for _, b := range names {
			if a == b {
				continue
			}
			if errors.Is(sentinels[a], sentinels[b]) {
				t.Errorf("errors.Is(%s, %s) is true, so a caller cannot tell the two classes apart", a, b)
			}
		}
	}
}

// TestSentinelTableHasNoFifthEntry reads the package's own source rather than
// the list above, so that a sentinel added to the package without a ruling
// behind it fails here instead of quietly widening the contract. Checking the
// list against itself would observe nothing.
//
// Run-time failures of encryption and decryption are the case this guards: they
// belong to the query that hit them, not to a startup classification table, and
// a sentinel for them is the shape to reject.
func TestSentinelTableHasNoFifthEntry(t *testing.T) {
	declared, err := exportedErrVars(".")
	if err != nil {
		t.Fatalf("reading the package source: %v", err)
	}
	want := slices.Sorted(maps.Keys(sentinels))
	if !slices.Equal(declared, want) {
		t.Errorf("the package declares sentinels %v, the design's table is %v", declared, want)
	}
}

// TestSentinelMessagesStateTheirSubject checks that each message is usable on
// its own in a startup diagnostic: prefixed with the package, and saying what
// failed rather than only naming a category.
func TestSentinelMessagesStateTheirSubject(t *testing.T) {
	for name, sentinel := range sentinels {
		msg := sentinel.Error()
		if !strings.HasPrefix(msg, "db: ") {
			t.Errorf("%s reads %q, which does not say which module it came from", name, msg)
		}
		if len(strings.Fields(msg)) < 4 {
			t.Errorf("%s reads %q, which names a category without saying what failed", name, msg)
		}
	}
}

// TestWrappingKeepsTheChain is the executable form of the "%w everywhere" rule.
// A %v leaves the text all but identical while making the sentinel unreachable,
// so the text is not evidence and errors.Is has to be the observation.
func TestWrappingKeepsTheChain(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:5432: connection refused")
	for name, sentinel := range sentinels {
		wrapped := fmt.Errorf("%w: opening postgres: %w", sentinel, cause)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("a wrap of %s does not match it with errors.Is", name)
		}
		if !errors.Is(wrapped, cause) {
			t.Errorf("a wrap of %s loses the underlying cause", name)
		}
	}
}

// exportedErrVars lists the exported package-level variables whose name starts
// with Err, across the package's non-test files.
func exportedErrVars(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range value.Names {
					if strings.HasPrefix(ident.Name, "Err") && ident.IsExported() {
						names = append(names, ident.Name)
					}
				}
			}
		}
	}
	slices.Sort(names)
	return names, nil
}

// TestCloseToleratesAHandleWithNothingToClose covers the two shapes Close meets
// on a rollback path, where it runs on whatever a failed startup left behind.
//
// A nil handle is the instance that was never constructed, and it is not an
// error: the rollback walks every module that entered construction, including
// ones whose New never returned. A handle with no connection pool behind it is
// an error, and the point of the assertion is that GORM's own reason survives
// the wrap — a %v here would read the same and lose it.
//
// Reentrancy over a live pool needs a driver and is pinned alongside the
// connection logic.
func TestCloseToleratesAHandleWithNothingToClose(t *testing.T) {
	if err := db.Close(nil); err != nil {
		t.Errorf("closing a nil handle reported %v, want nil: the rollback path reaches instances that were never constructed", err)
	}

	// A handle carrying no connection pool. gorm.DB embeds *gorm.Config, so
	// the Config has to be there for this to be a handle at all rather than a
	// struct literal gorm.Open never produces.
	err := db.Close(&gorm.DB{Config: &gorm.Config{}})
	if err == nil {
		t.Fatal("closing a handle with no connection pool reported no error")
	}
	if !errors.Is(err, gorm.ErrInvalidDB) {
		t.Errorf("closing a handle with no connection pool gave %v, which does not carry gorm.ErrInvalidDB", err)
	}
}
