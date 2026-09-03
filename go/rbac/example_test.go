package rbac_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// Example shows the whole wiring sequence a host performs for rbac: open
// and migrate the database, bootstrap the kernel over every module, then
// Attach rbac to the registry Bootstrap returned.
//
// The ordering is the point. Attach freezes the snapshot of every
// permission the host's modules declared, so it must run AFTER Bootstrap
// has given every module its turn to register -- which is why it is a
// method on the Module rather than something Register could have done.
func Example() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:rbac_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := rbac.NewModule(db)

	// Versioned SQL only -- never AutoMigrate.
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	registry, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(ctx, module)
	if err != nil {
		fmt.Println("bootstrap:", err)
		return
	}

	// Exactly once, after Bootstrap: this is what freezes the catalog.
	if _, attachErr := module.Attach(registry); attachErr != nil {
		fmt.Println("attach:", attachErr)
		return
	}

	fmt.Println(registry.Permissions.Permissions())
	// Output: [rbac:manage rbac:read]
}

// ExampleWithSubject shows how the authenticating side hands rbac an
// identity. This is the module's first no-import seam: rbac never learns
// what a user record looks like, so it never imports the module that owns
// one.
func ExampleWithSubject() {
	// In production authn builds this from the access token's claims. The
	// tenant always comes from the claims, never from a request parameter,
	// header or body.
	ctx := rbac.WithSubject(context.Background(), rbac.Subject{
		TenantID: "tenant-a",
		UserID:   "user-1",
	})

	sub, ok := rbac.SubjectFromContext(ctx)
	fmt.Println(ok, sub.TenantID, sub.UserID)

	// An incomplete subject is reported as no subject at all, so a
	// caller's "no subject, deny" branch covers both cases.
	partial := rbac.WithSubject(context.Background(), rbac.Subject{TenantID: "tenant-a"})
	_, ok = rbac.SubjectFromContext(partial)
	fmt.Println(ok)

	// Output:
	// true tenant-a user-1
	// false
}
