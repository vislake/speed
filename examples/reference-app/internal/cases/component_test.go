package cases

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/examples/reference-app/internal/cases/migrations"
)

// TestComponent_DeclaresTheSchemaAsset pins the descriptor's contribution:
// the well-formed contract, the module the migration ledger keys the set
// under, the migration set itself (the same embed the FS package carries, so
// the applied files are the ones beside them), the database requirement that
// makes a database-less selection fail by name, and the capability a
// distributed composition requires.
func TestComponent_DeclaresTheSchemaAsset(t *testing.T) {
	componenttest.AssertWellFormed(t, casesComponent)

	if casesComponent.Name != "cases" || casesComponent.Module != "cases" {
		t.Errorf("component identity = %q/%q, want cases/cases", casesComponent.Name, casesComponent.Module)
	}
	if casesComponent.Migrations != migrations.FS {
		t.Error("the component does not carry internal/cases/migrations' FS; the set would never reach the ledger")
	}
	if len(casesComponent.Requires) != 1 || casesComponent.Requires[0].Token != (*gorm.DB)(nil) || casesComponent.Requires[0].Optional {
		t.Errorf("Requires = %v, want one mandatory (*gorm.DB) requirement", casesComponent.Requires)
	}
	if casesComponent.Capabilities&pkgcore.MultiReplicaSafe == 0 {
		t.Errorf("Capabilities = %v, want MultiReplicaSafe declared", casesComponent.Capabilities)
	}
}

// TestComponent_NewConstructsItsCarrier drives the required callback: a
// non-nil product, since a nil one would fail every dependent requirement
// with a present-but-nil value instead.
func TestComponent_NewConstructsItsCarrier(t *testing.T) {
	instance, err := casesComponent.New(t.Context(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := instance.(*casesSchemaCarrier); !ok {
		t.Fatalf("New returned %T, want the component's own *casesSchemaCarrier", instance)
	}
}
