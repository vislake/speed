package sharing_test

// Runnable documentation for sharing's public API, mirroring
// go/pki/example_test.go's convention: this example is compiled AND
// executed by `go test`, so a change to sharing's public API that breaks
// the documented usage fails the build rather than only rotting in prose.
//
// This example is one of the compensating obligations AGENTS.md's "No real
// consumer yet" section places on this round (the reference app does not
// wire sharing as a live consumer): it covers the module's full main path
// -- create a share, access it as an unauthenticated viewer would, revoke
// it, and observe the very next access refused -- so at least this shape is
// known to compile and run under an external caller's own import, even
// without a real business module driving it.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/sharing"
)

// Example creates a share link, accesses it, revokes it, and observes the
// very next access refused -- rules 1, 2, 3 and 5
// (docs/internal/07-platform-services.md) exercised end to end through
// sharing's exported API alone.
func Example() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode
	// (dbkit.DialectPostgres). SQLite keeps this example self-contained
	// under `go test`, with no external service required.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:sharing_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := sharing.NewModule(db)

	// Migrations are versioned SQL, applied through dbkit's registry. There
	// is no AutoMigrate anywhere in this codebase.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// Bootstrap wires the module's permissions, audit action, event
	// catalog and config schema onto a real *pkgcore.Registry, and attaches
	// that registry to the module's Service -- the same path a host takes.
	if _, bootErr := pkgcore.NewKernel().Bootstrap(ctx, module); bootErr != nil {
		fmt.Println("bootstrap:", bootErr)
		return
	}

	// Creating a share is tenant data, so it requires a tenant in ctx --
	// the same rule every tenant-scoped repository in this codebase
	// enforces.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))

	created, err := module.Service().Create(tenantCtx, sharing.CreateParams{
		ResourceRef: "storage:smile-simulation-42",
	})
	if err != nil {
		fmt.Println("create:", err)
		return
	}
	fmt.Println("share created")

	// An unauthenticated viewer presents the raw token -- never the share's
	// own id -- exactly as it was returned above, this once.
	if _, accessErr := module.Service().Access(tenantCtx, created.Token, sharing.AccessParams{
		IP: "203.0.113.7",
	}); accessErr != nil {
		fmt.Println("access:", accessErr)
		return
	}
	fmt.Println("access granted")

	if revokeErr := module.Service().Revoke(tenantCtx, created.Share.ID); revokeErr != nil {
		fmt.Println("revoke:", revokeErr)
		return
	}
	fmt.Println("share revoked")

	// The very next access fails immediately -- rule 3
	// (docs/internal/07-platform-services.md's "revocation takes effect
	// immediately" rule): there is no cache on this module's own side to
	// invalidate.
	_, err = module.Service().Access(tenantCtx, created.Token, sharing.AccessParams{})
	fmt.Println("access after revoke:", err)

	// Output:
	// share created
	// access granted
	// share revoked
	// access after revoke: sharing.not_accessible
}
