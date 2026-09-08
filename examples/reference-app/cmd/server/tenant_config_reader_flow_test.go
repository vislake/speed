package main

// tenant_config_reader_flow_test.go is the mandatory-first-consumer proof
// the suite pins go/sharing's and go/compliance's own "no live
// TenantConfigReader adapter over go/config" limitation with (each
// module's own docs, "Tenant-configured default expiry" / "Expiry,
// view limit and no password" sections): it drives the exact
// sharingConfigReader/complianceConfigReader adapters server.go's real
// buildServer wires (defined alongside orgFeatureGate) against a real
// go/config Service, a real go/sharing Service and a real
// go/compliance.ExportService, all composed through the same
// pkgcore.NewKernel().Bootstrap + Module.Attach sequence buildServer
// itself uses -- proving both halves rule 2's own "N days if the tenant
// has not configured one" phrasing promises: a tenant that DOES configure
// one gets it, and a tenant that does not still gets the module's own
// fixed default when no tenant row overrides it.
//
// This does not go through the composed HTTP stack buildTestServer wires:
// neither sharing.Service.Create nor compliance.ExportService.Export has
// an owner-facing HTTP route yet (both modules record this
// as a known limitation), so sharing_flow_test.go's
// own secondSharingService already established the precedent of reaching
// these two Service-level operations directly rather than through HTTP.
// What this file adds beyond that precedent is wiring the SAME
// sharingConfigReader/complianceConfigReader types production code uses,
// rather than an unwired *sharing.Service/*compliance.Module, so what is
// proven here is the host adapters' own new code, not a stand-in for
// it.

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
)

// tenantConfigReaderHarness is what
// newTenantConfigReaderHarness hands back: a real, attached
// *config.Service alongside the real *sharing.Service and *compliance.Module
// wired against it through the host adapters -- everything a test
// needs to write a tenant's configured expiry and observe it actually
// govern a real Create/Export call.
type tenantConfigReaderHarness struct {
	config     *config.Service
	sharing    *sharing.Service
	compliance *compliance.Module
}

// newTenantConfigReaderHarness boots a minimal, self-contained composition
// of go/config, go/sharing and go/compliance over a real, freshly migrated
// SQLite database -- config.NewModule, sharing.NewModule (wired with
// sharingConfigReader) and compliance.NewModule (wired with
// compliance.WithSharing over that same sharing.Service, and with
// complianceConfigReader) registered through one real
// pkgcore.NewKernel().Bootstrap call, then config.Module.Attach -- the
// exact ordering buildServer itself uses (config's own *config.Service is
// only produced strictly after Bootstrap returns), just trimmed to the
// three modules the host adapters touch.
func newTenantConfigReaderHarness(t *testing.T) tenantConfigReaderHarness {
	t.Helper()
	ctx := context.Background()
	db := dbtest.NewSQLite(t)

	// configService is filled by configModule.Attach below (nil until
	// then); sharingConfigReader/complianceConfigReader hold a pointer to
	// this variable, dereferenced lazily, for the identical ordering
	// reason orgFeatureGate's own doc comment (server.go) explains: a
	// construction-time Option cannot capture a *config.Service that does
	// not exist yet.
	var configService *config.Service

	configModule := config.NewModule(db)
	sharingModule := sharing.NewModule(db,
		sharing.WithTenantConfigReader(sharingConfigReader{service: &configService}),
	)
	standaloneQueue := jobs.NewStandaloneQueue(db)
	t.Cleanup(func() {
		if closeErr := standaloneQueue.Close(ctx); closeErr != nil {
			t.Errorf("close standalone queue: %v", closeErr)
		}
	})
	// auditModule is needed here for the identical reason server.go's own
	// buildServer wires one alongside configModule: compliance.Module.
	// Register subscribes to config.EventConfigItemChanged
	// (config_audit.go's onConfigItemChanged) and writes the change into
	// the same audit_events table, so a config.Set that touches either
	// config item this test writes needs that table to already exist, not
	// merely the module's own AuditActions declaration.
	auditModule := audit.New(db)
	complianceModule := compliance.NewModule(audit.NewRepository(db),
		compliance.WithQueue(standaloneQueue),
		compliance.WithSharing(sharingModule.Service()),
		compliance.WithExportConfigReader(complianceConfigReader{service: &configService}),
	)

	migrationRegistry := dbkit.NewMigrationRegistry()
	if err := migrationRegistry.Register(configModule); err != nil {
		t.Fatalf("register config migrations: %v", err)
	}
	if err := migrationRegistry.Register(sharingModule); err != nil {
		t.Fatalf("register sharing migrations: %v", err)
	}
	if err := migrationRegistry.Register(auditModule); err != nil {
		t.Fatalf("register audit migrations: %v", err)
	}
	if err := migrationRegistry.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, configModule, sharingModule, complianceModule, auditModule)
	if err != nil {
		t.Fatalf("bootstrap kernel: %v", err)
	}

	configService, err = configModule.Attach(reg)
	if err != nil {
		t.Fatalf("attach config module: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := configService.Close(); closeErr != nil {
			t.Errorf("close config service: %v", closeErr)
		}
	})

	return tenantConfigReaderHarness{
		config:     configService,
		sharing:    sharingModule.Service(),
		compliance: complianceModule,
	}
}

