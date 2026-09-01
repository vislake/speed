package dbkit_test

// Runnable documentation for dbkit's public API, mirroring
// go/pkgcore/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to dbkit's public API that breaks
// the documented usage fails the build instead of only rotting in prose.
//
// Example demonstrates exactly the pattern AGENTS.md's "Typical
// integration" section walks through: a business module defines a
// tenant-scoped model, embeds dbkit.Repository[T] in its own repository
// type instead of holding a *gorm.DB, opens a connection through
// dbkit.Open, and drives a Create followed by a FindByID through it.

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// exampleSubscription is a tenant-scoped model, matching the shape every
// business module's own models are expected to follow: exported "ID" and
// "TenantID" string fields by those exact names (dbkit.Repository[T] reads
// both through reflection; see repository.go's own doc comment), with
// tenant_id the leftmost column of the composite primary key per the
// backend coding standard's data-model rules.
type exampleSubscription struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
	PlanID   string `gorm:"size:64;not null"`
	Status   string `gorm:"size:32;not null"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s exampleSubscription) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// TableName pins exampleSubscription's table name explicitly, independent
// of GORM's pluralization rules, matching the raw CREATE TABLE Example
// applies below.
func (exampleSubscription) TableName() string { return "example_subscriptions" }

// exampleSubscriptionRepository is the data-access type a real business
// module would define, embedding dbkit.Repository[T] instead of holding a
// *gorm.DB directly (backend coding standard, section 3.2).
type exampleSubscriptionRepository struct {
	*dbkit.Repository[exampleSubscription]
}

func newExampleSubscriptionRepository(db *gorm.DB) *exampleSubscriptionRepository {
	return &exampleSubscriptionRepository{Repository: dbkit.NewRepository[exampleSubscription](db)}
}

// Activate creates an active subscription under ctx's tenant, mirroring
// the convenience method AGENTS.md's own illustration adds on top of the
// embedded Repository[T].
func (r *exampleSubscriptionRepository) Activate(ctx context.Context, id, planID string) (*exampleSubscription, error) {
	sub := &exampleSubscription{ID: id, PlanID: planID, Status: "active"}
	if err := r.Create(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

func Example() {
	ctx := context.Background()

	// A real caller opens PostgreSQL in production (dbkit.DialectPostgres);
	// SQLite keeps this example self-contained and dependency-free under
	// `go test`, with no external service required.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:dbkit_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// A real module applies its own versioned migrations through
	// dbkit.MigrationRegistry (see migrations.go); a plain Exec stands in
	// for that here to keep this example self-contained.
	if err = db.Exec(`CREATE TABLE example_subscriptions (
		id        VARCHAR(26) NOT NULL,
		tenant_id VARCHAR(26) NOT NULL,
		plan_id   VARCHAR(64) NOT NULL,
		status    VARCHAR(32) NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		fmt.Println("migrate:", err)
		return
	}

	repo := newExampleSubscriptionRepository(db)

	// A real request's context already carries the tenant, injected by
	// tenancy.Middleware from the access token claims; building it
	// explicitly here is only for illustration.
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	sub, err := repo.Activate(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "plan_pro")
	if err != nil {
		fmt.Println("activate:", err)
		return
	}

	got, err := repo.FindByID(ctx, sub.ID) // promoted from the embedded *dbkit.Repository[exampleSubscription]
	if err != nil {
		fmt.Println("find:", err)
		return
	}
	fmt.Println(got.PlanID, got.Status)

	// Output:
	// plan_pro active
}
