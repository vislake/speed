package json_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/config/format/json"
	"github.com/vislake/speed/pkg/core"
)

// parser builds the format the way the configuration module gets it, out of
// the module's resources, so the tests exercise what a host actually ends up
// with rather than a value this package hands itself.
func parser(t *testing.T) config.Format {
	t.Helper()
	reg := core.New()
	reg.Register(json.Module())
	found := core.Resources[config.Format](reg)
	if len(found) != 1 {
		t.Fatalf("the module declares %d Format resources, want exactly 1", len(found))
	}
	return found[0].Value
}

func TestName(t *testing.T) {
	if got := parser(t).Name(); got != "json" {
		t.Errorf("Name() = %q, want %q", got, "json")
	}
}

// TestNestedObject pins the shape the conversion layer is written against:
// nested objects become nested map[string]any and numbers arrive as float64.
func TestNestedObject(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte(`{"cache":{"redis":{"pool-size":4,"addr":"localhost:6379"}}}`))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	want := map[string]any{
		"cache": map[string]any{
			"redis": map[string]any{
				"pool-size": float64(4),
				"addr":      "localhost:6379",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmarshal() = %#v, want %#v", got, want)
	}
}

// TestNonObjectTopLevelRejected covers the documents that parse as JSON and
// still cannot be a configuration: the top level carries the keys, so anything
// but a mapping has nowhere to put them.
func TestNonObjectTopLevelRejected(t *testing.T) {
	for name, data := range map[string]string{
		"array":  `[1, 2]`,
		"string": `"cache"`,
		"number": `7`,
		"bool":   `true`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parser(t).Unmarshal([]byte(data))
			if err == nil {
				t.Fatalf("Unmarshal() = %#v, want an error for a %s at the top level", got, name)
			}
			if !strings.Contains(err.Error(), "top level") {
				t.Errorf("err = %v, want the text to say where the problem is", err)
			}
		})
	}
}

// TestNullDocumentIsEmpty fixes the reading that a source carrying nothing is
// a legal run rather than a failure: the layers below it decide every value.
func TestNullDocumentIsEmpty(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte(`null`))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Unmarshal() = %#v, want an empty mapping", got)
	}
}

func TestMalformedDocumentRejected(t *testing.T) {
	if _, err := parser(t).Unmarshal([]byte(`{"a": }`)); err == nil {
		t.Fatal("Unmarshal() succeeded on malformed JSON, want an error")
	}
}

// TestRegistersAsFormatResource proves the blank-import contract: importing
// the package puts the parser in the process registry, where the
// configuration module finds it by resource type alone.
func TestRegistersAsFormatResource(t *testing.T) {
	var found []core.Resource[config.Format]
	for _, res := range core.Resources[config.Format](core.ProcessRegistry) {
		if res.Module == "config.format.json" {
			found = append(found, res)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the process registry holds %d Format resources from config.format.json, want 1", len(found))
	}
	if got := found[0].Value.Name(); got != "json" {
		t.Errorf("the registered parser answers for %q, want %q", got, "json")
	}
}

// TestJSONFormatUsesStdlibOnly is the executable form of what splitting the
// two parsers buys: a host that configures itself in JSON takes on no parsing
// dependency. A standard library path has no domain in its first segment.
func TestJSONFormatUsesStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg := strings.TrimSpace(line)
		if pkg == "" || strings.HasPrefix(pkg, "github.com/vislake/speed/pkg/") {
			continue
		}
		if first, _, _ := strings.Cut(pkg, "/"); strings.Contains(first, ".") {
			t.Errorf("the json format package depends on %s, which is outside the standard library", pkg)
		}
	}
}
