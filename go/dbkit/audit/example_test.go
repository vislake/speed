package audit_test

import (
	"context"
	"embed"
	"fmt"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/audit/migrations"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the dbkit.Open calls below have a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
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

// ExampleEmit shows the collection half of this round's design end to
// end: a business module registers its qualified action name on
// pkgcore.Registry.AuditActions, wires audit.New's pkgcore.Module (the
// persister) into the same registry, then records an impersonated action
// (an Actor together with an OnBehalfOf administrator -- root CLAUDE.md's
// dual-identity security rule) through Emit rather than writing to
// Repository directly. Emit publishes an event; the persister's own
// subscription (installed by Register) is what turns that event into the
// AuditEvent row this example reads back through ListByTenant.
func ExampleEmit() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: "file:audit_emit_example?mode=memory&cache=shared"})
	if err != nil {
		fmt.Println("open error:", err)
		return
	}
	defer func() {
		if sqlDB, closeErr := db.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	}()

	persister := audit.New(db)
	migrationsReg := dbkit.NewMigrationRegistry()
	if registerErr := migrationsReg.Register(persister); registerErr != nil {
		fmt.Println("register migrations error:", registerErr)
		return
	}
	if applyErr := migrationsReg.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations error:", applyErr)
		return
	}

	// A real host builds this Registry once at bootstrap and calls every
	// module's Register on it, persister's included; this example does
	// the same for just the one module it needs.
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if registerErr := persister.Register(reg); registerErr != nil {
		fmt.Println("register persister error:", registerErr)
		return
	}

	// A business module declares its own qualified action name at
	// Register time -- here, standing in for org's own Register call.
	const action = "org.member.remove"
	if addErr := reg.AuditActions.Add(action); addErr != nil {
		fmt.Println("add audit action error:", addErr)
		return
	}

	ctx = pkgcore.WithTenant(ctx, "tenant-1")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-42", DisplayName: "Ada"})
	ctx = pkgcore.WithOnBehalfOf(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"})

	emitErr := audit.Emit(ctx, reg.Events.Bus(), reg.AuditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "org.member", ID: "member-7", DisplayName: "Bob"},
		Result:   audit.Result{Success: true},
	})
	if emitErr != nil {
		fmt.Println("emit error:", emitErr)
		return
	}

	rows, listErr := audit.NewRepository(db).ListByTenant(ctx, "tenant-1")
	if listErr != nil {
		fmt.Println("list error:", listErr)
		return
	}
	if len(rows) != 1 {
		fmt.Println("unexpected row count:", len(rows))
		return
	}
	onBehalfOf, _ := rows[0].OnBehalfOf()
	fmt.Println(rows[0].Action, rows[0].Actor().ID, onBehalfOf.ID, rows[0].Result().Success)

	// Output:
	// org.member.remove user-42 admin-1 true
}
