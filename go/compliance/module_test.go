package compliance

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// fakeAuditModule feeds dbkit/audit's own embedded migrations to
// dbkit.MigrationRegistry, mirroring dbkit/audit's own repository_test.go
// fakeAuditModule (unexported to that package, so this file needs its own
// copy) -- only Name and Migrations are ever read by
// MigrationRegistry.Apply here.
type fakeAuditModule struct{}

func (fakeAuditModule) Name() string                              { return "audit" }
func (fakeAuditModule) DependsOn() []string                       { return nil }
func (fakeAuditModule) Migrations() embed.FS                      { return migrations.FS }
func (fakeAuditModule) Locales() embed.FS                         { return embed.FS{} }
func (fakeAuditModule) OpenAPISpec() []byte                       { return nil }
func (fakeAuditModule) Register(*pkgcore.ComponentRegistry) error { return nil }

// newTestAuditDB returns a migrated SQLite *gorm.DB carrying audit_events,
// for building a *audit.Repository in tests.
func newTestAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(fakeAuditModule{}); err != nil {
		t.Fatalf("register audit migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply audit migrations: %v", err)
	}
	return db
}

// newTestAuditRepo returns a *audit.Repository over a freshly migrated
// SQLite database, ready for NewModule.
func newTestAuditRepo(t *testing.T) *audit.Repository {
	t.Helper()
	return audit.NewRepository(newTestAuditDB(t))
}

// recordingQueue is a jobs.Queue that records every task Enqueue accepted,
// for assertions on what a schedule point puts on the queue -- mirroring
// go/storage's identical recordingQueue fixture (object_test.go), which
// this package's tests follow the same "share one fake across several
// _test.go files in the same package, no internal/testutil needed for a
// same-package fake" precedent for.
type recordingQueue struct {
	tasks []jobs.Task
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.tasks = append(q.tasks, task)
	return "", nil
}

func (q *recordingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (q *recordingQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = (*recordingQueue)(nil)

// TestModule_Register_RefusesAQueuelessBoot pins WithQueue's requirement:
// a Module built with no queue fails the assembly with ErrQueueRequired.
func TestModule_Register_RefusesAQueuelessBoot(t *testing.T) {
	m := NewModule(newTestAuditRepo(t))
	_, err := componenttest.DeclareModules(m)
	if !apperr.HasCode(err, ErrQueueRequired.Code) {
		t.Fatalf("assembly without a queue error = %v, want %s", err, ErrQueueRequired.Code)
	}
}

// TestModule_Register_DeclaresItsSurface proves Register's declarative
// contributions land on the registry: the config item, all four
// permissions, and all three audit actions.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	reg, err := componenttest.DeclareModules(m)
	if err != nil {
		t.Fatalf("assembly: %v", err)
	}

	foundConfig := false
	for _, item := range reg.ConfigSeat().Items() {
		if item.Key == ConfigDefaultRetentionWindow {
			foundConfig = true
		}
	}
	if !foundConfig {
		t.Errorf("Config.Items() missing %q", ConfigDefaultRetentionWindow)
	}

	wantPerms := []string{
		PermissionAuditRead, PermissionRetentionManage,
		PermissionErasureExecute, PermissionExportExecute,
	}
	perms := reg.PermissionsSeat().Permissions()
	for _, want := range wantPerms {
		if !containsString(perms, want) {
			t.Errorf("Permissions() = %v, missing %q", perms, want)
		}
	}

	wantActions := []string{AuditActionRetentionSweep, AuditActionErasureRequest, AuditActionExportRequest}
	actions := reg.AuditActionsSeat().Actions()
	for _, want := range wantActions {
		if !containsString(actions, want) {
			t.Errorf("AuditActions.Actions() = %v, missing %q", actions, want)
		}
	}
}

// TestModule_Register_WiresTheRetentionSweepJobHandler proves a host that
// drains reg.Jobs.Handlers() after the assembly gets a handler for
// taskTypeRetentionSweep.
func TestModule_Register_WiresTheRetentionSweepJobHandler(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	reg, err := componenttest.DeclareModules(m)
	if err != nil {
		t.Fatalf("assembly: %v", err)
	}
	handlers := reg.JobsSeat().Handlers()
	h, ok := handlers[taskTypeRetentionSweep]
	if !ok {
		t.Fatalf("Jobs.Handlers() missing %q", taskTypeRetentionSweep)
	}
	if _, ok := h.(jobs.Handler); !ok {
		t.Errorf("handler for %q is %T, want a jobs.Handler", taskTypeRetentionSweep, h)
	}
}

// TestModule_Register_WiresServicesFromTheRegistry proves the three
// services and AuditQuery are ready to use once the assembly returns: their
// registry-derived seams (Retention, EventBus, AuditActions, ObjectStore)
// are non-nil, so a call into any of them does not panic on a nil field.
func TestModule_Register_WiresServicesFromTheRegistry(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	// The registry carries the object store the export path writes through,
	// the same host-assembled value a real deployment provides.
	reg := componenttest.NewRegistry()
	reg.Put(pkgcore.NewLocalObjectStore(t.TempDir()))
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("assembly: %v", err)
	}
	if m.Retention().retention == nil || m.Retention().bus == nil || m.Retention().actions == nil {
		t.Error("RetentionService seams not wired after the assembly")
	}
	if m.Erasure().retention == nil || m.Erasure().bus == nil || m.Erasure().actions == nil {
		t.Error("ErasureService seams not wired after the assembly")
	}
	if m.Export().retention == nil || m.Export().bus == nil || m.Export().actions == nil || m.Export().store == nil {
		t.Error("ExportService seams not wired after the assembly")
	}
}

