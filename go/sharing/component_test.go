package sharing

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared assets.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, sharingComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database plus every optional product, and pins that
// each optional seam reached the built module when present.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))
	reg.Put(&recordingQueue{})
	reg.Put(fakeTenantConfigReader{d: 1234, ok: true})
	reg.Put(fakeResourceResolver{mime: "text/plain", body: "hello"})

	instance, err := sharingComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *sharing.Module", instance, instance)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
	if m.cfg == nil {
		t.Error("cfg is nil, want the registry's reader wired through WithTenantConfigReader")
	}
	if m.resolver == nil {
		t.Error("resolver is nil, want the registry's resolver wired through WithResourceResolver")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal construction: only the database is needed.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))

	instance, err := sharingComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *sharing.Module", instance, instance)
	}
	if m.queue != nil || m.cfg != nil || m.resolver != nil {
		t.Errorf("optional seams = (%v, %v, %v), want all nil without providers", m.queue, m.cfg, m.resolver)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := sharingComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
