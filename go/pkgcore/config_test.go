package pkgcore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// decodeMode is a named string type a decode target uses to prove named
// types convert.
type decodeMode string

// decodeNested is the nested struct of the decode fixture.
type decodeNested struct {
	Port int `json:"port"`
}

// decodeTarget is the fixture struct the strict-decode tests populate.
type decodeTarget struct {
	Host     string         `json:"host"`
	Retries  int            `json:"retries"`
	Small    int8           `json:"small"`
	Timeout  time.Duration  `json:"timeout"`
	Enabled  bool           `json:"enabled"`
	Mode     decodeMode     `json:"mode"`
	Trusted  []string       `json:"trusted"`
	Labels   map[string]int `json:"labels"`
	Nested   decodeNested   `json:"nested"`
	Untagged string
	Any      any       `json:"any"`
	Pointer  *int      `json:"pointer"`
	When     time.Time `json:"when"`
	Skipped  string    `json:"-"`
}

func TestNewComponentConfigSortsKeysAndWithPreservesOrder(t *testing.T) {
	fromMap := NewComponentConfig(map[string]any{"b": 2, "a": 1, "c": 3})
	if got, want := fromMap.Keys(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NewComponentConfig keys = %v, want sorted %v", got, want)
	}
	if fromMap.Len() != 3 {
		t.Errorf("Len() = %d, want 3", fromMap.Len())
	}

	ordered := NewComponentConfig(nil).With("z", 1).With("a", 2).With("m", 3)
	if got, want := ordered.Keys(), []string{"z", "a", "m"}; !reflect.DeepEqual(got, want) {
		t.Errorf("With keys = %v, want call order %v", got, want)
	}

	replaced := ordered.With("a", 9)
	if got, want := replaced.Keys(), []string{"z", "a", "m"}; !reflect.DeepEqual(got, want) {
		t.Errorf("With replacement keys = %v, want position preserved %v", got, want)
	}
	if v, _ := Value[int](replaced, "a"); v != 9 {
		t.Errorf("replaced value = %d, want 9", v)
	}
	if v, _ := Value[int](ordered, "a"); v != 2 {
		t.Errorf("With mutated its base: base value = %d, want 2", v)
	}
	if got, want := NewComponentConfig(nil).With("b", 1).With("a", 2).Keys(), []string{"b", "a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("With on empty config keys = %v, want %v", got, want)
	}
}

