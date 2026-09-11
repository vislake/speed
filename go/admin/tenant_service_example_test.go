package admin

// Runnable documentation for TenantService.ListAllRows, mirroring
// usage_example_test.go's convention and living IN-package for the same
// kind of reason: the fixture migrates admin's own table set through the
// unexported adminMigrationModule that example defines, and seeds the
// ledger one row past the repository's unexported page cap.
//
// This example is compiled AND executed by `go test`, so a change to the
// ledger walk that breaks the documented usage fails the build rather
// than only rotting in prose.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
)

// ExampleTenantService_ListAllRows demonstrates the whole-ledger read
// behind every cross-tenant walk in this module: ListAllRows pages
// through TenantRepository.List's own cursor until the ledger is
// exhausted, so a ledger grown past the repository's single-call page
// cap still comes back whole -- one capped List call would silently drop
// every row beyond the cap, an omission no cross-tenant caller's
// contract allows. The executed read below is that walk on a ledger
// seeded one row past the cap.
func ExampleTenantService_ListAllRows() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:admin_example_listallrows?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(adminMigrationModule{}); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	svc := NewTenantService(NewTenantRepository(db))

	// maxTenantListLimit+1 rows: the last one is the row a single capped
	// List call would drop and the paged walk exists to keep.
	for i := 0; i < maxTenantListLimit+1; i++ {
		t := &Tenant{
			TenantID:    fmt.Sprintf("tenant-example-%04d", i),
			DisplayName: "Example Co",
		}
		if createErr := svc.Create(ctx, t); createErr != nil {
			fmt.Println("create:", createErr)
			return
		}
	}

	rows, err := svc.ListAllRows(ctx)
	if err != nil {
		fmt.Println("list all rows:", err)
		return
	}
	fmt.Println("rows:", len(rows))

	// Output:
	// rows: 501
}
