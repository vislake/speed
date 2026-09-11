package demo

import (
	"context"
	"slices"
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

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so both notification types land in the
// assembly's own seat.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, demoComponent); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	var keys []string
	for _, typ := range reg.Notifications.Types() {
		keys = append(keys, typ.Key)
	}
	for _, want := range []string{patientReminderNotificationType.Key, simulationReadyNotificationType.Key} {
		if !slices.Contains(keys, want) {
			t.Errorf("Notifications seat = %v, want the %q declaration", keys, want)
		}
	}
}
