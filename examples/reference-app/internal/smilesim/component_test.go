package smilesim

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/examples/reference-app/internal/smilesim/migrations"
)

// TestComponent_DeclaresTheSchemaAsset pins the descriptor's contribution:
// the well-formed contract, the module the migration ledger keys the set
// under, the migration set itself (the same embed the FS package carries, so
// the applied files are the ones beside them), the database requirement that
// makes a database-less selection fail by name, and the capability a
// distributed composition requires.
func TestComponent_DeclaresTheSchemaAsset(t *testing.T) {
	componenttest.AssertWellFormed(t, smilesimComponent)

	if smilesimComponent.Name != "smilesim" || smilesimComponent.Module != "smilesim" {
		t.Errorf("component identity = %q/%q, want smilesim/smilesim", smilesimComponent.Name, smilesimComponent.Module)
	}
	if smilesimComponent.Migrations != migrations.FS {
		t.Error("the component does not carry internal/smilesim/migrations' FS; the set would never reach the ledger")
	}
	if len(smilesimComponent.Requires) != 1 || smilesimComponent.Requires[0].Token != (*gorm.DB)(nil) || smilesimComponent.Requires[0].Optional {
		t.Errorf("Requires = %v, want one mandatory (*gorm.DB) requirement", smilesimComponent.Requires)
	}
	if smilesimComponent.Capabilities&pkgcore.MultiReplicaSafe == 0 {
		t.Errorf("Capabilities = %v, want MultiReplicaSafe declared", smilesimComponent.Capabilities)
	}
}

// TestComponent_NewConstructsItsCarrier drives the required callback: a
// non-nil product, since a nil one would fail every dependent requirement
// with a present-but-nil value instead.
func TestComponent_NewConstructsItsCarrier(t *testing.T) {
	instance, err := smilesimComponent.New(t.Context(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := instance.(*smilesimSchemaCarrier); !ok {
		t.Fatalf("New returned %T, want the component's own *smilesimSchemaCarrier", instance)
	}
}
