// Package assemblytest drives a module's declaration turn through a
// minimal, real component assembly for this app's per-module test suites.
// It is leaf-shaped -- pkgcore and testing only -- so a module's test
// binary can import it without reaching back into the packages that import
// the module under test (the same rule testutil/dbschema follows for the
// migration suites). It is test-only: nothing in this app's executable
// code may import it.
package assemblytest

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// Declarer is one module's declaration body, addressed the way a module's
// Register method is: what AssembleWithProviders drives inside its Init
// stage.
type Declarer interface {
	Register(*pkgcore.ComponentRegistry) error
}

// AssembleWithProviders drives module's declaration turn inside a real Init
// stage over a fresh registry whose strict composition selects exactly the
// declaration and the given provider components. It is the component-face
// shape a module's provider-facing Gateway resolves through, driven without
// the full application assembly: the module's Register attaches the
// registry to its Gateway (and claims whatever seats it declares), and each
// provider component is selectable by the name its route carries. The
// completed registry is returned; the caller reads the module's own Gateway
// and Credentials off the module value it handed in.
func AssembleWithProviders(t *testing.T, module Declarer, providers ...pkgcore.Component) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name: "testutil.declare",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
		Init: func(_ context.Context, r *pkgcore.ComponentRegistry, _ any) error {
			return module.Register(r)
		},
	}); err != nil {
		t.Fatalf("assemblytest: register declaration component: %v", err)
	}
	selected := map[string]any{"testutil.declare": nil}
	for _, c := range providers {
		if err := reg.Register(c); err != nil {
			t.Fatalf("assemblytest: register provider component %q: %v", c.Name, err)
		}
		selected[c.Name] = nil
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{"components": selected, "strict": true}))
	ctx := context.Background()
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("assemblytest: prepare: %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("assemblytest: construct: %v", err)
	}
	if err := reg.Verify(ctx); err != nil {
		t.Fatalf("assemblytest: verify: %v", err)
	}
	if err := reg.Init(ctx); err != nil {
		t.Fatalf("assemblytest: declare: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}
