package pkgcore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Fixture types shared by the component test files.
type (
	// compTokenA and compTokenB are two distinct contract types a fixture
	// component can provide or require.
	compTokenA struct{}
	// compTokenB is the second distinct contract type.
	compTokenB struct{}
	// compSpreader is an interface contract: a token (*compSpreader)(nil)
	// matches any product implementing it.
	compSpreader interface{ spread() string }
	// compSpreadImpl implements compSpreader.
	compSpreadImpl struct{}
	// compSchema is a config struct a fixture descriptor uses as its
	// ConfigSchema.
	compSchema struct {
		Host string `json:"host"`
	}
)

func (compSpreadImpl) spread() string { return "spread" }

// noopNew is a New callback that produces a *compTokenA product.
func noopNew(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
	return &compTokenA{}, nil
}

func TestRegisterGlobalAcceptsAndRejectsDuplicates(t *testing.T) {
	c := Component{Name: "test.global.register", New: noopNew}
	if err := Register(c); err != nil {
		t.Fatalf("Register(%q) = %v, want nil", c.Name, err)
	}

	err := Register(c)
	if !errors.Is(err, ErrDuplicateComponent) {
		t.Fatalf("second Register = %v, want ErrDuplicateComponent", err)
	}
	for _, want := range []string{`"test.global.register"`, "stage registration", "already registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("duplicate error %q does not carry element %q", err, want)
		}
	}

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("MustRegister on a duplicate name did not panic")
		}
		if got := recovered.(string); !strings.Contains(got, "test.global.register") {
			t.Errorf("MustRegister panic %q does not name the component", got)
		}
	}()
	MustRegister(c)
}

func TestRegisterGlobalSeesTheComponent(t *testing.T) {
	name := "test.global.visible"
	if err := Register(Component{Name: name, New: noopNew}); err != nil {
		t.Fatalf("Register(%q) = %v, want nil", name, err)
	}
	if _, ok := globalComponent(name); !ok {
		t.Errorf("globalComponent(%q) not found after Register", name)
	}
	reg := NewComponentRegistry()
	if _, ok := reg.registered(name); !ok {
		t.Errorf("NewComponentRegistry did not seed the globally registered component %q", name)
	}
}

func TestValidateComponentRejectsMalformedDescriptors(t *testing.T) {
	valid := Component{Name: "test.validate", New: noopNew}
	if err := validateComponent(valid); err != nil {
		t.Fatalf("validateComponent(valid) = %v, want nil", err)
	}

	cases := []struct {
		name string
		c    Component
		want string
	}{
		{
			name: "empty name",
			c:    Component{New: noopNew},
			want: "empty name",
		},
		{
			name: "no New callback",
			c:    Component{Name: "test.validate.nonew"},
			want: "no New callback",
		},
		{
			name: "schema not a pointer",
			c:    Component{Name: "test.validate.schema", New: noopNew, ConfigSchema: compSchema{}},
			want: "ConfigSchema",
		},
		{
			name: "schema pointer to non-struct",
			c:    Component{Name: "test.validate.schema2", New: noopNew, ConfigSchema: (*string)(nil)},
			want: "ConfigSchema",
		},
		{
			name: "requirement token untyped",
			c:    Component{Name: "test.validate.req", New: noopNew, Requires: []Requirement{{Token: nil}}},
			want: "an untyped nil is not a contract token",
		},
		{
			name: "requirement token not a pointer",
			c:    Component{Name: "test.validate.req2", New: noopNew, Requires: []Requirement{{Token: compSchema{}}}},
			want: "is not a contract token",
		},
		{
			name: "Provides entry not a pointer",
			c:    Component{Name: "test.validate.prov", New: noopNew, Provides: []any{"nope"}},
			want: "Provides entry 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateComponent(tc.c)
			if !errors.Is(err, ErrInvalidComponent) {
				t.Fatalf("validateComponent = %v, want ErrInvalidComponent", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateComponentAcceptsFullDescriptor(t *testing.T) {
	c := Component{
		Name:         "test.validate.full",
		Module:       "test",
		Prepare:      func(context.Context, *ComponentRegistry) error { return nil },
		New:          noopNew,
		Verify:       func(context.Context, *ComponentRegistry, any) error { return nil },
		Init:         func(context.Context, *ComponentRegistry, any) error { return nil },
		Start:        func(context.Context, *ComponentRegistry, any) error { return nil },
		Stop:         func(context.Context, *ComponentRegistry, any) error { return nil },
		Close:        func(context.Context, *ComponentRegistry, any) error { return nil },
		Requires:     []Requirement{{Token: (*compTokenA)(nil)}},
		Provides:     []any{(*compTokenB)(nil)},
		Capabilities: MultiReplicaSafe,
		ConfigSchema: (*compSchema)(nil),
	}
	if err := validateComponent(c); err != nil {
		t.Fatalf("validateComponent(full descriptor) = %v, want nil", err)
	}
}

func TestProductMatchesToken(t *testing.T) {
	cases := []struct {
		name    string
		product any
		token   any
		want    bool
	}{
		{"pointer product, pointer token", &compTokenA{}, (*compTokenA)(nil), true},
		{"implementing product, interface-behind-pointer token", compSpreadImpl{}, (*compSpreader)(nil), true},
		{"pointer product, interface-behind-pointer token", &compSpreadImpl{}, (*compSpreader)(nil), true},
		{"unrelated product", &compTokenB{}, (*compTokenA)(nil), false},
		{"interface-behind-pointer token, non-implementing product", &compTokenA{}, (*compSpreader)(nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := productMatchesToken(reflect.TypeOf(tc.product), reflect.TypeOf(tc.token))
			if got != tc.want {
				t.Errorf("productMatchesToken(%T, %T) = %v, want %v", tc.product, tc.token, got, tc.want)
			}
		})
	}
}

func TestTokenDisplayStripsPointer(t *testing.T) {
	cases := []struct {
		token any
		want  string
	}{
		{(*compTokenA)(nil), "pkgcore.compTokenA"},
		{(*compSpreader)(nil), "pkgcore.compSpreader"},
	}
	for _, tc := range cases {
		if got := tokenDisplay(reflect.TypeOf(tc.token)); got != tc.want {
			t.Errorf("tokenDisplay(%T) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestClosestName(t *testing.T) {
	registered := []string{"authn", "org", "mailer.smtp", "mailer.console"}
	cases := []struct {
		name string
		want string
	}{
		{"authnn", "authn"},
		{"orgg", "org"},
		{"mailer.smtpp", "mailer.smtp"},
		{"completely.different", ""},
	}
	for _, tc := range cases {
		if got := closestName(tc.name, registered); got != tc.want {
			t.Errorf("closestName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
