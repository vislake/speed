package audit_test

import (
	"context"
	"embed"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/pkgcore"
)

// exampleAuditModule is the minimal pkgcore.Module shape needed to feed
// audit's embedded migrations to a dbkit.MigrationRegistry. A real host
// gets this for free from the pkgcore.Module the audit persister exposes
// (landing alongside this same round's collection mechanisms); this
// stand-in keeps the example self-contained.
type exampleAuditModule struct{}

func (exampleAuditModule) Name() string                     { return "audit" }
func (exampleAuditModule) DependsOn() []string              { return nil }
func (exampleAuditModule) Migrations() embed.FS             { return migrations.FS }
func (exampleAuditModule) Locales() embed.FS                { return embed.FS{} }
func (exampleAuditModule) OpenAPISpec() []byte              { return nil }
func (exampleAuditModule) Register(*pkgcore.Registry) error { return nil }

// Example shows Repository end to end: migrating audit_events, appending
// an event that records an impersonated action (an Actor together with an
// OnBehalfOf administrator -- root CLAUDE.md's dual-identity security
// rule), and reading it back.
func Example() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: "file::memory:?cache=shared"})
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer func() {
		if sqlDB, closeErr := db.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	}()

	migrationsReg := dbkit.NewMigrationRegistry()
	if registerErr := migrationsReg.Register(exampleAuditModule{}); registerErr != nil {
		fmt.Println("register migrations error:", registerErr)
		return
	}
	if applyErr := migrationsReg.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations error:", applyErr)
		return
	}

	repo := audit.NewRepository(db)

	evt := &audit.AuditEvent{
		Action:   "org.member.remove",
		TenantID: "tenant-1",
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-42", DisplayName: "Ada"})
	admin := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}
	evt.SetOnBehalfOf(&admin)
	evt.SetResource(audit.Resource{Type: "org.member", ID: "member-7", DisplayName: "Bob"})
	evt.SetResult(audit.Result{Success: true})

	if insertErr := repo.Insert(ctx, evt); insertErr != nil {
		fmt.Println("insert error:", insertErr)
		return
	}

	got, err := repo.Get(ctx, evt.ID)
	if err != nil {
		fmt.Println("get error:", err)
		return
	}
	onBehalfOf, _ := got.OnBehalfOf()
	fmt.Println(got.Action, got.Actor().ID, onBehalfOf.ID, got.Result().Success)

	// Output:
	// org.member.remove user-42 admin-1 true
}
