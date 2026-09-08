package admin

// Runnable documentation for SendRecordSearchService, mirroring the
// examples in example_test.go but living IN-package because the service's
// bus arrives through Module.Register's unexported attach -- see the
// example's own doc comment below.
//
// This example is compiled AND executed by `go test`, so a change to the
// single-tenant path's wiring contract that breaks the documented usage
// fails the build rather than only rotting in prose.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
)

// ExampleNewSendRecordSearchService demonstrates the cross-tenant
// notification send-record search (SendRecordSearchService)'s
// single-tenant path: a platform operator searching ONE tenant's send
// records is still a cross-tenant read of platform data, so even the
// named-tenant path goes through tenancy.WithSystemContext's audited
// wrapper -- the read must leave the same tenancy.system_context.entered
// audit trail the cross-tenant path does.
// That wrapper needs both the "admin.cross_tenant" system purpose
// registered (RegisterSystemPurpose) and the service's bus attached
// (Module.Register's own wiring step, reproduced here by hand because
// this example composes the service directly rather than through a full
// Module graph; a real host never does either step itself).
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

	// The two wiring steps Module.Register performs for a real host:
	// declare the cross-tenant system purpose, and hand the service the
	// bus every WithSystemContext grant audits onto.
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())

	search := NewSendRecordSearchService(notifModule.Deliveries(), nil)
	search.attach(reg.EventBus())

	records, err := search.Query(ctx, "operator-1", "tenant-acme", notification.SendRecordFilter{Limit: 50})
	if err != nil {
		fmt.Println("query:", err)
		return
	}
	fmt.Println("records found:", len(records))

	// Output:
	// records found: 0
}