// TestModule_Register_RegistersItsOwnExportManifestCleanupParticipant
// proves Register adds the module's own export-manifests cleanup
// participant (export_cleanup.go) onto reg.Retention, wired over the
// module's own audit repository and the registry's resolved ObjectStore --
// the seam a host's retention-sweep schedule drives, so a registered
// compliance module's sweep reaps expired export manifests with no further
// wiring.
func TestModule_Register_RegistersItsOwnExportManifestCleanupParticipant(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	reg, err := componenttest.DeclareModules(m)
	if err != nil {
		t.Fatalf("assembly: %v", err)
	}

	found := false
	for _, p := range reg.RetentionSeat().Participants() {
		if p.Name != exportManifestsParticipantName {
			continue
		}
		found = true
		if p.Sweep == nil {
			t.Error("the export-manifests participant registered without a Sweep callback")
		}
	}
	if !found {
		t.Errorf("Retention.Participants() = %v, missing the module's own %q participant", reg.RetentionSeat().Participants(), exportManifestsParticipantName)
	}
}

// TestModule_WithSharing_WiresExportServiceSharing proves WithSharing
// attaches the given SharingCreator onto ExportService directly at
// construction time -- unlike the registry-derived seams
// TestModule_Register_WiresServicesFromTheRegistry checks, this one needs
// no assembly at all, since WithSharing is not a pkgcore.ComponentRegistry seam
// (module.go's Register doc comment explains why).
func TestModule_WithSharing_WiresExportServiceSharing(t *testing.T) {
	fake := &fakeSharingCreator{}
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}), WithSharing(fake))
	if m.Export().sharing != SharingCreator(fake) {
		t.Error("WithSharing should wire ExportService.sharing to the given SharingCreator")
	}
}

// TestModule_NameAndOpenAPISpec pins the module's simple identity methods.
func TestModule_NameAndOpenAPISpec(t *testing.T) {
	m := NewModule(newTestAuditRepo(t))
	if m.Name() != "compliance" {
		t.Errorf("Name() = %q, want %q", m.Name(), "compliance")
	}
	if m.DependsOn() != nil {
		t.Errorf("DependsOn() = %v, want nil", m.DependsOn())
	}
	if m.OpenAPISpec() != nil {
		t.Errorf("OpenAPISpec() = %v, want nil -- no HTTP surface this round", m.OpenAPISpec())
	}
	entries, err := fs.ReadDir(m.Migrations(), ".")
	if err != nil {
		t.Fatalf("ReadDir(Migrations()): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Migrations() = %v entries, want none -- compliance owns no table of its own", entries)
	}
	if m.AuditQuery() == nil {
		t.Fatal("AuditQuery() = nil, want the module's read-only query layer")
	}
	if m.AuditQuery().repo != m.auditRepo {
		t.Error("AuditQuery() should read through the same repository NewModule was built with")
	}
}

