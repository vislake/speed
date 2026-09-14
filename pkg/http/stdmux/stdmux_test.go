package stdmux

import (
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
	speedhttp "github.com/vislake/speed/pkg/http"
)

// TestModuleRegistersItself pins what importing this package is meant to be
// enough for: the host writes the import and the entry point is assembled from
// its configuration, with no registration call of its own.
//
// The name is written out here instead of being read from the constant, which
// is the only way this pins anything: a lookup by moduleName follows the
// constant wherever it goes and passes under any name at all. The absence of a
// module under "http" is the other half of the same statement — that is the
// release unit, and no module registers under it.
func TestModuleRegistersItself(t *testing.T) {
	const name = "http.stdmux"
	if moduleName != name {
		t.Errorf("this module registers as %q, and an implementation subpackage is named %q, "+
			"the release unit plus the subpackage", moduleName, name)
	}
	if _, found := core.ProcessRegistry.Lookup("http"); found {
		t.Error(`a module is registered under "http", which names the release unit and not a module`)
	}
	registered, found := core.ProcessRegistry.Lookup(name)
	if !found {
		t.Fatalf("importing this package did not register a module named %q", name)
	}
	if len(registered.Provides) != 1 ||
		reflect.TypeOf(registered.Provides[0].Token) != reflect.TypeFor[*speedhttp.Router]() {
		t.Errorf("the registered module delivers %+v, and it has to deliver Router", registered.Provides)
	}
	if !registered.Provides[0].Exclusive {
		t.Error("the registered module delivers Router without the exclusive claim")
	}
}

// TestEngineIsAServeMux pins the engine this subpackage binds. The seam takes
// any handler that accepts pattern registrations, and which one arrived here is
// the whole of what distinguishes this subpackage from the next one.
func TestEngineIsAServeMux(t *testing.T) {
	if _, ok := newEngine().(*http.ServeMux); !ok {
		t.Errorf("the engine is a %T, and this subpackage is the one that binds *http.ServeMux", newEngine())
	}
}

// TestEngineReportsAPatternClashByPanicking pins the behaviour the parent's
// mounting point is built around: this engine reports a clash by panicking, and
// the parent catches it there and turns it into a startup failure wrapping
// ErrRouteConflict. An engine that returned an error instead, or accepted the
// second registration silently, would leave that conversion catching nothing.
func TestEngineReportsAPatternClashByPanicking(t *testing.T) {
	engine := newEngine()
	engine.Handle("GET /things", http.NotFoundHandler())

	var raised any
	func() {
		defer func() { raised = recover() }()
		engine.Handle("GET /things", http.NotFoundHandler())
	}()

	if raised == nil {
		t.Fatal("the engine accepted two registrations of one pattern without a word")
	}
	complaint := toText(raised)
	if !strings.Contains(complaint, "conflicts with pattern") {
		t.Errorf("the engine's complaint does not say the patterns clash: %v", raised)
	}
	// Both sides are named, which is what the parent's startup failure carries
	// through to the reader.
	if strings.Count(complaint, "GET /things") < 2 {
		t.Errorf("the engine's complaint names only one side of the clash: %v", raised)
	}
}

// toText renders a recovered value for an assertion.
func toText(raised any) string {
	if err, ok := raised.(error); ok {
		return err.Error()
	}
	if text, ok := raised.(string); ok {
		return text
	}
	return reflect.TypeOf(raised).String()
}

// TestDependencyClosureStaysInsideTheRepo is the executable form of the claim
// this subpackage's documentation makes: it brings no dependency a host would
// not otherwise carry. The four in-repository prefixes are struck out first —
// the closure necessarily contains them — and what is left has to be the
// standard library.
func TestDependencyClosureStaysInsideTheRepo(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	own := []string{
		"github.com/vislake/speed/pkg/core",
		"github.com/vislake/speed/pkg/config",
		"github.com/vislake/speed/pkg/log",
		"github.com/vislake/speed/pkg/http",
	}
	scanned := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" {
			continue
		}
		scanned++
		mine := false
		for _, prefix := range own {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				mine = true
			}
		}
		if mine {
			continue
		}
		if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
			t.Errorf("stdmux depends on %s, which is outside the standard library", pkg)
		}
	}
	if scanned == 0 {
		t.Fatal("go list -deps listed no package, so the scan above proved nothing")
	}
}
