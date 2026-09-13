package config

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// expandOne expands a single module's declaration and indexes the result by
// path, which is how the assertions below address the items.
func expandOne(t *testing.T, s Schema, prefix string) map[string]*manifestItem {
	t.Helper()
	items, err := expandSchema("mod", s, prefix)
	if err != nil {
		t.Fatalf("expanding the declaration failed: %v", err)
	}
	byPath := make(map[string]*manifestItem, len(items))
	for _, item := range items {
		byPath[item.path] = item
	}
	return byPath
}

// expandFails expands a declaration that is expected to be rejected.
func expandFails(t *testing.T, s Schema, prefix string) error {
	t.Helper()
	items, err := expandSchema("mod", s, prefix)
	if err == nil {
		t.Fatalf("expanding the declaration produced %d items, want a rejection", len(items))
	}
	if !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("expansion returned %v, want ErrInvalidSchema", err)
	}
	return err
}

func mount(value any) []Mount { return []Mount{{Value: value}} }

func pathsOf(byPath map[string]*manifestItem) []string {
	return slices.Sorted(maps.Keys(byPath))
}

func TestFieldPathTagPrecedence(t *testing.T) {
	type carrier struct {
		Tagged   string `config:"alpha" json:"beta"`
		JSONOnly string `json:"gamma,omitempty"`
		PoolSize int
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "")
	want := []string{"alpha", "gamma", "pool-size"}
	if got := pathsOf(byPath); !slices.Equal(got, want) {
		t.Fatalf("expansion produced paths %v, want %v", got, want)
	}
}

func TestCamelToKebab(t *testing.T) {
	cases := map[string]string{
		"PoolSize":  "pool-size",
		"Addr":      "addr",
		"HTTPPort":  "http-port",
		"ID":        "id",
		"MaxConns2": "max-conns2",
	}
	for field, want := range cases {
		if got := camelToKebab(field); got != want {
			t.Errorf("field %s derives key %q, want %q", field, got, want)
		}
	}
}

func TestKeySegmentCharsetRejects(t *testing.T) {
	type underscore struct {
		Field string `config:"pool_size"`
	}
	type capital struct {
		Field string `config:"PoolSize"`
	}
	type dotted struct {
		Field string `config:"redis.addr"`
	}
	type plain struct {
		Field string
	}
	cases := map[string]struct {
		schema   Schema
		fragment string
	}{
		"underscore in a key":     {Schema{Mounts: mount(&underscore{})}, "pool_size"},
		"upper case in a key":     {Schema{Mounts: mount(&capital{})}, "PoolSize"},
		"dot inside a segment":    {Schema{Mounts: mount(&dotted{})}, "redis.addr"},
		"empty namespace segment": {Schema{Namespace: "cache..redis", Mounts: mount(&plain{})}, "empty segment"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := expandFails(t, tc.schema, "")
			if !strings.Contains(err.Error(), tc.fragment) {
				t.Fatalf("rejection reads %q, which does not mention %q", err, tc.fragment)
			}
		})
	}
}

func TestSkipsUndecodableFields(t *testing.T) {
	type inner struct {
		Addr string
	}
	type carrier struct {
		Kept     string
		hidden   string //nolint:unused // the point of the case is that it is skipped
		Hook     func() error
		Events   chan int
		Anything any
		Absent   *inner
		Present  *inner
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{Present: &inner{Addr: "localhost"}})}, "")
	want := []string{"kept", "present.addr"}
	if got := pathsOf(byPath); !slices.Equal(got, want) {
		t.Fatalf("expansion produced paths %v, want %v", got, want)
	}
	for _, skipped := range []string{"hidden", "hook", "events", "anything", "absent"} {
		if _, found := byPath[skipped]; found {
			t.Errorf("path %q reached the manifest, and no source can give it a value", skipped)
		}
	}
}

func TestNonNilStructPointerExpands(t *testing.T) {
	type tls struct {
		ServerName string
		MinVersion int
	}
	type carrier struct {
		TLS *tls
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{TLS: &tls{ServerName: "example.test", MinVersion: 3}})}, "")
	if got := byPath["tls.server-name"]; got == nil || got.def != "example.test" {
		t.Fatalf("tls.server-name is %+v, want the prototype's current value example.test", got)
	}
	if got := byPath["tls.min-version"]; got == nil || got.def != 3 {
		t.Fatalf("tls.min-version is %+v, want the prototype's current value 3", got)
	}
}

