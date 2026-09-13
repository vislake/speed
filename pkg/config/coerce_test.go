package config

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// coerceTo converts a value into T and fails the test when it cannot.
func coerceTo[T any](t *testing.T, raw any) T {
	t.Helper()
	got, err := coerce("some.path", raw, reflect.TypeFor[T]())
	if err != nil {
		t.Fatalf("converting %#v to %s failed: %v", raw, reflect.TypeFor[T](), err)
	}
	return got.(T)
}

// coerceRejects converts a value that is expected to be refused.
func coerceRejects[T any](t *testing.T, raw any) error {
	t.Helper()
	got, err := coerce("some.path", raw, reflect.TypeFor[T]())
	if err == nil {
		t.Fatalf("converting %#v to %s produced %#v, want a rejection", raw, reflect.TypeFor[T](), got)
	}
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("conversion returned %v, want ErrTypeMismatch", err)
	}
	return err
}

func TestDurationFromString(t *testing.T) {
	if got := coerceTo[time.Duration](t, "5m"); got != 5*time.Minute {
		t.Fatalf("\"5m\" converted to %v, want 5m0s", got)
	}
	if got := coerceTo[time.Duration](t, "1h30m"); got != 90*time.Minute {
		t.Fatalf("\"1h30m\" converted to %v, want 1h30m0s", got)
	}
}

// TestBareNumberDurationRejected pins the one rule this mechanism has to give
// itself: a Duration is an int64 count of nanoseconds, so a bare 300 would
// read as five minutes and mean three hundred nanoseconds.
func TestBareNumberDurationRejected(t *testing.T) {
	for name, raw := range map[string]any{
		"a JSON number":              float64(300),
		"an integer":                 300,
		"a digit string but no unit": "300",
	} {
		t.Run(name, func(t *testing.T) {
			coerceRejects[time.Duration](t, raw)
		})
	}
}

func TestIntFromJSONNumber(t *testing.T) {
	if got := coerceTo[int](t, float64(10)); got != 10 {
		t.Fatalf("the JSON number 10 converted to %d, want 10", got)
	}
	err := coerceRejects[int](t, float64(10.5))
	if !strings.Contains(err.Error(), "10.5") {
		t.Fatalf("the rejection reads %q, which does not name the value that was given", err)
	}
}

func TestTextUnmarshalerSupported(t *testing.T) {
	want := time.Date(2026, 9, 13, 10, 30, 0, 0, time.UTC)
	if got := coerceTo[time.Time](t, "2026-09-13T10:30:00Z"); !got.Equal(want) {
		t.Fatalf("the timestamp converted to %v, want %v", got, want)
	}
	if got := coerceTo[net.IP](t, "10.0.0.1"); !got.Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("the address converted to %v, want 10.0.0.1", got)
	}
	// A type that decodes itself from text takes a string and nothing else.
	coerceRejects[time.Time](t, float64(17))
	// Its own rejection reaches the caller rather than being swallowed.
	err := coerceRejects[time.Time](t, "not a timestamp")
	if !strings.Contains(err.Error(), "not a timestamp") {
		t.Fatalf("the rejection reads %q, which does not name the value that was given", err)
	}
}

func TestScalarConversions(t *testing.T) {
	if got := coerceTo[bool](t, "true"); !got {
		t.Fatal("the string \"true\" converted to false")
	}
	if got := coerceTo[bool](t, false); got {
		t.Fatal("the boolean false converted to true")
	}
	if got := coerceTo[int32](t, "-7"); got != -7 {
		t.Fatalf("the string \"-7\" converted to %d, want -7", got)
	}
	if got := coerceTo[uint16](t, float64(8080)); got != 8080 {
		t.Fatalf("the JSON number 8080 converted to %d, want 8080", got)
	}
	if got := coerceTo[float64](t, "2.5"); got != 2.5 {
		t.Fatalf("the string \"2.5\" converted to %v, want 2.5", got)
	}
	if got := coerceTo[float64](t, 3); got != 3 {
		t.Fatalf("the integer 3 converted to %v, want 3", got)
	}
	if got := coerceTo[string](t, "plain"); got != "plain" {
		t.Fatalf("the string converted to %q", got)
	}
	// A number is not silently rendered into a string field: 1 and "1" stay
	// apart, which is the same reason a numeric key is refused.
	coerceRejects[string](t, float64(1))
	coerceRejects[bool](t, "yes please")
	coerceRejects[uint8](t, -1)
}

func TestIntegerOverflowRejected(t *testing.T) {
	err := coerceRejects[int8](t, float64(300))
	if !strings.Contains(err.Error(), "int8") {
		t.Fatalf("the rejection reads %q, which does not name the target type", err)
	}
	coerceRejects[int8](t, 300)
	coerceRejects[uint32](t, float64(1e20))
}

