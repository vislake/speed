package aigateway

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/storage"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared system purpose.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, aiGatewayComponent)
}

// TestComponent_NewBuildsAGatewayWithEverySeam drives the component's New
// over a registry carrying the database plus every optional product, and
// pins that image generation, the entitlement gate and the usage recorder
// were all wired.
func TestComponent_NewBuildsAGatewayWithEverySeam(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))
	reg.Put(&recordingImageQueue{})
	reg.Put(storage.NewModule(nil))
	reg.Put(EntitlementsFunc(func(context.Context, string, int64) (Decision, error) {
		return Decision{}, nil
	}))
	reg.Put(&fakeUsageRecorder{})

	instance, err := aiGatewayComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *aigateway.Module", instance, instance)
	}
	if _, ok := m.gateway.imageJobHandler(); !ok {
		t.Error("gateway has no image job handler, want image generation wired from the queue and storage products")
	}
	if m.gateway.entitlements == nil {
		t.Error("entitlements is nil, want the registry's gate wired through WithEntitlements")
	}
	if m.gateway.usage == nil {
		t.Error("usage recorder is nil, want the registry's recorder wired through WithUsageRecorder")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal, chat-only construction: only the database is needed.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))

	instance, err := aiGatewayComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *aigateway.Module", instance, instance)
	}
	if _, ok := m.gateway.imageJobHandler(); ok {
		t.Error("gateway has an image job handler without the queue and storage products, want a chat-only gateway")
	}
	if m.gateway.entitlements != nil || m.gateway.usage != nil {
		t.Errorf("optional seams = (%v, %v), want both nil without providers", m.gateway.entitlements, m.gateway.usage)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := aiGatewayComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
