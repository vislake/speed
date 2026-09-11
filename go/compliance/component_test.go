package compliance

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared system purposes.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, complianceComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database, the config module, the queue and both
// optional seams, and pins that every value reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	db := dbtest.NewSQLite(t)
	reg.Put(db)
	reg.Put(config.NewModule(db))
	reg.Put(&recordingQueue{})
	sharing := &fakeSharingCreator{}
	reg.Put(sharing)
	reg.Put(testutil.FakeTenantLister{})

	instance, err := complianceComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *compliance.Module", instance, instance)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
	if m.export.cfg == nil {
		t.Error("export config reader is nil, want the config module's handle wired through WithExportConfigReader")
	}
	if m.export.sharing != SharingCreator(sharing) {
		t.Error("export sharing is not the registry's creator, want it wired through WithSharing")
	}
	if m.retention.lister == nil {
		t.Error("retention lister is nil, want the registry's lister wired through WithTenantLister")
	}
	if m.auditRepo == nil {
		t.Error("audit repo is nil, want it built over the registry's database")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal construction: the database, the config module and the
// queue suffice.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	db := dbtest.NewSQLite(t)
	reg.Put(db)
	reg.Put(config.NewModule(db))
	reg.Put(&recordingQueue{})

	instance, err := complianceComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *compliance.Module", instance, instance)
	}
	if m.export.sharing != nil || m.retention.lister != nil {
		t.Errorf("optional seams = (%v, %v), want both nil without providers", m.export.sharing, m.retention.lister)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := complianceComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