type ringA struct {
	B *ringB
}

type ringB struct {
	A *ringA
}

func TestTypeCycleNamesFieldAndType(t *testing.T) {
	a := &ringA{}
	b := &ringB{A: a}
	a.B = b
	err := expandFails(t, Schema{Mounts: mount(a)}, "")
	for _, fragment := range []string{"config.ringA", "A"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("rejection reads %q, which does not mention %q", err, fragment)
		}
	}
}

func TestSameTypeOnSiblingBranchesIsNotCycle(t *testing.T) {
	type node struct {
		Name string
	}
	type carrier struct {
		Left  *node
		Right *node
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{Left: &node{}, Right: &node{}})}, "")
	want := []string{"left.name", "right.name"}
	if got := pathsOf(byPath); !slices.Equal(got, want) {
		t.Fatalf("expansion produced paths %v, want %v", got, want)
	}
}

func TestDeepNestingHasNoDepthLimit(t *testing.T) {
	const depth = 20
	nested := reflect.StructOf([]reflect.StructField{{Name: "Leaf", Type: reflect.TypeFor[string]()}})
	for range depth - 1 {
		nested = reflect.StructOf([]reflect.StructField{{Name: "Inner", Type: nested}})
	}
	byPath := expandOne(t, Schema{Mounts: mount(reflect.New(nested).Interface())}, "")
	want := strings.Repeat("inner.", depth-1) + "leaf"
	if got := pathsOf(byPath); !slices.Equal(got, []string{want}) {
		t.Fatalf("expansion produced paths %v, want the single path %q", got, want)
	}
}

func TestEnvNameDerivation(t *testing.T) {
	type redis struct {
		MaxConns int
	}
	byPath := expandOne(t, Schema{
		Namespace: "cache",
		Mounts:    []Mount{{Path: "redis", Value: &redis{}}},
	}, "MYAPP")
	item := byPath["cache.redis.max-conns"]
	if item == nil {
		t.Fatalf("expansion produced paths %v, want cache.redis.max-conns", pathsOf(byPath))
	}
	if want := "MYAPP_CACHE__REDIS__MAX_CONNS"; item.envName != want {
		t.Fatalf("item reads environment variable %q, want %q", item.envName, want)
	}
}

func TestPinnedEnvNameWins(t *testing.T) {
	type carrier struct {
		Colour string
	}
	byPath := expandOne(t, Schema{
		Mounts: mount(&carrier{}),
		Items:  map[string]Item{"colour": {EnvName: "NO_COLOR"}},
	}, "MYAPP")
	if got := byPath["colour"].envName; got != "NO_COLOR" {
		t.Fatalf("item reads environment variable %q, want the pinned NO_COLOR", got)
	}
}

func TestNoPrefixDisablesDerivedEnvNamesOnly(t *testing.T) {
	type carrier struct {
		Colour   string
		PoolSize int
	}
	byPath := expandOne(t, Schema{
		Mounts: mount(&carrier{}),
		Items:  map[string]Item{"colour": {EnvName: "NO_COLOR"}},
	}, "")
	if got := byPath["colour"].envName; got != "NO_COLOR" {
		t.Fatalf("pinned item reads environment variable %q, want NO_COLOR", got)
	}
	if got := byPath["pool-size"].envName; got != "" {
		t.Fatalf("derived item reads environment variable %q, want none without a prefix", got)
	}
}

func TestEmptyNamespaceLandsAtTopLevel(t *testing.T) {
	type carrier struct {
		LogLevel string
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "MYAPP")
	item := byPath["log-level"]
	if item == nil {
		t.Fatalf("expansion produced paths %v, want the top-level path log-level", pathsOf(byPath))
	}
	if want := "MYAPP_LOG_LEVEL"; item.envName != want {
		t.Fatalf("item reads environment variable %q, want %q", item.envName, want)
	}
}

func TestContainerFieldsAreLeaves(t *testing.T) {
	type endpoint struct {
		Addr string
	}
	type carrier struct {
		Labels    map[string]string
		Endpoints []endpoint
		Names     []string
		Deadline  time.Duration
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "")
	want := []string{"deadline", "endpoints", "labels", "names"}
	if got := pathsOf(byPath); !slices.Equal(got, want) {
		t.Fatalf("expansion produced paths %v, want %v", got, want)
	}
	kinds := map[string]leafKind{
		"labels":    kindContainer,
		"endpoints": kindContainer,
		"names":     kindStringList,
		"deadline":  kindScalar,
	}
	for path, want := range kinds {
		if got := byPath[path].kind; got != want {
			t.Errorf("item %q is kind %d, want %d", path, got, want)
		}
	}
}

