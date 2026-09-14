package db_test

import (
	"os/exec"
	"strings"
	"testing"
)

// repoPrefix is this repository's own module path. Every module of it is inside
// this package's budget: a module here is imported as a module of this
// repository, and none of them reaches a driver.
const repoPrefix = "github.com/vislake/speed/"

// TestRootPackageImportsNoDriver is the executable form of the boundary the
// packaging decision draws: the root package wraps GORM, and a dialect's driver
// enters through the implementation subpackage that binds it.
//
// Nothing holds that split at compile time. An implementation subpackage and the
// root package are one module, so the module's dependency list carries both
// drivers whichever engine a host deploys on, and the split is a rule about
// imports. This case is the gate that keeps it — the cost of having no compiler
// support here is recorded in ADR-module-packaging-2026-09-13.
//
// The observation is the root package's own dependency face: what a host
// compiles by importing this package, and no more. It is the non-test face,
// because `go list -deps .` reports exactly that: the cases in this package
// import a real driver to have an engine to run against, and an import a case
// makes is not what a host pays for. Moving one of those imports into a
// non-test file is the mistake this catches, and it is caught here.
//
// The budget is the standard library, this repository's modules, and GORM's own
// closure. Anything else came in through this package, and a driver is the shape
// that matters: it is the dependency a host would carry whichever engine it
// picked.
func TestRootPackageImportsNoDriver(t *testing.T) {
	root := dependenciesOf(t, ".")
	// The positive control. This package wraps GORM, so a listing that lost
	// gorm.io/gorm came from a load that did not happen, and every negative
	// assertion below would pass on an empty list.
	if !root["gorm.io/gorm"] {
		t.Fatalf("the root package's dependency list does not hold gorm.io/gorm, which is what this " +
			"package wraps: what is checked below is not this package's dependency face")
	}

	withinBudget := dependenciesOf(t, "gorm.io/gorm")
	for pkg := range root {
		switch {
		case stdlibPackage(pkg), strings.HasPrefix(pkg, repoPrefix), withinBudget[pkg]:
			continue
		}
		t.Errorf("the root package depends on %s. Wrapping GORM costs gorm.io/gorm and its own "+
			"closure, and nothing besides: a dialect driver belongs to the implementation subpackage "+
			"that binds it, so that a host compiling this package carries no engine but the one it "+
			"deploys on", pkg)
	}
}

// dependenciesOf lists the packages a `go list -deps` target reaches.
func dependenciesOf(t *testing.T, target string) map[string]bool {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", target).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s failed: %v\n%s", target, err, out)
	}
	reached := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if pkg := strings.TrimSpace(line); pkg != "" {
			reached[pkg] = true
		}
	}
	if len(reached) == 0 {
		t.Fatalf("go list -deps %s listed nothing, so there is no dependency face to check", target)
	}
	return reached
}

// stdlibPackage reports whether a package path names a package of the standard
// library, whose first path element never carries a dot.
func stdlibPackage(pkg string) bool {
	first, _, _ := strings.Cut(pkg, "/")
	return !strings.Contains(first, ".")
}
