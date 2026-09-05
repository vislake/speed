package admin_test

// Runnable documentation for admin's public API, mirroring
// go/pki/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to admin's public API that breaks the
// documented usage fails the build rather than only rotting in prose.
//
// It drives D5's impersonation lifecycle -- start, a fail-closed lookup of
// an unrelated id, and end -- entirely through admin's exported API:
// admin.NewModule, Module.Tenants/Impersonation, and their Start/Lookup/End
// methods. It deliberately does not call Module.Register (which needs a
// real authn.Service, org.Module, compliance.Module and notification.Module
// wired in -- see AGENTS.md's wiring section and
// examples/reference-app/cmd/server for the full composition), because
// TenantService and ImpersonationService are usable the moment NewModule
// returns: their audit/notification side effects are simply no-ops until
// Register attaches a bus, exactly as their own doc comments describe.

import (
	"context"
	"embed"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/go/admin"
)

// stubAuthnModule stands in for a real *authn.Module purely to satisfy
// dbkit.MigrationRegistry.Apply's dependency-order check: admin.Module
// declares DependsOn() == []string{"authn"} (its Register must run after
// authn's own -- see admin's own DependsOn doc comment), and the migration
// registry's dependency sort requires every named dependency to be present
// in the SAME registry, exactly as a real deployment's registry always
// has authn's migrations registered alongside admin's. This example needs
// no actual authn table, so the stub ships an empty migration set.
type stubAuthnModule struct{}

func (stubAuthnModule) Name() string                     { return "authn" }
func (stubAuthnModule) DependsOn() []string              { return nil }
func (stubAuthnModule) Migrations() embed.FS             { return embed.FS{} }
func (stubAuthnModule) Locales() embed.FS                { return embed.FS{} }
func (stubAuthnModule) OpenAPISpec() []byte              { return nil }
func (stubAuthnModule) Register(*pkgcore.Registry) error { return nil }

func Example() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := admin.NewModule(db)

	// A real host applies every bootstrapped module's migrations before
	// opening it for business. This example needs only admin's own two
	// tables, but the registry's dependency sort still requires "authn"
	// (admin.Module.DependsOn()'s one entry) to be present -- see
	// stubAuthnModule's own doc comment.
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(stubAuthnModule{}); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if regErr := migrations.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	grant, err := module.Impersonation().Start(ctx, admin.StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: pkgcore.TenantID("tenant-acme"),
		Reason:         "customer support ticket #42",
	})
	if err != nil {
		fmt.Println("start:", err)
		return
	}
	fmt.Println("grant started for target:", grant.TargetUserID)

	if _, ok := module.Impersonation().Lookup(ctx, "some-other-grant-id"); ok {
		fmt.Println("unexpectedly found an unrelated grant id")
		return
	}
	fmt.Println("unrelated grant id found:", false)

	found, ok := module.Impersonation().Lookup(ctx, grant.ID)
	fmt.Println("started grant is active:", ok && found.Active(time.Now()))

	if _, err := module.Impersonation().End(ctx, grant.ID, "admin-1"); err != nil {
		fmt.Println("end:", err)
		return
	}

	_, ok = module.Impersonation().Lookup(ctx, grant.ID)
	fmt.Println("grant active after End:", ok)

	// Output:
	// grant started for target: user-1
	// unrelated grant id found: false
	// started grant is active: true
	// grant active after End: false
}

// ExampleModule_AttachRBAC demonstrates D8's role-management surface
// (RoleService): a real rbac.Service, bootstrapped and Attach-ed
// independently, exactly the way go/rbac/example_test.go's own Example
// does it, then wired onto an admin.Module through AttachRBAC -- the
// distinct, post-Bootstrap call this file's own Module.AttachRBAC doc
// comment explains is required because rbac.Module.Attach must run after
// every module's own Register, a moment admin's own Register runs before.
func ExampleModule_AttachRBAC() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example_roles?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	rbacModule := rbac.NewModule(db)
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(rbacModule); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	registry, err := pkgcore.NewKernel().Bootstrap(ctx, rbacModule)
	if err != nil {
		fmt.Println("bootstrap:", err)
		return
	}
	rbacService, err := rbacModule.Attach(registry)
	if err != nil {
		fmt.Println("attach:", err)
		return
	}
	defer func() { _ = rbacService.Close() }()

	adminModule := admin.NewModule(db)
	adminModule.AttachRBAC(rbacService)

	role, err := adminModule.Roles().DefineRole(ctx, "tenant-acme", rbac.RoleDefinition{
		Key:         "auditor",
		Permissions: []string{"rbac:read"},
	})
	if err != nil {
		fmt.Println("define role:", err)
		return
	}
	fmt.Println("role key:", role.Key)

	if bindErr := adminModule.Roles().AssignRole(ctx, "tenant-acme", "user-1", role.Key, ""); bindErr != nil {
		fmt.Println("assign role:", bindErr)
		return
	}
	fmt.Println("assigned to user-1")

	// Output:
	// role key: auditor
	// assigned to user-1
}

// ExampleNewExportService demonstrates D7's export leg (ExportService)'s
// up-front validation: an empty tenantID is refused before the call ever
// reaches the wrapped compliance.ExportService or jobs.Queue (both nil
// here, since neither is touched on this path) -- exactly the guard
// AssignRole/DefineRole/AssignRole above share with every other admin
// service taking a caller-named tenantID. Enqueue's real success path --
// a genuine job landing on a real jobs.Queue and a real
// compliance.ExportService.Export run against a real go/sharing.Service
// -- is proven end to end in export_test.go instead, which needs a full
// org+compliance+sharing+queue fixture this doc example deliberately
// does not reconstruct.
func ExampleNewExportService() {
	exportSvc := admin.NewExportService(nil, nil)

	_, err := exportSvc.Enqueue(context.Background(), "")
	fmt.Println(err)

	// Output:
	// admin.tenant_id_required
}

// ExampleNewUsageService demonstrates D9's cross-tenant usage/billing
// dashboard (UsageService)'s deliberate fail-closed contract: with
// neither go/metering nor go/billing wired (both are optional --
// Module.WithMetering/WithBilling's own doc comments), Summary refuses
// outright with ErrUsageModulesNotWired before ever touching admin's own
// tenant ledger, exactly as go/admin/AGENTS.md's Known limitations
// records this app's own reference deployment currently exercises it.
func ExampleNewUsageService() {
	usageSvc := admin.NewUsageService(nil, nil, nil)

	_, err := usageSvc.Summary(context.Background(), "operator-1")
	fmt.Println(err)

	// Output:
	// admin.usage_modules_not_wired
}

// ExampleNewSendRecordSearchService demonstrates D10's cross-tenant
// notification send-record search (SendRecordSearchService)'s
// single-tenant path: a tenantID given directly calls straight through to
// notification.SendRecordRepository.ListByFilter, needing neither admin's
// own tenant ledger nor an EventBus (both only matter for the empty-
// tenantID cross-tenant path Module.Register wires via its unexported
// attach -- see that method's own doc comment), so a bare
// *notification.DeliveryService is enough to exercise it here.
func ExampleNewSendRecordSearchService() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example_send_records?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	notifModule := notification.NewModule(db)
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(notifModule); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	search := admin.NewSendRecordSearchService(notifModule.Deliveries(), nil)
	records, err := search.Query(ctx, "operator-1", "tenant-acme", notification.SendRecordFilter{Limit: 50})
	if err != nil {
		fmt.Println("query:", err)
		return
	}
	fmt.Println("records found:", len(records))

	// Output:
	// records found: 0
}
