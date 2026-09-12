package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// describeFixture is the schema the projection tests read: one field per
// option of the declaration vocabulary, a nested mapping, a scalar struct
// leaf and a json "-" field.
type describeFixture struct {
	Host      string        `json:"host" config:"expose,env=APP_LISTEN_ADDR,group=network"`
	CipherKey []byte        `json:"cipher_key" config:"derive,required,sensitive,group=security"`
	CacheTTL  int           `json:"cache_ttl"`
	Plain     string        `json:"plain" config:"required"`
	Timeout   time.Duration `json:"timeout" config:"expose"`
	Stamp     time.Time     `json:"stamp"`
	Backend   struct {
		Addr string `json:"addr" config:"expose"`
		Port int    `json:"port"`
	} `json:"backend"`
	Skipped string `json:"skipped" config:"-"`
	Hidden  string `json:"-"`
}

// describeSummary returns the summary for one key, failing when the
// projection described no such field.
func describeSummary(t *testing.T, fields []FieldSummary, key string) FieldSummary {
	t.Helper()
	for _, f := range fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no summary for key %q in %v", key, describeKeys(fields))
	return FieldSummary{}
}

// describeKeys lists the projected keys, for failure output.
func describeKeys(fields []FieldSummary) []string {
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.Key)
	}
	return keys
}

// TestDescribe_ProjectsTheDeclarationVocabular pins the projection's shape
// field by field: the local key path in the block's own spelling, the Go
// field name, the option set, the documentation markers and the rendered
// type -- and pins that a json "-" field and the children of a skipped
// container are not described at all.
func TestDescribe_ProjectsTheDeclarationVocabular(t *testing.T) {
	t.Parallel()

	fields, err := Describe((*describeFixture)(nil))
	if err != nil {
		t.Fatalf("Describe() error = %v, want nil", err)
	}
	want := []string{"host", "cipher_key", "cache_ttl", "plain", "timeout", "stamp", "backend.addr", "backend.port", "skipped"}
	if got := describeKeys(fields); !equalStrings(got, want) {
		t.Fatalf("Describe() keys = %v, want %v", got, want)
	}

	host := describeSummary(t, fields, "host")
	if host.Name != "Host" || host.Type.String() != "string" || host.TypeName() != "string" {
		t.Errorf("host = %+v, want the string field %s", host, "Host")
	}
	if !host.Expose || host.Derive || host.Required || host.Sensitive || host.Skip {
		t.Errorf("host options = %+v, want expose alone", host)
	}
	if host.Env != "APP_LISTEN_ADDR" || host.Group != "network" {
		t.Errorf("host pin/group = %q/%q, want APP_LISTEN_ADDR/network", host.Env, host.Group)
	}

	key := describeSummary(t, fields, "cipher_key")
	if key.TypeName() != "[]byte" {
		t.Errorf("cipher_key TypeName() = %q, want the operator-facing []byte spelling", key.TypeName())
	}
	if !key.Derive || !key.Expose || !key.Required || !key.Sensitive {
		t.Errorf("cipher_key options = %+v, want derive/required/sensitive over the implied expose", key)
	}

	plain := describeSummary(t, fields, "plain")
	if plain.Expose || plain.Derive {
		t.Errorf("plain = %+v, want a block-only field", plain)
	}
	if !plain.Required {
		t.Errorf("plain = %+v, want the required option", plain)
	}

	if ttl := describeSummary(t, fields, "cache_ttl"); ttl.TypeName() != "int" || ttl.Expose {
		t.Errorf("cache_ttl = %+v, want an unexposed int", ttl)
	}
	if stamp := describeSummary(t, fields, "stamp"); stamp.Type != timeType {
		t.Errorf("stamp type = %v, want the scalar struct described as one leaf", stamp.Type)
	}
	if addr := describeSummary(t, fields, "backend.addr"); !addr.Expose || addr.Name != "Addr" {
		t.Errorf("backend.addr = %+v, want the nested exposed leaf with its own Go name", addr)
	}
	if port := describeSummary(t, fields, "backend.port"); port.Expose {
		t.Errorf("backend.port = %+v, want the nested untagged field block-only", port)
	}

	skipped := describeSummary(t, fields, "skipped")
	if !skipped.Skip || skipped.Expose || skipped.Derive || skipped.Required || skipped.Sensitive || skipped.Env != "" || skipped.Group != "" {
		t.Errorf("skipped = %+v, want the skip option alone", skipped)
	}
}