func TestComponentConfigDecodePopulatesEveryShape(t *testing.T) {
	cfg := NewComponentConfig(map[string]any{
		"host":     "smtp.example.com",
		"retries":  "3",         // text into int
		"timeout":  "5m",        // text into time.Duration
		"enabled":  "true",      // text into bool
		"mode":     "immediate", // named string type
		"trusted":  []any{"10.0.0.1", "10.0.0.2"},
		"labels":   map[string]any{"tier": 2},
		"nested":   map[string]any{"port": 2525},
		"Untagged": "by field name",
		"any":      []int{1, 2},
		"pointer":  7,
		"when":     "2026-01-02T03:04:05Z",
	})

	var target decodeTarget
	if err := cfg.Decode(&target); err != nil {
		t.Fatalf("Decode = %v, want nil", err)
	}
	if target.Host != "smtp.example.com" {
		t.Errorf("Host = %q", target.Host)
	}
	if target.Retries != 3 {
		t.Errorf("Retries = %d, want 3", target.Retries)
	}
	if target.Timeout != 5*time.Minute {
		t.Errorf("Timeout = %v, want 5m", target.Timeout)
	}
	if !target.Enabled {
		t.Errorf("Enabled = false, want true")
	}
	if target.Mode != "immediate" {
		t.Errorf("Mode = %q", target.Mode)
	}
	if !reflect.DeepEqual(target.Trusted, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Errorf("Trusted = %v", target.Trusted)
	}
	if !reflect.DeepEqual(target.Labels, map[string]int{"tier": 2}) {
		t.Errorf("Labels = %v", target.Labels)
	}
	if target.Nested.Port != 2525 {
		t.Errorf("Nested.Port = %d", target.Nested.Port)
	}
	if target.Untagged != "by field name" {
		t.Errorf("Untagged = %q", target.Untagged)
	}
	if !reflect.DeepEqual(target.Any, []int{1, 2}) {
		t.Errorf("Any = %#v", target.Any)
	}
	if target.Pointer == nil || *target.Pointer != 7 {
		t.Errorf("Pointer = %v", target.Pointer)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !target.When.Equal(want) {
		t.Errorf("When = %v, want %v", target.When, want)
	}

	// Case-insensitive matching, and an empty config leaving every field at
	// its zero value.
	var insensitive decodeTarget
	if err := NewComponentConfig(map[string]any{"HOST": "h"}).Decode(&insensitive); err != nil {
		t.Fatalf("case-insensitive Decode = %v, want nil", err)
	}
	if insensitive.Host != "h" {
		t.Errorf("Host = %q, want %q", insensitive.Host, "h")
	}

	var empty decodeTarget
	empty.Host = "pre-set"
	if err := NewComponentConfig(nil).Decode(&empty); err != nil {
		t.Fatalf("empty Decode = %v, want nil", err)
	}
	if empty.Host != "pre-set" {
		t.Errorf("empty Decode overwrote a field: Host = %q", empty.Host)
	}
}

func TestComponentConfigDecodeUnknownKeys(t *testing.T) {
	var target decodeTarget

	err := NewComponentConfig(map[string]any{"prot": 2525, "hots": "x"}).Decode(&target)
	if !errors.Is(err, ErrUnknownConfigKey) {
		t.Fatalf("Decode = %v, want ErrUnknownConfigKey", err)
	}
	for _, want := range []string{`"prot"`, `"hots"`, "are not declared", "accepted:", "host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}

	// A json:"-" field is not accepted, and the accepted list spells the
	// declared keys in declaration order.
	err = NewComponentConfig(map[string]any{"skipped": "x"}).Decode(&target)
	if !errors.Is(err, ErrUnknownConfigKey) || !strings.Contains(err.Error(), `"skipped"`) {
		t.Errorf("Decode of a json-\"-\" key = %v, want unknown-key error naming it", err)
	}

	// A nested unknown key is reported with its dotted path.
	err = NewComponentConfig(map[string]any{"nested": map[string]any{"prt": 1}}).Decode(&target)
	if !errors.Is(err, ErrUnknownConfigKey) {
		t.Fatalf("nested unknown key = %v, want ErrUnknownConfigKey", err)
	}
	if !strings.Contains(err.Error(), `"nested.prt"`) {
		t.Errorf("nested unknown-key error %q does not carry the dotted path", err)
	}
}

func TestComponentConfigDecodeRejectsUnfitValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  any
		want string
	}{
		{"text is not an integer", "retries", "abc", "not a valid integer"},
		{"fractional float into int", "retries", 1.5, "not a whole number"},
		{"text is not a duration", "timeout", "soon", "not a valid duration"},
		{"text is not a bool", "enabled", "maybe", "not a valid bool"},
		{"map into string", "host", map[string]any{"a": 1}, "cannot be assigned"},
		{"int into slice", "trusted", 5, "is not a list"},
		{"string into mapping", "labels", "x", "is not a mapping"},
		{"overflow", "small", 300, "does not fit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var target decodeTarget
			err := NewComponentConfig(map[string]any{tc.key: tc.val}).Decode(&target)
			if err == nil {
				t.Fatalf("Decode accepted %v for %q", tc.val, tc.key)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestComponentConfigDecodeTargetShape(t *testing.T) {
	cfg := NewComponentConfig(nil)
	for _, target := range []any{nil, decodeTarget{}} {
		if err := cfg.Decode(target); err == nil {
			t.Errorf("Decode(%T) = nil, want an error for a non-nil-pointer-to-struct target", target)
		}
	}
	if err := cfg.Decode(&decodeTarget{}); err != nil {
		t.Errorf("Decode(&decodeTarget{}) = %v, want nil", err)
	}
}

func TestComponentConfigValue(t *testing.T) {
	cfg := NewComponentConfig(map[string]any{"host": "h", "port": 8080})

	if v, err := Value[string](cfg, "host"); err != nil || v != "h" {
		t.Errorf(`Value[string]("host") = (%q, %v), want ("h", nil)`, v, err)
	}
	if v, err := Value[int](cfg, "port"); err != nil || v != 8080 {
		t.Errorf(`Value[int]("port") = (%d, %v), want (8080, nil)`, v, err)
	}
	if _, err := Value[string](cfg, "missing"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf(`Value[string]("missing") = %v, want an error naming the key`, err)
	}
	if _, err := Value[int](cfg, "host"); err == nil {
		t.Error(`Value[int]("host") = nil error, want a conversion failure`)
	}
}

func TestParseComposition(t *testing.T) {
	t.Run("empty config is standalone and non-strict", func(t *testing.T) {
		comp, err := parseComposition(NewComponentConfig(nil))
		if err != nil {
			t.Fatalf("parseComposition = %v, want nil", err)
		}
		if comp.mode != DeploymentModeStandalone {
			t.Errorf("mode = %q, want standalone", comp.mode)
		}
		if comp.strict {
			t.Error("strict = true, want false")
		}
		if comp.components.Len() != 0 {
			t.Errorf("components = %v, want empty", comp.components.Keys())
		}
	})

	t.Run("full composition", func(t *testing.T) {
		cfg := NewComponentConfig(map[string]any{
			"deployment": "distributed",
			"strict":     true,
			"components": map[string]any{"authn": nil},
		})
		comp, err := parseComposition(cfg)
		if err != nil {
			t.Fatalf("parseComposition = %v, want nil", err)
		}
		if comp.mode != DeploymentModeDistributed || !comp.strict {
			t.Errorf("mode = %q, strict = %v; want distributed/true", comp.mode, comp.strict)
		}
		if _, ok := comp.components.lookup("authn"); !ok {
			t.Errorf("components = %v, want authn present", comp.components.Keys())
		}
	})

	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := parseComposition(NewComponentConfig(map[string]any{"deploymnet": "standalone"}))
		if !errors.Is(err, ErrUnknownConfigKey) {
			t.Fatalf("parseComposition = %v, want ErrUnknownConfigKey", err)
		}
		for _, want := range []string{`"deploymnet"`, "composition configuration", "stage prepare", "accepted: components, deployment, strict"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("invalid deployment value", func(t *testing.T) {
		_, err := parseComposition(NewComponentConfig(map[string]any{"deployment": "nonsense"}))
		if !errors.Is(err, ErrInvalidDeploymentMode) {
			t.Fatalf("parseComposition = %v, want ErrInvalidDeploymentMode", err)
		}
		if !strings.Contains(err.Error(), "stage prepare") {
			t.Errorf("error %q does not name the stage", err)
		}
	})

	t.Run("non-string deployment", func(t *testing.T) {
		if _, err := parseComposition(NewComponentConfig(map[string]any{"deployment": 7})); err == nil {
			t.Fatal("parseComposition accepted a non-string deployment")
		}
	})

	t.Run("non-bool strict", func(t *testing.T) {
		if _, err := parseComposition(NewComponentConfig(map[string]any{"strict": "yes"})); err == nil {
			t.Fatal("parseComposition accepted a non-bool strict")
		}
	})

	t.Run("non-mapping components", func(t *testing.T) {
		if _, err := parseComposition(NewComponentConfig(map[string]any{"components": 5})); err == nil {
			t.Fatal("parseComposition accepted a non-mapping components value")
		}
	})

	t.Run("nested ComponentConfig as components", func(t *testing.T) {
		comp, err := parseComposition(NewComponentConfig(nil).With("components", NewComponentConfig(nil).With("a", nil)))
		if err != nil {
			t.Fatalf("parseComposition = %v, want nil", err)
		}
		if got, want := comp.components.Keys(), []string{"a"}; !reflect.DeepEqual(got, want) {
			t.Errorf("components keys = %v, want %v", got, want)
		}
	})
}
