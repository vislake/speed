package dbkit_test

// Runnable documentation for the db components and the lifecycle calls they
// add, following example_test.go's own convention: everything here is
// compiled and executed by `go test`.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/basemodule"
	"github.com/vislake/speed/go/pkgcore"
)

// ExampleClose opens a connection and releases it through the one sanctioned
// teardown step: Close resolves the underlying database handle and closes
// it, so every pooled connection is released.
func ExampleClose() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit-close-example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	if err := dbkit.Close(db); err != nil {
		fmt.Println("close:", err)
		return
	}
	fmt.Println("connection closed")

	// Output:
	// connection closed
}

// ExampleApplyMigrations assembles the db.sqlite component together with a
// host component carrying a migration set and drives the stage sequence an
// application runs: Prepare validates the composition, Construct connects
// (and only connects -- construction failure must not touch the database),
// and Verify applies every selected component's migration set through
// ApplyMigrations, dependency-ordered and recorded in the schema_migrations
// ledger under each component's name.
func ExampleApplyMigrations() {
	ctx := context.Background()

	reg := pkgcore.NewComponentRegistry()
	err := reg.Register(pkgcore.Component{
		Name:       "fixture.base",
		Migrations: basemodule.Migrations,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
	})
	if err != nil {
		fmt.Println("register:", err)
		return
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"db.sqlite": map[string]any{
				"dsn": "file:dbkit-apply-migrations-example?mode=memory&cache=shared",
			},
			"fixture.base": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify}, // ApplyMigrations runs here
	} {
		if err := stage.run(ctx); err != nil {
			fmt.Println(stage.name+":", err)
			return
		}
	}
	fmt.Println("migrations applied")

	if err := reg.Close(ctx); err != nil {
		fmt.Println("close:", err)
	}

	// Output:
	// migrations applied
}
