package stdmux

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
// the whole of what distinguishes this subpackage from the next one. The
// wrapper is what carries the attribution; the multiplexer beneath it is the
// router, and the engine's own answers come from that one.
func TestEngineIsAServeMux(t *testing.T) {
	engine, ok := newEngine().(*engine)
	if !ok {
		t.Fatalf("the engine is a %T, and this subpackage is the one that binds *http.ServeMux", newEngine())
	}
	if engine.mux == nil {
		t.Error("the engine carries no multiplexer, so nothing is behind the seam")
	}
}

// TestEngineServesWhatItHandles pins that the wrapper in front of the
// multiplexer still routes: the seam's other half is that an engine answers as
// a handler, and a wrapper that failed to delegate would leave every endpoint
// answering 404 with the chain built around it.
func TestEngineServesWhatItHandles(t *testing.T) {
	engine := newEngine()
	engine.Handle("GET /things", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/things", nil))
	if recorder.Code != http.StatusTeapot {
		t.Errorf("the request answered %d, and the handler bound to the pattern writes %d",
			recorder.Code, http.StatusTeapot)
	}

	missing := httptest.NewRecorder()
	engine.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/elsewhere", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("an unrouted request answered %d, want %d", missing.Code, http.StatusNotFound)
	}
}

// TestEngineAttributesARefusalToAPair pins the attribution the parent's
// mounting point asks of this subpackage. The parent sees only that a mount
// was refused; which mounted pattern the refused one cannot stand beside is the
// engine's answer, and a refusal that goes unanswered leaves the startup
// failure saying so instead of naming a pair.
//
// The second half is the other side of the same method: a pattern no mounted
// one clashes with reproduces nothing, so there is no pair to name and the
// answer is false rather than an arbitrary first pattern.
func TestEngineAttributesARefusalToAPair(t *testing.T) {
	mounted := []string{"GET /things/{id}", "GET /other"}

	partner, found := newEngine().(*engine).AttributeRefusal("GET /things/{name}", mounted)
	if !found {
		t.Fatal("the refusal was not attributed, and the multiplexer judges this pair as a clash")
	}
	if partner != "GET /things/{id}" {
		t.Errorf("the refusal was attributed to %q, and it is %q the newcomer clashes with",
			partner, "GET /things/{id}")
	}

	if partner, found := newEngine().(*engine).AttributeRefusal("GET /unrelated", mounted); found {
		t.Errorf("the refusal of a pattern that clashes with nothing was attributed to %q, "+
			"and no mounted pattern reproduces it", partner)
	}
}

// TestEngineAnswersTheAttributionTheParentAsksFor pins the seam's second
// requirement from the answering side, mechanically.
//
// The parent declares the attribution as the method of an unexported interface,
// so this package cannot name the interface to hold the engine against it.
// Reading it out of the parent's source is what closes that: a signature that
// drifted on either side would leave the attribution silently unanswered —
// startup still fails, still says attribution was not available — and the
// answer would be gone with nothing going red.
func TestEngineAnswersTheAttributionTheParentAsksFor(t *testing.T) {
	asked := attributionTheParentAsksFor(t)
	if len(asked) == 0 {
		t.Fatalf("no attribution interface was found in the parent package, so this test proved nothing")
	}

	engine := reflect.TypeOf(newEngine())
	for name, asked := range asked {
		method, found := engine.MethodByName(name)
		if !found {
			t.Errorf("the parent asks an engine for %s, and %T does not answer it", name, newEngine())
			continue
		}
		if got := methodSignature(method); got != asked {
			t.Errorf("the parent asks for %s %s, and the engine answers %s: the attribution "+
				"would go unanswered without either side failing to compile", name, asked, got)
		}
	}
}

// methodSignature renders a method's type as funcSignature renders a declared
// one: the receiver is dropped, and the parameter and result types stand
// without their names. reflect keeps the receiver as the first argument, which
// is the only thing the two renderings have to be reconciled about.
func methodSignature(method reflect.Method) string {
	var params, results []string
	for i := 1; i < method.Type.NumIn(); i++ {
		params = append(params, method.Type.In(i).String())
	}
	for i := range method.Type.NumOut() {
		results = append(results, method.Type.Out(i).String())
	}
	return signature(params, results)
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

// attributionTheParentAsksFor reads the parent package's source and renders
// every method of its attribution interface the way reflect renders a func
// type, so the two can be compared. The interface is unexported, which is why
// it is read rather than named: a package cannot import it.
func attributionTheParentAsksFor(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("reading the parent package directory failed: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join("..", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing the parent's %s failed: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "refusalAttributor" {
					continue
				}
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok {
					t.Fatalf("the parent declares refusalAttributor as %T, not as an interface", typeSpec.Type)
				}
				for _, method := range iface.Methods.List {
					if len(method.Names) != 1 {
						continue
					}
					fn, ok := method.Type.(*ast.FuncType)
					if !ok {
						continue
					}
					found[method.Names[0].Name] = funcSignature(fn)
				}
			}
		}
	}
	return found
}

// funcSignature renders a declared func type the way methodSignature renders a
// reflected one.
func funcSignature(fn *ast.FuncType) string {
	return signature(fieldsTypes(fn.Params), fieldsTypes(fn.Results))
}

// fieldsTypes renders one side of a declared signature as a list of type names,
// without the names the parameters or results were given.
func fieldsTypes(list *ast.FieldList) []string {
	if list == nil {
		return nil
	}
	var rendered []string
	for _, field := range list.List {
		repeated := len(field.Names)
		if repeated == 0 {
			repeated = 1
		}
		for range repeated {
			rendered = append(rendered, types.ExprString(field.Type))
		}
	}
	return rendered
}

// signature is the one rendering both sides agree on: parameter and result
// types in order, without names, and the results always parenthesised so that a
// single one reads the same as several.
func signature(params, results []string) string {
	return "func(" + strings.Join(params, ", ") + ") (" + strings.Join(results, ", ") + ")"
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
