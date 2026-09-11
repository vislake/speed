package demo

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared locales asset.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, demoComponent)
}

// TestComponent_NewBuildsTheModule drives the component's New, which needs
// nothing from the registry, and pins that it produces the module.
func TestComponent_NewBuildsTheModule(t *testing.T) {
	instance, err := demoComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *demo.Module", instance, instance)
	}
	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
}