func TestFlagOriginRequiresFlagName(t *testing.T) {
	type carrier struct {
		Addr string
	}
	err := expandFails(t, Schema{
		Mounts: mount(&carrier{}),
		Items:  map[string]Item{"addr": {Origins: OriginFlag}},
	}, "")
	if !strings.Contains(err.Error(), "FlagName") {
		t.Fatalf("rejection reads %q, which does not mention FlagName", err)
	}
}

func TestSensitiveRequiresDescription(t *testing.T) {
	type carrier struct {
		Password string
	}
	err := expandFails(t, Schema{
		Mounts: mount(&carrier{}),
		Items:  map[string]Item{"password": {Sensitive: true}},
	}, "")
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("rejection reads %q, which does not name the item", err)
	}
}

func TestOrphanItemKeyRejected(t *testing.T) {
	type redis struct {
		Addr string
	}
	err := expandFails(t, Schema{
		Mounts: []Mount{{Path: "redis", Value: &redis{}}},
		Items:  map[string]Item{"redis.addres": {Required: true}},
	}, "")
	for _, fragment := range []string{"mod", "redis.addres"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("rejection reads %q, which does not mention %q", err, fragment)
		}
	}
}

func TestMountValueMustCarryAStruct(t *testing.T) {
	var absent *struct{ Addr string }
	cases := map[string]any{
		"a scalar":             "not a struct",
		"a nil struct pointer": absent,
		"nothing at all":       nil,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			expandFails(t, Schema{Mounts: mount(value)}, "")
		})
	}
}

// TestTagDashSkipsField pins the reading of a "-" tag: the field stays out of
// the manifest, as it does everywhere else a tag of that shape is read.
func TestTagDashSkipsField(t *testing.T) {
	type carrier struct {
		Kept     string
		Internal string `json:"-"`
		Excluded string `config:"-"`
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "")
	if got := pathsOf(byPath); !slices.Equal(got, []string{"kept"}) {
		t.Fatalf("expansion produced paths %v, want the single path kept", got)
	}
}

// TestTextUnmarshalerStructIsALeaf pins a struct type that takes its value from
// a string of its own as an item rather than as a level of the path: expanding
// it would reach nothing but unexported fields and the item would disappear.
func TestTextUnmarshalerStructIsALeaf(t *testing.T) {
	type carrier struct {
		Deadline time.Time
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "")
	item := byPath["deadline"]
	if item == nil {
		t.Fatalf("expansion produced paths %v, want the single path deadline", pathsOf(byPath))
	}
	if item.kind != kindScalar || item.typ != reflect.TypeFor[time.Time]() {
		t.Fatalf("deadline is kind %d of type %s, want a scalar of time.Time", item.kind, item.typ)
	}
}

// TestEmbeddedStructContributesASegment pins an embedded field to the same rule
// as a named one: its field name, which is its type name, becomes a path
// segment. An embedded unexported type is skipped like any unexported field.
func TestEmbeddedStructContributesASegment(t *testing.T) {
	byPath := expandOne(t, Schema{Mounts: mount(&embedder{})}, "")
	want := []string{"embedded-options.addr", "name"}
	if got := pathsOf(byPath); !slices.Equal(got, want) {
		t.Fatalf("expansion produced paths %v, want %v", got, want)
	}
}

// EmbeddedOptions is embedded by the case above, and has to be exported for the
// embedded field itself to be.
type EmbeddedOptions struct {
	Addr string
}

type hiddenOptions struct {
	Secret string
}

type embedder struct {
	EmbeddedOptions
	hiddenOptions
	Name string
}

// TestNilScalarPointerIsALeaf pins a nil pointer to a non-struct as an item: a
// source that gives it a value has somewhere to put it, unlike a nil struct
// pointer whose whole subtree would arrive without defaults.
func TestNilScalarPointerIsALeaf(t *testing.T) {
	type carrier struct {
		Retries *int
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "")
	item := byPath["retries"]
	if item == nil {
		t.Fatalf("expansion produced paths %v, want the single path retries", pathsOf(byPath))
	}
	if item.kind != kindScalar {
		t.Fatalf("retries is kind %d, want a scalar", item.kind)
	}
}