func TestStringListCommaSeparatedReplaces(t *testing.T) {
	item := &manifestItem{path: "hosts", typ: reflect.TypeFor[[]string](), kind: kindStringList}
	got, err := coerceText("hosts", "a,b,c", item)
	if err != nil {
		t.Fatalf("converting the list failed: %v", err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the list converted to %#v, want %#v", got, want)
	}
	// The layer replaces rather than appends: a second value does not carry
	// the first one along.
	got, err = coerceText("hosts", "only", item)
	if err != nil {
		t.Fatalf("converting the list failed: %v", err)
	}
	if want := []string{"only"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a second value produced %#v, want %#v", got, want)
	}
	got, err = coerceText("hosts", "", item)
	if err != nil {
		t.Fatalf("converting an empty list failed: %v", err)
	}
	if want := []string{}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an empty value produced %#v, want an empty list", got)
	}
}

// TestCommaInsideElementIsNotEscapable pins the accepted cost of the flat
// form: the separator carries no escape, so an element holding a comma has to
// come from the primary config source. A quoting rule of this project's own is
// exactly what the flat form is meant to avoid.
func TestCommaInsideElementIsNotEscapable(t *testing.T) {
	item := &manifestItem{path: "hosts", typ: reflect.TypeFor[[]string](), kind: kindStringList}
	for _, written := range []string{`a\,b`, `"a,b"`} {
		got, err := coerceText("hosts", written, item)
		if err != nil {
			t.Fatalf("converting %q failed: %v", written, err)
		}
		if len(got.([]string)) != 2 {
			t.Fatalf("%q produced %#v, and the separator was expected to split it in two "+
				"whatever quoting was attempted", written, got)
		}
	}
}

// TestEmbeddedFieldInsideContainerElement pins that the keys of a container
// element address an embedded field directly, the way the manifest expansion
// and Decode address it. A config file would otherwise read differently inside
// a container than outside one.
func TestEmbeddedFieldInsideContainerElement(t *testing.T) {
	type element struct {
		EmbeddedOptions
		Port int
	}
	list := coerceTo[[]element](t, []any{map[string]any{"addr": "a:1", "port": float64(1)}})
	want := []element{{EmbeddedOptions: EmbeddedOptions{Addr: "a:1"}, Port: 1}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("the list of structs converted to %#v, want %#v", list, want)
	}
}

// TestContainersFromPrimaryConverted covers the shapes only the primary source
// can give: a map with a concrete value type, and a list of structs whose keys
// are derived by the same rule as a path segment.
func TestContainersFromPrimaryConverted(t *testing.T) {
	got := coerceTo[map[string]int](t, map[string]any{"a": float64(1), "b": float64(2)})
	if want := map[string]int{"a": 1, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the map converted to %#v, want %#v", got, want)
	}

	type endpoint struct {
		Addr     string
		PoolSize int    `config:"pool"`
		Hidden   string `json:"-"`
	}
	list := coerceTo[[]endpoint](t, []any{
		map[string]any{"addr": "a:1", "pool": float64(3), "-": "ignored", "stray": true},
	})
	want := []endpoint{{Addr: "a:1", PoolSize: 3}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("the list of structs converted to %#v, want %#v", list, want)
	}
}

// TestConvertedContainersDoNotAliasTheSource pins that a converted map is a
// fresh one: two holders of the same value must not be able to write into each
// other.
func TestConvertedContainersDoNotAliasTheSource(t *testing.T) {
	source := map[string]string{"a": "1"}
	got := coerceTo[map[string]string](t, source)
	got["a"] = "changed"
	if source["a"] != "1" {
		t.Fatal("writing into the converted map reached the value it was converted from")
	}
}

func TestTypeMismatchNamesPathAndType(t *testing.T) {
	_, err := coerce("cache.ttl", "abc", reflect.TypeFor[time.Duration]())
	if !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("the conversion returned %v, want ErrTypeMismatch", err)
	}
	for _, want := range []string{"cache.ttl", "time.Duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the rejection reads %q, which does not name %q", err, want)
		}
	}
}

// TestPointerFieldsAreAllocated pins that a source giving a value for a
// pointer field is asking for one to exist.
func TestPointerFieldsAreAllocated(t *testing.T) {
	got := coerceTo[*int](t, float64(5))
	if got == nil || *got != 5 {
		t.Fatalf("the value converted to %v, want a pointer to 5", got)
	}
	stamp := coerceTo[*time.Time](t, "2026-09-13T10:30:00Z")
	if stamp == nil || stamp.Year() != 2026 {
		t.Fatalf("the timestamp converted to %v, want a pointer to it", stamp)
	}
}
