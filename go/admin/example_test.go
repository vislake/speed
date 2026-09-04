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
	"github.com/vislake/speed/go/pkgcore"

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