// TestDescribe_SkippedContainerKeepsItsSubtreeOut pins the skip rule for a
// container: the field is described as one skipped field and its children are
// not described at all, so no source can name them.
func TestDescribe_SkippedContainerKeepsItsSubtreeOut(t *testing.T) {
	t.Parallel()

	type schema struct {
		Managed struct {
			Address string `json:"address" config:"expose"`
		} `json:"managed" config:"-"`
		Open struct {
			Address string `json:"address" config:"expose"`
		} `json:"open"`
	}
	fields, err := Describe((*schema)(nil))
	if err != nil {
		t.Fatalf("Describe() error = %v, want nil", err)
	}
	want := []string{"managed", "open.address"}
	if got := describeKeys(fields); !equalStrings(got, want) {
		t.Fatalf("Describe() keys = %v, want %v", got, want)
	}
	if managed := describeSummary(t, fields, "managed"); !managed.Skip {
		t.Errorf("managed = %+v, want the skipped container described as one skipped field", managed)
	}
}

// TestDescribe_NilAndShape pins the degenerate targets: nil describes as an
// empty list, and anything but a pointer to a struct fails with
// ErrInvalidTarget.
func TestDescribe_NilAndShape(t *testing.T) {
	t.Parallel()

	fields, err := Describe(nil)
	if err != nil || fields != nil {
		t.Fatalf("Describe(nil) = %v, %v, want no fields and no error", fields, err)
	}
	if _, err := Describe(describeFixture{}); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("Describe(struct value) error = %v, want ErrInvalidTarget", err)
	}
	if _, err := Describe(&[]string{}); !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("Describe(pointer to slice) error = %v, want ErrInvalidTarget", err)
	}
}

// TestDescribe_TagFailures pins the closed option set: every malformed
// declaration fails the description naming the field, because a declaration
// that says something no source could resolve must not be projected as if it
// resolved.
func TestDescribe_TagFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema any
		frag   string
	}{
		{
			name: "an unknown option",
			schema: (*struct {
				Field string `json:"field" config:"frobnicate"`
			})(nil),
			frag: `"frobnicate"`,
		},
		{
			name: "derive on a field that is not []byte",
			schema: (*struct {
				Field string `json:"field" config:"derive"`
			})(nil),
			frag: "only a []byte field",
		},
		{
			name: "a repeated derive option",
			schema: (*struct {
				Field []byte `json:"field" config:"derive,derive"`
			})(nil),
			frag: "repeats",
		},
		{
			name: "a malformed env pin",
			schema: (*struct {
				Field string `json:"field" config:"env="`
			})(nil),
			frag: "env=NAME",
		},
		{
			name: "a repeated env pin",
			schema: (*struct {
				Field string `json:"field" config:"env=ONE,env=TWO"`
			})(nil),
			frag: "env=NAME",
		},
		{
			name: "the skip option beside another option",
			schema: (*struct {
				Field string `json:"field" config:"-,required"`
			})(nil),
			frag: "beside other options",
		},
		{
			name: "options on a container field",
			schema: (*struct {
				Field struct {
					Inner string `json:"inner"`
				} `json:"field" config:"required"`
			})(nil),
			frag: "nested struct",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Describe(tt.schema)
			if !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("Describe() error = %v, want ErrInvalidTarget", err)
			}
			if !strings.Contains(err.Error(), tt.frag) {
				t.Errorf("Describe() error = %v, want it to carry %q", err, tt.frag)
			}
		})
	}
}

// TestDescribe_TypeNameRendersKeyMaterial pins the documentation spelling:
// key material renders as "[]byte" while every other type renders as its own
// string, so a renderer shared across the writable and read-only ends of the
// contract cannot disagree with the descriptor spelling.
func TestDescribe_TypeNameRendersKeyMaterial(t *testing.T) {
	t.Parallel()

	if got := (FieldSummary{Type: byteSliceType}).TypeName(); got != "[]byte" {
		t.Errorf("TypeName() = %q, want []byte", got)
	}
	if got := (FieldSummary{Type: timeType}).TypeName(); got != "time.Time" {
		t.Errorf("TypeName() = %q, want time.Time", got)
	}
	if got := (FieldSummary{}).TypeName(); got != "" {
		t.Errorf("TypeName() = %q, want empty for a zero summary", got)
	}
}

// equalStrings compares two string slices element by element.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
