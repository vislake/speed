package admin_test

// Runnable documentation for admin's public API, mirroring
// go/pki/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to admin's public API that breaks the
// documented usage fails the build rather than only rotting in prose.
//
// It demonstrates D5's impersonation pipeline failing closed at its
// construction boundary: Module.Impersonation().Start refuses with
// ErrImpersonationNotWired until Module.Register has attached the
// service's mandatory host seams (P2-4's fix). Driving Start through the
// exported surface exactly as a consumer would -- admin.NewModule plus
// Module.Impersonation, with no Register -- used to start a grant
// silently, with target-existence and membership validation and the
// mandatory security notification all skipped (the shape the exported
// half-built constructor handed out); it now fails closed with a named
// error before anything is written. The full start/lookup/end lifecycle
// with validation, notification and audit wired for real requires
// Register, which in turn needs real authn, org, compliance and
// notification modules wired in -- see AGENTS.md's wiring section and
// examples/reference-app/cmd/server for the full composition -- so that
// lifecycle is pinned by the module's own suites
// (impersonation_service_locale_test.go and module_test.go) rather than
// reconstructed here, exactly as ExampleNewExportService defers its real
// path to export_test.go.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/go/admin"
)

func Example() {
	ctx := context.Background()

	// A real host opens (and migrates) its database before Bootstrap; this
	// example opens one so NewModule is constructed exactly as in a real
	// host. Constructing a Module performs no I/O, and the refusal below
	// fires before the database is ever touched.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := admin.NewModule(db)

	// Before Module.Register has attached the service's mandatory host
	// seams -- the state every consumer sees until Bootstrap runs -- Start
	// fails closed with a named error instead of starting a grant whose
	// target was never validated and who was never notified (P2-4's fix).
	_, err = module.Impersonation().Start(ctx, admin.StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: pkgcore.TenantID("tenant-acme"),
		Reason:         "customer support ticket #42",
	})
	fmt.Println("start:", err)

	// Output:
	// start: admin.impersonation_not_wired
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

	_, err := exportSvc.Enqueue(context.Background(), "", "operator-1")
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
