package admin

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	auditmigrations "github.com/vislake/speed/go/dbkit/audit/migrations"
	"github.com/vislake/speed/go/pkgcore"
)

// auditMigrationModule mirrors orgMigrationModule for dbkit/audit's own
// migration files.
type auditMigrationModule struct{}

func (auditMigrationModule) Name() string                     { return "audit" }
func (auditMigrationModule) DependsOn() []string              { return nil }
func (auditMigrationModule) Migrations() embed.FS             { return auditmigrations.FS }
func (auditMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (auditMigrationModule) OpenAPISpec() []byte              { return nil }
func (auditMigrationModule) Register(*pkgcore.Registry) error { return nil }

// newTestAuditService returns an AuditService over a real audit.Repository
// and a real TenantService, both sharing one fresh database.
func newTestAuditService(t *testing.T) (*AuditService, *audit.Repository, *TenantRepository) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	db := testutil.NewDB(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(auditMigrationModule{}); err != nil {
		t.Fatalf("register audit's migrations: %v", err)
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply audit's migrations: %v", err)
	}

	auditRepo := audit.NewRepository(db)
	tenantRepo := NewTenantRepository(db)
	tenants := NewTenantService(tenantRepo)
	svc := NewAuditService(compliance.NewAuditQuery(auditRepo), tenants)
	svc.attach(newTestRegistry().EventBus())
	return svc, auditRepo, tenantRepo
}

func insertTestEvent(t *testing.T, repo *audit.Repository, tenantID, action string) {
	t.Helper()
	evt := &audit.AuditEvent{TenantID: tenantID, Action: action, OccurredAt: time.Now().UTC()}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
	evt.SetResource(audit.Resource{Type: "note", ID: "note-1"})
	evt.SetResult(audit.Result{Success: true})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}
}

func TestAuditService_Query_SingleTenant(t *testing.T) {
	svc, auditRepo, _ := newTestAuditService(t)
	insertTestEvent(t, auditRepo, "tenant-a", "notes.note.create")
	insertTestEvent(t, auditRepo, "tenant-b", "notes.note.create")

	events, err := svc.Query(context.Background(), "operator-1", AuditFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(events) != 1 || events[0].TenantID != "tenant-a" {
		t.Fatalf("Query(tenantId=tenant-a) = %+v, want exactly tenant-a's event", events)
	}
}

func TestAuditService_Query_CrossTenant_UsesLedgerAsTenantList(t *testing.T) {
	svc, auditRepo, tenantRepo := newTestAuditService(t)
	ctx := context.Background()

	if err := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-b"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	insertTestEvent(t, auditRepo, "tenant-a", "notes.note.create")
	insertTestEvent(t, auditRepo, "tenant-b", "notes.note.create")
	// A tenant NOT in the ledger: the cross-tenant read must not see it,
	// since D7's cross-tenant path draws its candidate list from D3's own
	// ledger.
	insertTestEvent(t, auditRepo, "tenant-unregistered", "notes.note.create")

	events, err := svc.Query(ctx, "operator-1", AuditFilter{})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Query(cross-tenant) returned %d events, want exactly 2 (ledger tenants only)", len(events))
	}
}

func TestAuditService_Query_FiltersByAction(t *testing.T) {
	svc, auditRepo, tenantRepo := newTestAuditService(t)
	ctx := context.Background()
	if err := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	insertTestEvent(t, auditRepo, "tenant-a", "notes.note.create")
	insertTestEvent(t, auditRepo, "tenant-a", "org.member.remove")

	events, err := svc.Query(ctx, "operator-1", AuditFilter{TenantID: "tenant-a", Action: "org.member.remove"})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(events) != 1 || events[0].Action != "org.member.remove" {
		t.Fatalf("Query(action=org.member.remove) = %+v, want exactly that one event", events)
	}
}

func TestAuditService_Get_PassesThrough(t *testing.T) {
	svc, auditRepo, _ := newTestAuditService(t)
	insertTestEvent(t, auditRepo, "tenant-a", "notes.note.create")

	all, err := auditRepo.ListByTenant(context.Background(), "tenant-a")
	if err != nil || len(all) != 1 {
		t.Fatalf("ListByTenant() = %+v, %v", all, err)
	}

	got, err := svc.Get(context.Background(), all[0].ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got == nil || got.ID != all[0].ID {
		t.Fatalf("Get() = %+v, want the inserted event", got)
	}
}
