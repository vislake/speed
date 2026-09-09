package compliance

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// configModuleStub feeds go/config's own embedded migrations to
// dbkit.MigrationRegistry, mirroring module_test.go's fakeAuditModule for
// dbkit/audit's (a module's own migrations are not exported through the
// module type, so a test that wants the configs table migrated applies the
// migrations directly) -- only Name and Migrations are ever read by
// MigrationRegistry.Apply here.
type configModuleStub struct{}

func (configModuleStub) Name() string                     { return "config" }
func (configModuleStub) DependsOn() []string              { return nil }
func (configModuleStub) Migrations() embed.FS             { return configmigrations.FS }
func (configModuleStub) Locales() embed.FS                { return embed.FS{} }
func (configModuleStub) OpenAPISpec() []byte              { return nil }
func (configModuleStub) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = configModuleStub{}

// newRetentionConfigService returns a live *config.Service over a freshly
// migrated configs table, attached the way a host attaches one: a real
// config.Module registered on a real pkgcore.Registry, its schema frozen
// by Attach. withComplianceItems controls whether compliance's own
// Register ran on that registry first -- the schema then carries
// ConfigDefaultRetentionWindow (the shape of every real host, which
// bootstraps the module) or does not (the shape of a config service whose
// schema was frozen before compliance ever registered, which is the one
// live way RetentionWindow's config read can error). A test wires the
// returned service onto a RetentionService with svc.cfg = <it>, exactly
// what Module.WithConfigService does.
func newRetentionConfigService(t *testing.T, withComplianceItems bool) *config.Service {
	t.Helper()
	db := dbtest.NewSQLite(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(configModuleStub{}); err != nil {
		t.Fatalf("register config migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply config migrations: %v", err)
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	cfgModule := config.NewModule(db)
	if err := cfgModule.Register(reg); err != nil {
		t.Fatalf("config.Module.Register: %v", err)
	}
	if withComplianceItems {
		m := NewModule(newTestAuditRepo(t), WithQueue(&recordingQueue{}))
		if err := m.Register(reg); err != nil {
			t.Fatalf("compliance.Module.Register: %v", err)
		}
	}
	svc, err := cfgModule.Attach(reg)
	if err != nil {
		t.Fatalf("config.Module.Attach: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// TestRetentionService_SweepTenant_ConfigWiredOverrideDrivesTheCutoff
// proves the reason WithConfigService exists: with a *config.Service wired
// and a tenant-level override of ConfigDefaultRetentionWindow set, the
// sweep's cutoff follows the override, not the module's own 30-day
// default. A soft-deleted row inside the override window but far outside
// the default window is reaped only because the configured value shrank
// the window -- without the wiring the same rows would survive every sweep
// until the operator's override took effect.
func TestRetentionService_SweepTenant_ConfigWiredOverrideDrivesTheCutoff(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	svc.cfg = newRetentionConfigService(t, true)

	tenant := pkgcore.TenantID("tenant-a")
	override := 5 * 24 * time.Hour
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	if err := svc.cfg.Set(ctx, config.ScopeTenant, ConfigDefaultRetentionWindow,
		config.Value{Data: override}, config.Actor("compliance-test-admin")); err != nil {
		t.Fatalf("set tenant retention window override: %v", err)
	}

	window, err := svc.RetentionWindow(ctx, tenant)
	if err != nil {
		t.Fatalf("RetentionWindow: %v", err)
	}
	if window != override {
		t.Fatalf("RetentionWindow = %v, want the tenant override %v", window, override)
	}

	seedFakeNote(t, repo, tenant, "past-override", "subject-1", time.Now().Add(-10*24*time.Hour))
	seedFakeNote(t, repo, tenant, "within-override", "subject-1", time.Now().Add(-24*time.Hour))

	result, err := svc.SweepTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.TotalReaped() != 1 {
		t.Fatalf("TotalReaped() = %d, want exactly 1 -- only the row past the 5-day override window", result.TotalReaped())
	}
	if fakeNoteExists(t, repo, tenant, "past-override") {
		t.Error("the row soft-deleted 10 days ago should have been reaped under the 5-day override")
	}
	if !fakeNoteExists(t, repo, tenant, "within-override") {
		t.Error("the row soft-deleted a day ago must survive -- it is inside the override window")
	}
}

// TestRetentionService_SweepTenant_ConfigReadErrorFailsClosedBeforeAnyParticipant
// proves the sweep never guesses a window when its wired config service
// cannot answer: RetentionWindow's read of ConfigDefaultRetentionWindow
// errors when the service's frozen schema predates compliance's own
// registration (the key is not declared), and SweepTenant propagates that
// error before any participant runs -- a mis-wired or stale config service
// must fail the sweep loudly rather than silently sweeping under an
// unvalidated default.
func TestRetentionService_SweepTenant_ConfigReadErrorFailsClosedBeforeAnyParticipant(t *testing.T) {
	svc, repo := newRetentionHarness(t)
	svc.cfg = newRetentionConfigService(t, false)
	tenant := pkgcore.TenantID("tenant-a")
	seedFakeNote(t, repo, tenant, "expired-1", "subject-1", wellPastDefaultWindow())

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	if _, err := svc.RetentionWindow(ctx, tenant); err == nil {
		t.Error("RetentionWindow over a schema without ConfigDefaultRetentionWindow = nil error, want one")
	}

	result, err := svc.SweepTenant(context.Background(), tenant)
	if err == nil {
		t.Fatal("SweepTenant = nil error, want the config read error propagated")
	}
	if result.Tenant != "" || !result.Cutoff.IsZero() || result.Reaped != nil || result.Errors != nil {
		t.Errorf("SweepTenant result = %+v, want the zero result -- no participant may run when the window cannot be resolved", result)
	}
	if !fakeNoteExists(t, repo, tenant, "expired-1") {
		t.Error("the expired row must survive: the sweep failed before any participant ran")
	}
}
