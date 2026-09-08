package admin

// Runnable documentation for UsageService's metering leg (D9), mirroring
// send_records_example_test.go's convention and living IN-package for the
// same reason: the service's bus arrives through Module.Register's
// unexported attach -- see the example's own doc comment below.
//
// This example is compiled AND executed by `go test`, so a change to D9's
// read path that breaks the documented usage fails the build rather than
// only rotting in prose.

import (
	"context"
	"embed"
	"fmt"

	"github.com/vislake/speed/go/admin/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/pkgcore"
)

// adminMigrationModule is the minimal pkgcore.Module the example below
// feeds to dbkit.MigrationRegistry, carrying admin's real migration files
// -- the same shape admin/internal/testutil's own migrationModule uses.
// The registry works in terms of modules, and the real admin.Module cannot
// carry its own files here: its DependsOn() names "authn", so a registry
// holding only admin would trip dbkit.MigrationRegistry.Apply's
// missing-dependency check over a database that has no authn tables.
type adminMigrationModule struct{}

func (adminMigrationModule) Name() string                     { return "admin" }
func (adminMigrationModule) DependsOn() []string              { return nil }
func (adminMigrationModule) Migrations() embed.FS             { return migrations.FS }
func (adminMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (adminMigrationModule) OpenAPISpec() []byte              { return nil }
func (adminMigrationModule) Register(*pkgcore.Registry) error { return nil }

// ExampleUsageService_Summary demonstrates D9's cross-tenant usage
// dashboard's metering leg (UsageService.Summary) against REAL
// go/metering state: a go/metering module constructed the way a host's
// Module.WithMetering wiring constructs it, one tenant in admin's own D3
// ledger, and one real metering event folded into one real
// metering_usage_summaries row -- read back through metering's own
// SummaryRepository.List, the cross-module read path a WithMetering
// wiring opens (Summary calls meteringModule.Summaries().List once per
// ledger tenant). A comment naming that path would not prove it; the
// executed read does. This example is the execution-evidence leg of the
// D9 no-consumer exception's compensating obligations, recorded alongside
// the still-plain limitation and the deliberately unfrozen API in
// AGENTS.md's Known limitations, mirroring go/pki's X.509 section.
//
// go/billing is deliberately absent (the WithBilling counterpart of the
// same wiring choice), so the row's CreditBalance/ActiveSubscription
// fields stay nil -- the "wired module's dimension present, unwired
// module's absent" contract usage.go documents -- and the summary
// performs none of the billing-balance materialization its doc comment
// describes.
func ExampleUsageService_Summary() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example_usage?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// The real module whose rows Summary reads: metering.NewModule needs
	// only the host's migrated database, exactly as a host wiring
	// WithMetering would construct it. Registering the same instance with
	// dbkit.MigrationRegistry applies metering's own table set from zero.
	meteringModule := metering.NewModule(db)
	registry := dbkit.NewMigrationRegistry()
	for _, m := range []pkgcore.Module{meteringModule, adminMigrationModule{}} {
		if regErr := registry.Register(m); regErr != nil {
			fmt.Println("register migrations:", regErr)
			return
		}
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// The two wiring steps Module.Register performs for a real host:
	// declare the "admin.cross_tenant" system purpose, and hand the
	// service the bus every tenancy.WithSystemContext grant audits onto.
	// This example composes the service directly rather than through a
	// full Module graph; a real host never does either step itself.
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())

	// One tenant in admin's own ledger (D3's manual-registration path),
	// and one real metering event folded into that tenant's real
	// metering_usage_summaries row.
	adminModule := NewModule(db)
	if cErr := adminModule.Tenants().Create(ctx, &Tenant{TenantID: "tenant-usage-example", DisplayName: "Usage Example Co"}); cErr != nil {
		fmt.Println("create tenant:", cErr)
		return
	}
	if iErr := meteringModule.Aggregator().Ingest(ctx, metering.UsageEvent{
		TenantID:       "tenant-usage-example",
		Feature:        "ai.generation",
		Quantity:       3,
		IdempotencyKey: "usage-example-1",
	}); iErr != nil {
		fmt.Println("ingest:", iErr)
		return
	}

	usageSvc := NewUsageService(meteringModule, nil, adminModule.Tenants())
	usageSvc.attach(reg.EventBus())

	rows, err := usageSvc.Summary(ctx, "operator-1")
	if err != nil {
		fmt.Println("summary:", err)
		return
	}
	row := rows[0]
	fmt.Printf("%s recorded %d metering summary row\n", row.TenantID, len(row.MeteringSummaries))
	if len(row.MeteringSummaries) > 0 {
		fmt.Printf("feature: %s, quantity: %v\n", row.MeteringSummaries[0].Feature, row.MeteringSummaries[0].Quantity)
	}
	fmt.Println("credit balance:", row.CreditBalance)

	// Output:
	// tenant-usage-example recorded 1 metering summary row
	// feature: ai.generation, quantity: 3
	// credit balance: <nil>
}