// setTenantDurationConfig writes value at ScopeTenant for key, under the
// given tenant's own context -- the real go/config Set path an operator's
// admin-console write would ultimately go through,
// exercised here directly.
func setTenantDurationConfig(t *testing.T, cfg *config.Service, tenant pkgcore.TenantID, key string, value time.Duration) {
	t.Helper()
	tenantCtx := pkgcore.WithTenant(context.Background(), tenant)
	if err := cfg.Set(tenantCtx, config.ScopeTenant, key, config.Value{Data: value}, "test-actor"); err != nil {
		t.Fatalf("Set(%q, %v): %v", key, value, err)
	}
}

// TestTenantConfigReader_Sharing_ConfiguredTenant_UsesConfiguredExpiry
// proves sharingConfigReader genuinely resolves a tenant's configured
// sharing.default_expiry override -- written through go/config's real Set
// path -- into the expiry sharing.Service.Create actually mints, rather
// than always falling back to the module's own fixed 30-day default.
func TestTenantConfigReader_Sharing_ConfiguredTenant_UsesConfiguredExpiry(t *testing.T) {
	h := newTenantConfigReaderHarness(t)
	const tenant = pkgcore.TenantID("tenant-configured")
	setTenantDurationConfig(t, h.config, tenant, sharing.ConfigDefaultExpiry, 7*24*time.Hour)

	before := time.Now()
	result, err := h.sharing.Create(pkgcore.WithTenant(context.Background(), tenant), sharing.CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now()

	if result.Share.ExpiresAt == nil {
		t.Fatal("ExpiresAt must not be nil")
	}
	wantEarliest := before.Add(7 * 24 * time.Hour)
	wantLatest := after.Add(7 * 24 * time.Hour)
	if result.Share.ExpiresAt.Before(wantEarliest) || result.Share.ExpiresAt.After(wantLatest) {
		t.Errorf("ExpiresAt = %v, want between %v and %v (the tenant-configured 7 days)", *result.Share.ExpiresAt, wantEarliest, wantLatest)
	}
}

// TestTenantConfigReader_Sharing_UnconfiguredTenant_FallsBackToDefault
// proves the inverse: a tenant that never configured sharing.default_expiry
// still gets the module's own fixed 30-day default, unchanged from before
// the default shape -- sharingConfigReader wired but reporting "unconfigured"
// behaves exactly as if it were never wired at all.
func TestTenantConfigReader_Sharing_UnconfiguredTenant_FallsBackToDefault(t *testing.T) {
	h := newTenantConfigReaderHarness(t)
	const tenant = pkgcore.TenantID("tenant-unconfigured")

	before := time.Now()
	result, err := h.sharing.Create(pkgcore.WithTenant(context.Background(), tenant), sharing.CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	after := time.Now()

	if result.Share.ExpiresAt == nil {
		t.Fatal("ExpiresAt must not be nil")
	}
	const defaultShareExpiry = 30 * 24 * time.Hour
	wantEarliest := before.Add(defaultShareExpiry)
	wantLatest := after.Add(defaultShareExpiry)
	if result.Share.ExpiresAt.Before(wantEarliest) || result.Share.ExpiresAt.After(wantLatest) {
		t.Errorf("ExpiresAt = %v, want between %v and %v (the module's own 30-day default)", *result.Share.ExpiresAt, wantEarliest, wantLatest)
	}
}

// TestTenantConfigReader_Compliance_ConfiguredTenant_UsesConfiguredExpiry
// proves complianceConfigReader genuinely resolves a tenant's configured
// compliance.export_delivery_expiry override -- written through go/config's
// real Set path -- into the expiry ExportService.Export's minted delivery
// share actually carries, rather than always falling back to the module's
// own fixed 24-hour default.
func TestTenantConfigReader_Compliance_ConfiguredTenant_UsesConfiguredExpiry(t *testing.T) {
	h := newTenantConfigReaderHarness(t)
	const tenant = pkgcore.TenantID("tenant-configured")
	setTenantDurationConfig(t, h.config, tenant, compliance.ConfigExportDeliveryExpiry, 2*time.Hour)

	before := time.Now()
	result, err := h.compliance.Export().Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	wantEarliest := before.Add(2 * time.Hour)
	wantLatest := after.Add(2 * time.Hour)
	if result.Delivery.ExpiresAt.Before(wantEarliest) || result.Delivery.ExpiresAt.After(wantLatest) {
		t.Errorf("Delivery.ExpiresAt = %v, want between %v and %v (the tenant-configured 2 hours)", result.Delivery.ExpiresAt, wantEarliest, wantLatest)
	}
}

// TestTenantConfigReader_Compliance_UnconfiguredTenant_FallsBackToDefault
// proves the inverse: a tenant that never configured
// compliance.export_delivery_expiry still gets the module's own fixed
// 24-hour default when no tenant row overrides it.
func TestTenantConfigReader_Compliance_UnconfiguredTenant_FallsBackToDefault(t *testing.T) {
	h := newTenantConfigReaderHarness(t)
	const tenant = pkgcore.TenantID("tenant-unconfigured")

	before := time.Now()
	result, err := h.compliance.Export().Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	const defaultExportDeliveryExpiry = 24 * time.Hour
	wantEarliest := before.Add(defaultExportDeliveryExpiry)
	wantLatest := after.Add(defaultExportDeliveryExpiry)
	if result.Delivery.ExpiresAt.Before(wantEarliest) || result.Delivery.ExpiresAt.After(wantLatest) {
		t.Errorf("Delivery.ExpiresAt = %v, want between %v and %v (the module's own 24-hour default)", result.Delivery.ExpiresAt, wantEarliest, wantLatest)
	}
}
