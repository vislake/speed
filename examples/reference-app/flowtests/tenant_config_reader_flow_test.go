package flowtests

// tenant_config_reader_flow_test.go is the mandatory-first-consumer proof
// for the two host adapters over go/config that internal/app/server.go's
// real BuildServer wires: app.ShareExpiryReader (the platform's
// composition toolkit) and compliance's own compliance.NewConfigReader,
// both over the config module's lazy Handle. It drives them against a
// real go/config Service, a real go/sharing Service and a real
// go/compliance.ExportService, all composed through the same
// pkgcore.NewKernel().Bootstrap + Module.Attach sequence BuildServer
// itself uses. The tenant-configured default expiry applies when the
// tenant has configured one; a tenant that has not still gets the
// module's own fixed default when no tenant row overrides it.
//
// This file reaches these Service-level operations directly rather than
// through the composed HTTP stack buildTestServer wires (the precedent
// sharing_flow_test.go's secondSharingService set), because what is
// under test is the reader wiring itself: the SAME
// ShareExpiryReader/compliance.NewConfigReader combinations production
// code uses, composed with the real services, not an unwired
// *sharing.Service/*compliance.Module stand-in.

import (
	"context"
	"testing"
	"time"

	speedbridges "github.com/vislake/speed/go/app/bridges"

	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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
// speedbridges.ShareExpiryReader over the config module's lazy Handle) and
// compliance.NewModule (wired with
// compliance.WithSharing over that same sharing.Service, and with
// compliance.NewConfigReader over the same handle)
// registered through one real
// pkgcore.NewKernel().Bootstrap call, then config.Module.Attach -- the
// exact ordering BuildServer itself uses (config's own *config.Service is
// only produced strictly after Bootstrap returns), just trimmed to the
// three modules the host adapters touch.
func newTenantConfigReaderHarness(t *testing.T) tenantConfigReaderHarness {
	t.Helper()
	ctx := context.Background()
	db := dbtest.NewSQLite(t)

	// configService is filled by configModule.Attach below (nil until
	// then). Both host adapters read through the config module's lazy
	// Handle, which exists from construction and reports the config
	// module's coded not-attached refusal until Attach has run -- a
	// construction-time Option cannot capture the *config.Service itself,
	// which does not exist yet.
	var configService *config.Service

	configModule := config.NewModule(db)
	sharingModule := sharing.NewModule(db,
		sharing.WithTenantConfigReader(speedbridges.ShareExpiryReader{Handle: configModule.Handle()}),
	)
	standaloneQueue := jobs.NewStandaloneQueue(db)
	t.Cleanup(func() {
		if closeErr := standaloneQueue.Close(ctx); closeErr != nil {
			t.Errorf("close standalone queue: %v", closeErr)
		}
	})
	// auditModule is needed here for the identical reason internal/app/server.go's own
	// BuildServer wires one alongside configModule: compliance.Module.
	// Register subscribes to config.EventConfigItemChanged
	// (config_audit.go's onConfigItemChanged) and writes the change into
	// the same audit_events table, so a config.Set that touches either
	// config item this test writes needs that table to already exist, not
	// merely the module's own AuditActions declaration.
	auditModule := audit.New(db)
	complianceModule := compliance.NewModule(audit.NewRepository(db),
		compliance.WithQueue(standaloneQueue),
		compliance.WithSharing(sharingModule.Service()),
		compliance.WithExportConfigReader(compliance.NewConfigReader(configModule.Handle())),
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

	// The modules' Register calls and config's Attach share the registry's
	// one Init window: the seats accept writes only during Init. The
	// registry carries a local object store, the host-assembled value
	// compliance's export path writes its manifest through.
	reg := componenttest.NewRegistry()
	reg.Put(pkgcore.NewLocalObjectStore(t.TempDir()))
	if err := componenttest.DeclareAll(reg,
		configModule.Register, sharingModule.Register, complianceModule.Register, auditModule.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := configModule.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			configService = attached
			return nil
		},
	); err != nil {
		t.Fatalf("declare and attach config: %v", err)
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
// proves speedbridges.ShareExpiryReader genuinely resolves a tenant's configured
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
// still gets the module's own fixed 30-day default --
// ShareExpiryReader wired but reporting "unconfigured"
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
// proves compliance.NewConfigReader genuinely resolves a tenant's
// configured compliance.export_delivery_expiry override -- written through
// go/config's real Set path -- into the expiry ExportService.Export's
// minted delivery share actually carries, rather than always falling back
// to the module's own fixed 24-hour default.
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
