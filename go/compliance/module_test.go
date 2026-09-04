package compliance

import (
	"context"
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeAuditModule feeds dbkit/audit's own embedded migrations to
// dbkit.MigrationRegistry, mirroring dbkit/audit's own repository_test.go
// fakeAuditModule (unexported to that package, so this file needs its own
// copy) -- only Name and Migrations are ever read by
// MigrationRegistry.Apply here.
type fakeAuditModule struct{}

func (fakeAuditModule) Name() string                     { return "audit" }
func (fakeAuditModule) DependsOn() []string              { return nil }
func (fakeAuditModule) Migrations() embed.FS             { return migrations.FS }
func (fakeAuditModule) Locales() embed.FS                { return embed.FS{} }
func (fakeAuditModule) OpenAPISpec() []byte              { return nil }
func (fakeAuditModule) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = fakeAuditModule{}

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
// a Module built with no queue fails Bootstrap with ErrQueueRequired.
func TestModule_Register_RefusesAQueuelessBoot(t *testing.T) {
	m := NewModule(newTestAuditRepo(t))
	_, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if !hasCode(err, ErrQueueRequired.Code) {
		t.Fatalf("Bootstrap without a queue error = %v, want %s", err, ErrQueueRequired.Code)
	}
}

// TestModule_Register_DeclaresItsSurface proves Register's declarative
// contributions land on the registry: the config item, all four
// permissions, and all three audit actions.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	foundConfig := false
	for _, item := range reg.Config.Items() {
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
	perms := reg.Permissions.Permissions()
	for _, want := range wantPerms {
		if !containsString(perms, want) {
			t.Errorf("Permissions() = %v, missing %q", perms, want)
		}
	}

	wantActions := []string{AuditActionRetentionSweep, AuditActionErasureRequest, AuditActionExportRequest}
	actions := reg.AuditActions.Actions()
	for _, want := range wantActions {
		if !containsString(actions, want) {
			t.Errorf("AuditActions.Actions() = %v, missing %q", actions, want)
		}
	}
}

// TestModule_Register_WiresTheRetentionSweepJobHandler proves a host that
// drains reg.Jobs.Handlers() after Bootstrap gets a handler for
// taskTypeRetentionSweep.
func TestModule_Register_WiresTheRetentionSweepJobHandler(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	handlers := reg.Jobs.Handlers()
	h, ok := handlers[taskTypeRetentionSweep]
	if !ok {
		t.Fatalf("Jobs.Handlers() missing %q", taskTypeRetentionSweep)
	}
	if _, ok := h.(jobs.Handler); !ok {
		t.Errorf("handler for %q is %T, want a jobs.Handler", taskTypeRetentionSweep, h)
	}
}

// TestModule_Register_WiresServicesFromTheRegistry proves the three
// services and AuditQuery are ready to use once Bootstrap returns: their
// registry-derived seams (Retention, EventBus, AuditActions, ObjectStore)
// are non-nil, so a call into any of them does not panic on a nil field.
func TestModule_Register_WiresServicesFromTheRegistry(t *testing.T) {
	m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
	if _, err := pkgcore.NewKernel().Bootstrap(context.Background(), m); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if m.Retention().retention == nil || m.Retention().bus == nil || m.Retention().actions == nil {
		t.Error("RetentionService seams not wired after Bootstrap")
	}
	if m.Erasure().retention == nil || m.Erasure().bus == nil || m.Erasure().actions == nil {
		t.Error("ErasureService seams not wired after Bootstrap")
	}
	if m.Export().retention == nil || m.Export().bus == nil || m.Export().actions == nil || m.Export().store == nil {
		t.Error("ExportService seams not wired after Bootstrap")
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
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
