package aigateway

import (
	"context"
	"slices"
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

// TestComponent_SelfDescribesItsSystemPurposes pins the module's one audited
// purpose as descriptor data: the assembly registers the declaration when it
// closes its Init stage, so the purpose is registered with no help from the
// module's Register call.
func TestComponent_SelfDescribesItsSystemPurposes(t *testing.T) {
	want := []pkgcore.SystemPurpose{SystemPurposeCredentialWrite}
	if !slices.Equal(aiGatewayComponent.SystemPurposes, want) {
		t.Fatalf("component SystemPurposes = %v, want %v", aiGatewayComponent.SystemPurposes, want)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so its permissions and HTTP mount land in the
// assembly's own seats and the gateway takes the assembly's own declaration
// face for its call-time KVStore reads.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, aiGatewayComponent,
		dbtest.NewSQLite(t),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionWrite, PermissionManagePlatform} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	if routes := reg.Routes.Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}
	// No queue and no storage product were provided: Init must claim no
	// image-generation handler, the chat-only shape.
	if _, ok := reg.Jobs.Handlers()[TaskTypeImageGenerate]; ok {
		t.Error("Init claimed the image-generation handler without a queue and storage")
	}
	if m.gateway.host == nil {
		t.Error("the gateway did not take the assembly's declaration face")
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
}
