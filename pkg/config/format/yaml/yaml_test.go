package yaml_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/config/format/yaml"
	"github.com/vislake/speed/pkg/core"
)

// parser builds the format the way the configuration module gets it, out of
// the module's resources, so the tests exercise what a host actually ends up
// with rather than a value this package hands itself.
func parser(t *testing.T) config.Format {
	t.Helper()
	reg := core.New()
	reg.Register(yaml.Module())
	found := core.Resources[config.Format](reg)
	if len(found) != 1 {
		t.Fatalf("the module declares %d Format resources, want exactly 1", len(found))
	}
	return found[0].Value
}

func TestName(t *testing.T) {
	if got := parser(t).Name(); got != "yaml" {
		t.Errorf("Name() = %q, want %q", got, "yaml")
	}
}

// TestNestedMapping pins the tree the conversion layer is written against,
// including the types YAML resolves scalars to: 5m has to stay a string for a
// duration field to parse it, and a count arrives as an integer.
func TestNestedMapping(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte("cache:\n  redis:\n    pool-size: 4\n    ttl: 5m\n    tls: true\n"))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	want := map[string]any{
		"cache": map[string]any{
			"redis": map[string]any{
				"pool-size": 4,
				"ttl":       "5m",
				"tls":       true,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmarshal() = %#v, want %#v", got, want)
	}
}

// TestSequenceOfMappings covers the shape a list of structs is written in: the
// elements reach the conversion layer as string-keyed maps like any other
// mapping.
func TestSequenceOfMappings(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte("endpoints:\n  - addr: a:1\n  - addr: b:2\n"))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	want := map[string]any{"endpoints": []any{
		map[string]any{"addr": "a:1"},
		map[string]any{"addr": "b:2"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmarshal() = %#v, want %#v", got, want)
	}
}

// TestTopLevelNonStringKeyRejected covers the key YAML permits and this
// mechanism does not: a number turned into its text would make 1 and "1" the
// same key for good.
func TestTopLevelNonStringKeyRejected(t *testing.T) {
	_, err := parser(t).Unmarshal([]byte("1: a\n"))
	if err == nil {
		t.Fatal("Unmarshal() succeeded on a numeric key, want an error")
	}
	if !strings.Contains(err.Error(), "!!int") {
		t.Errorf("err = %v, want the text to name what the key resolved to", err)
	}
}

// TestNestedNonStringKeyRejected proves the rule reaches the whole tree, which
// is where it matters: a nested map[any]any would otherwise reach the
// conversion layer as a type it has no rule for.
func TestNestedNonStringKeyRejected(t *testing.T) {
	if _, err := parser(t).Unmarshal([]byte("a:\n  1: x\n")); err == nil {
		t.Fatal("Unmarshal() succeeded on a nested numeric key, want an error")
	}
}

// TestQuotedNumericKeyAccepted is the other half of the rule: quoting says the
// key is a string, and YAML resolves it to one.
func TestQuotedNumericKeyAccepted(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte("\"1\": x\n"))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"1": "x"}) {
		t.Errorf("Unmarshal() = %#v, want the quoted key kept as a string", got)
	}
}

// TestAliasResolved covers anchors and aliases, which the walk has to follow
// itself: an alias node carries no value of its own.
func TestAliasResolved(t *testing.T) {
	got, err := parser(t).Unmarshal([]byte("base: &b\n  addr: a:1\nprimary: *b\n"))
	if err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	want := map[string]any{
		"base":    map[string]any{"addr": "a:1"},
		"primary": map[string]any{"addr": "a:1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmarshal() = %#v, want %#v", got, want)
	}
}

// TestNonStringKeyInsideAliasRejected keeps the key rule from being escaped by
// writing the mapping once and referring to it.
func TestNonStringKeyInsideAliasRejected(t *testing.T) {
	if _, err := parser(t).Unmarshal([]byte("base: &b\n  1: x\nprimary: *b\n")); err == nil {
		t.Fatal("Unmarshal() succeeded on a numeric key reached through an alias, want an error")
	}
}

// TestUndefinedAliasRejected covers an alias with no anchor behind it.
func TestUndefinedAliasRejected(t *testing.T) {
	if _, err := parser(t).Unmarshal([]byte("primary: *missing\n")); err == nil {
		t.Fatal("Unmarshal() succeeded on an undefined alias, want an error")
	}
}

// TestSelfReferentialAliasRejected covers an anchor holding an alias to
// itself. The library parses it and leaves a cycle in the tree, so a walk
// without memory of its path never returns; the failure has to be reported
// rather than waited out.
func TestSelfReferentialAliasRejected(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := parser(t).Unmarshal([]byte("a: &x\n  b: *x\n"))
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatal("Unmarshal() succeeded on an anchor that holds itself, want an error")
	}
}

// TestMergeKeyRejected pins the consequence of the key rule the design fixes:
// << carries the merge tag rather than the string tag, so it is refused like
// any other key that is not a string.
func TestMergeKeyRejected(t *testing.T) {
	_, err := parser(t).Unmarshal([]byte("base: &b\n  addr: a:1\nprimary:\n  <<: *b\n  port: 2\n"))
	if err == nil {
		t.Fatal("Unmarshal() succeeded on a merge key, want an error")
	}
	if !strings.Contains(err.Error(), "<<") {
		t.Errorf("err = %v, want the text to name the merge key it refused", err)
	}
}

// TestMultipleDocumentsRejected covers a stream carrying more than one
// document. Reading the first and dropping the rest would leave settings
// somebody wrote with no effect and nothing said about it.
func TestMultipleDocumentsRejected(t *testing.T) {
	_, err := parser(t).Unmarshal([]byte("a: 1\n---\nb: 2\n"))
	if err == nil {
		t.Fatal("Unmarshal() succeeded on a two-document stream, want an error")
	}
	if !strings.Contains(err.Error(), "document") {
		t.Errorf("err = %v, want the text to say what it refused", err)
	}
}

// TestEmptyDocumentIsEmptyMapping fixes the reading that a source carrying
// nothing is a legal run rather than a failure.
func TestEmptyDocumentIsEmptyMapping(t *testing.T) {
	for name, data := range map[string]string{
		"nothing":      "",
		"comment only": "# nothing here\n",
		"null":         "null\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parser(t).Unmarshal([]byte(data))
			if err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("Unmarshal() = %#v, want an empty mapping", got)
			}
		})
	}
}

// TestNonMappingTopLevelRejected covers the documents that parse as YAML and
// still cannot be a configuration: the top level carries the keys, so anything
// but a mapping has nowhere to put them.
func TestNonMappingTopLevelRejected(t *testing.T) {
	for name, data := range map[string]string{
		"sequence": "- 1\n- 2\n",
		"scalar":   "cache\n",
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

func TestMalformedDocumentRejected(t *testing.T) {
	if _, err := parser(t).Unmarshal([]byte("a: [1, 2\n")); err == nil {
		t.Fatal("Unmarshal() succeeded on malformed YAML, want an error")
	}
}

// TestRegistersAsFormatResource proves the blank-import contract: importing
// the package puts the parser in the process registry, where the
// configuration module finds it by resource type alone.
func TestRegistersAsFormatResource(t *testing.T) {
	var found []core.Resource[config.Format]
	for _, res := range core.Resources[config.Format](core.ProcessRegistry) {
		if res.Module == "config.format.yaml" {
			found = append(found, res)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the process registry holds %d Format resources from config.format.yaml, want 1", len(found))
	}
	if got := found[0].Value.Name(); got != "yaml" {
		t.Errorf("the registered parser answers for %q, want %q", got, "yaml")
	}
}