// TestModule_Register_DuplicateDeclarationsArePropagated proves Register's
// failure contract: a registry that already holds one of compliance's
// declared config keys, permissions, audit actions or the module's own
// reserved retention-participant name makes Register fail with that
// registrar's duplicate error -- two modules owning one declaration is a
// bug rather than a silent last-write-wins merge, and the host must hear
// which declaration collided.
func TestModule_Register_DuplicateDeclarationsArePropagated(t *testing.T) {
	cases := []struct {
		name      string
		preseed   func(t *testing.T, reg *pkgcore.ComponentRegistry)
		wantError error
	}{
		{
			name: "config item",
			preseed: func(t *testing.T, reg *pkgcore.ComponentRegistry) {
				if err := reg.ConfigSeat().Add(configItemDecls[0]); err != nil {
					t.Fatalf("preseed config item: %v", err)
				}
			},
			wantError: pkgcore.ErrDuplicateConfigKey,
		},
		{
			name: "permission",
			preseed: func(t *testing.T, reg *pkgcore.ComponentRegistry) {
				if err := reg.PermissionsSeat().Add(PermissionAuditRead); err != nil {
					t.Fatalf("preseed permission: %v", err)
				}
			},
			wantError: pkgcore.ErrDuplicatePermission,
		},
		{
			name: "audit action",
			preseed: func(t *testing.T, reg *pkgcore.ComponentRegistry) {
				if err := reg.AuditActionsSeat().Add(AuditActionRetentionSweep); err != nil {
					t.Fatalf("preseed audit action: %v", err)
				}
			},
			wantError: pkgcore.ErrDuplicateAuditAction,
		},
		{
			name: "reserved retention participant name",
			preseed: func(t *testing.T, reg *pkgcore.ComponentRegistry) {
				p := pkgcore.RetentionParticipant{Name: exportManifestsParticipantName, Sweep: testutil.NoopSweep, Erase: testutil.NoopErase}
				if err := reg.RetentionSeat().Add(p); err != nil {
					t.Fatalf("preseed retention participant: %v", err)
				}
			},
			wantError: pkgcore.ErrDuplicateRetentionParticipant,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := componenttest.NewRegistry()
			m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
			// The preseeded declaration and the module's Register share the
			// registry's one Init window: the seats accept writes only
			// during Init.
			err := componenttest.DeclareAll(reg,
				func(r *pkgcore.ComponentRegistry) error {
					tc.preseed(t, r)
					return nil
				},
				m.Register,
			)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("Register over a preseeded registry error = %v, want %v", err, tc.wantError)
			}
		})
	}
}

// TestModule_Register_DeclaresTheRetentionSweepSchedule pins the module's
// periodic declaration: Register puts exactly the retention-sweep schedule
// on the registry's Schedules seat -- declaring means scheduled, so a host
// running a jobs.Scheduler over the finished registry sweeps every tenant
// at the module's own window cadence, the schedule point this module does
// not run itself.
func TestModule_Register_DeclaresTheRetentionSweepSchedule(t *testing.T) {
	reg := componenttest.NewRegistry()
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}

	decls := reg.SchedulesSeat().Declarations()
	if len(decls) != 1 || decls[0] != retentionSweepSchedule {
		t.Errorf("Register declared %+v, want exactly the retention-sweep schedule %+v", decls, retentionSweepSchedule)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// fakeExpiryReader is a comparable ExportDeliveryExpiryReader answering
// "no tenant-configured value", for the WithExportConfigReader wiring
// test.
type fakeExpiryReader struct{}

func (fakeExpiryReader) ExportDeliveryExpiry(context.Context, pkgcore.TenantID) (time.Duration, bool, error) {
	return 0, false, nil
}

var _ ExportDeliveryExpiryReader = fakeExpiryReader{}

// TestModule_WithOptions_WireTheirSeams proves the three remaining Module
// construction options attach what their docs promise -- WithConfigService
// gives RetentionService its live config reader, WithTenantLister makes
// SweepAllTenants enumerate (an unwired service answers
// ErrTenantListerRequired instead), and WithExportConfigReader gives
// ExportService its expiry reader -- the same wiring proof
// TestModule_WithSharing_WiresExportServiceSharing gives WithSharing.
func TestModule_WithOptions_WireTheirSeams(t *testing.T) {
	cfg := newRetentionConfigService(t, true)
	lister := testutil.FakeTenantLister{}
	reader := fakeExpiryReader{}
	m := NewModule(newTestAuditRepo(t),
		WithQueue(&recordingQueue{}),
		WithConfigService(cfg),
		WithTenantLister(lister),
		WithExportConfigReader(reader),
	)

	if m.Retention().cfg != cfg {
		t.Error("WithConfigService should wire RetentionService.cfg to the given *config.Service")
	}
	results, err := m.Retention().SweepAllTenants(context.Background())
	if err != nil {
		t.Errorf("SweepAllTenants with a wired lister error = %v, want the empty list swept cleanly", err)
	}
	if len(results) != 0 {
		t.Errorf("SweepAllTenants results = %v, want none -- the test lister returns no tenants", results)
	}
	if m.Export().cfg != ExportDeliveryExpiryReader(reader) {
		t.Error("WithExportConfigReader should wire ExportService.cfg to the given ExportDeliveryExpiryReader")
	}
}
