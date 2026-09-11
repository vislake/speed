package componenttest

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// This file carries the test-only seat helpers: a component's declaration
// body writes into the ten declaration seats, and those seats accept writes
// only while the Init stage runs, so a test that exercises a declaration
// body has to drive a real assembly up to that stage. Both helpers below do
// exactly that -- they advance the stages of the assembly the test builds,
// and the seats open because the assembly's own Init stage opened them.
// Nothing here can make a write outside Init legal: the gate inside each
// seat remains the only gate, and a test that calls a seat after a helper
// returns sees the same refusal production sees.

// DuringInit runs declare inside a real Init stage over reg, handing it the
// assembly itself as the declaration face. Use it to exercise a declaration
// body that is not a component's own Init callback, with the seat gate
// checking the write exactly as it checks a component's.
//
// It is test-support code. It fails t when the registry refuses the
// assembly it builds or when declare returns an error, so the test's own
// assertions start from a stage the production driver builds the same way.
func DuringInit(t *testing.T, reg *pkgcore.ComponentRegistry, declare func(pkgcore.Registrar) error) {
	t.Helper()
	const name = "componenttest.declare"
	if err := reg.Register(pkgcore.Component{
		Name: name,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return declare(reg)
		},
	}); err != nil {
		t.Fatalf("componenttest: register the declaration component: %v", err)
	}
	reg.Put(strictComposition(name))
	if err := runThroughInit(context.Background(), reg); err != nil {
		t.Fatalf("componenttest: drive the stages up to Init: %v", err)
	}
}

// RunInit drives a minimal real assembly over c. It registers c -- or, when
// a descriptor of the same name is already registered on reg (a package's
// own descriptor is seeded from the global registration), drives that
// registered one after checking the caller passed the same declaration --
// plus one provider component per value, each declaring the value's own type
// as its product, so the assembly resolves c's requirements against exactly
// the values the test chose. It selects exactly those components in strict
// mode -- no auto-pull, so a requirement the values do not satisfy fails the
// Prepare stage naming the token -- and runs Prepare, Construct, Verify and
// Init, so on return the seats are closed again exactly as they are after
// the production driver's Init. The product c's New built is reachable
// through pkgcore.Get from reg, as it is after any assembly.
//
// It is test-support code. A registry mistake (a malformed descriptor, a
// mismatched duplicate) fails t; a refusal by the assembly itself is
// returned, so a test can pin the refusal.
func RunInit(t *testing.T, reg *pkgcore.ComponentRegistry, c pkgcore.Component, values ...any) error {
	t.Helper()
	if existing, ok := registeredDescriptor(reg, c.Name); ok {
		if !sameDeclarations(existing, c) {
			t.Fatalf("componenttest: %q is already registered with a different descriptor", c.Name)
		}
	} else if err := reg.Register(c); err != nil {
		t.Fatalf("componenttest: register component %q: %v", c.Name, err)
	}

	names := []string{c.Name}
	for i, value := range values {
		name := fmt.Sprintf("componenttest.value.%d", i)
		token := reflect.Zero(reflect.TypeOf(value)).Interface()
		held := value
		if err := reg.Register(pkgcore.Component{
			Name:     name,
			Provides: []any{token},
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return held, nil
			},
		}); err != nil {
			t.Fatalf("componenttest: register the provider for value %d (%T): %v", i, value, err)
		}
		names = append(names, name)
	}

	reg.Put(strictComposition(names...))
	return runThroughInit(context.Background(), reg)
}

// registeredDescriptor returns the descriptor registered on reg under name.
func registeredDescriptor(reg *pkgcore.ComponentRegistry, name string) (pkgcore.Component, bool) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == name {
			return c, true
		}
	}
	return pkgcore.Component{}, false
}

// sameDeclarations reports whether two descriptors declare the same thing:
// the static declaration fields, which is what a duplicate registration must
// agree on. The callbacks are not compared -- functions are not comparable,
// and a registered descriptor's callbacks are the ones the assembly runs.
func sameDeclarations(a, b pkgcore.Component) bool {
	return a.Module == b.Module &&
		a.Capabilities == b.Capabilities &&
		reflect.DeepEqual(a.Requires, b.Requires) &&
		reflect.DeepEqual(a.Provides, b.Provides) &&
		reflect.DeepEqual(a.BootstrapKeys, b.BootstrapKeys) &&
		reflect.DeepEqual(a.SystemPurposes, b.SystemPurposes)
}

// strictComposition builds a composition configuration selecting exactly
// names with auto-pull off, so the assembly is made of nothing but the
// components the test named.
func strictComposition(names ...string) pkgcore.ComponentConfig {
	block := pkgcore.ComponentConfig{}
	for _, name := range names {
		block = block.With(name, nil)
	}
	return pkgcore.NewComponentConfig(nil).
		With("components", block).
		With("strict", true)
}

// runThroughInit drives one assembly from Prepare through Init.
func runThroughInit(ctx context.Context, reg *pkgcore.ComponentRegistry) error {
	if err := reg.Prepare(ctx); err != nil {
		return err
	}
	if err := reg.Construct(ctx); err != nil {
		return err
	}
	if err := reg.Verify(ctx); err != nil {
		return err
	}
	return reg.Init(ctx)
}
