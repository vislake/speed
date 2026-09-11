package compliance

import (
	"context"
	"slices"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
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

// TestComponent_NewFailsWithoutEachMidChainProduct pins the fail-closed
// shape of every mandatory product past the database: each missing one
// fails the construction naming its own gap instead of building a
// half-wired module.
func TestComponent_NewFailsWithoutEachMidChainProduct(t *testing.T) {
	db := dbtest.NewSQLite(t)

	t.Run("queue", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(db)
		instance, err := complianceComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatalf("New = %v, nil error; want the missing queue reported", instance)
		}
	})

	t.Run("config module", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(db)
		reg.Put(&recordingQueue{})
		instance, err := complianceComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatalf("New = %v, nil error; want the missing config module reported", instance)
		}
	})
}

// TestComponent_InitDeclaresThroughTheGate drives the component's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so every declaration lands in the assembly's
// own seats, the services take the assembly's seam values, and the
// assembly's Init-closing beat registers the module's system purposes.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := dbtest.NewSQLite(t)
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, complianceComponent,
		db,
		audit.New(db),
		config.NewModule(db),
		&recordingQueue{},
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	keys := make([]string, 0, len(reg.Config.Items()))
	for _, item := range reg.Config.Items() {
		keys = append(keys, item.Key)
	}
	if !slices.Contains(keys, ConfigDefaultRetentionWindow) || !slices.Contains(keys, ConfigExportDeliveryExpiry) {
		t.Errorf("Config seat items = %v, want the module's two configuration items", keys)
	}
	if perms := reg.Permissions.Permissions(); !slices.Contains(perms, PermissionRetentionManage) || !slices.Contains(perms, PermissionErasureExecute) {
		t.Errorf("Permissions seat = %v, want the module's permissions", perms)
	}
	if actions := reg.AuditActions.Actions(); !slices.Contains(actions, AuditActionRetentionSweep) {
		t.Errorf("AuditActions seat = %v, want the module's audit actions", actions)
	}
	if participants := reg.Retention.Participants(); len(participants) != 1 {
		t.Errorf("Retention seat = %v, want the export-manifests cleanup participant", participants)
	}
	if _, claimed := reg.Jobs.Handlers()[taskTypeRetentionSweep]; !claimed {
		t.Errorf("Jobs seat = %v, want the retention-sweep handler", reg.Jobs.Handlers())
	}
	if decls := reg.Schedules.Declarations(); len(decls) != 1 || decls[0].Type != retentionSweepSchedule.Type {
		t.Errorf("Schedules seat = %v, want the retention-sweep schedule", decls)
	}

	// The declarations reached the running services: each service holds the
	// assembly's own seats, which the declaration body attached while the
	// seats were open.
	if m.retention.bus == nil || m.retention.retention == nil || m.retention.actions == nil {
		t.Errorf("retention service seams = (%v, %v, %v), want the assembly's values",
			m.retention.bus, m.retention.retention, m.retention.actions)
	}
	if m.erasure.retention == nil || m.export.retention == nil {
		t.Error("erasure/export services did not take the assembly's Retention seat")
	}

	// The assembly's own Init-closing beat registered the module's system
	// purposes, the declaration the component descriptor carries.
	for _, purpose := range complianceComponent.SystemPurposes {
		if _, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Actor: "test", Purpose: purpose}); err != nil {
			t.Errorf("WithSystemContext(%q) = %v, want the purpose registered by the assembly", purpose, err)
		}
	}
}

// TestComponent_SelfDescribesItsSystemPurposes pins the declaration the
// module used to make from its own Register: the descriptor carries exactly
// the two audited purposes the module acts under, so the assembly registers
// the same set the module's registration turn once did.
func TestComponent_SelfDescribesItsSystemPurposes(t *testing.T) {
	want := []pkgcore.SystemPurpose{SystemPurposeRetentionSweep, SystemPurposeRightToErasure}
	if !slices.Equal(complianceComponent.SystemPurposes, want) {
		t.Fatalf("component SystemPurposes = %v, want %v", complianceComponent.SystemPurposes, want)
	}
}
