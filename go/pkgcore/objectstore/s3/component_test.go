package s3

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor through the component
// contract: the naming convention, the decodable schema, the typed tokens.
func TestComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, objectStoreS3Component)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if objectStoreS3Component.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", objectStoreS3Component.Capabilities, Capabilities)
	}
}

// TestComponent_ConstructsThroughTheRegistry selects the component in a real
// assembly and drives Prepare and Construct: nothing is dialed (minio-go
// connects lazily), the product resolves through Get at the module's
// contract type, and the minio-go client exposes no close, so the component
// declares no Close and the registry's Close is a no-op for it.
func TestComponent_ConstructsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"objectstore.s3": map[string]any{
				"endpoint":   "objects.component.test",
				"bucket":     "component",
				"access_key": "component-key",
				"secret_key": "component-secret",
			},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	if _, err := pkgcore.Get[pkgcore.ObjectStore](reg); err != nil {
		t.Errorf("Get[pkgcore.ObjectStore] error = %v, want the constructed store", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Errorf("Close() error = %v, want nil: this component owns nothing closable", err)
	}
}

// TestComponent_MissingRequiredSettingFailsConstruction pins the error path
// the flat adapter already promises for this seam: a configuration missing
// one of the four required settings fails Construct with a named error
// instead of panicking through the constructor.
func TestComponent_MissingRequiredSettingFailsConstruction(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"objectstore.s3": map[string]any{
				"endpoint":   "objects.component.test",
				"access_key": "component-key",
				"secret_key": "component-secret",
			},
		},
	}))

	if err := reg.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	err := reg.Construct(context.Background())
	if err == nil {
		t.Fatal("Construct() without a bucket succeeded, want the missing-setting error")
	}
	if !strings.Contains(err.Error(), "Bucket") {
		t.Errorf("Construct() error = %v, want it to name the missing required setting", err)
	}
}
