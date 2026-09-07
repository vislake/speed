package admin

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/authn"
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

// insertImpersonationTestEvent inserts one audit event written during an
// impersonation session: Actor is the impersonated user the session
// substituted, OnBehalfOf the real administrator behind it (pkgcore's
// dual-identity rule) -- the row shape admin's own ImpersonationMiddleware
// actually writes (pipeline.go).
func insertImpersonationTestEvent(t *testing.T, repo *audit.Repository, tenantID, actorID, onBehalfOfID, action string) {
	t.Helper()
	evt := &audit.AuditEvent{TenantID: tenantID, Action: action, OccurredAt: time.Now().UTC()}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID})
	evt.SetOnBehalfOf(&pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: onBehalfOfID})
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

// TestHandler_AdminListAuditEvents_ServesChangesBackToTheOperator is
// P2-2's read-back regression at the D7 shell: the audit event's
// before/after changes must come back through GET /api/v1/admin/audit-events,
// so the record an operator searches for -- an impersonation start among
// them -- carries the diff that names the operator's own reason. On
// unfixed code the shell served every element of the six-element audit
// event shape except Changes: an event could say that an impersonation
// started but never why, and the mandatory reason (docs/internal/23-admin.md
// section 4.1: the reason itself is part of the audit) was a write-time
// formality the operator who wrote it could not read back once the grant
// ended. The assertion walks the raw JSON response so the regression
// holds against the wire shape itself.
func TestHandler_AdminListAuditEvents_ServesChangesBackToTheOperator(t *testing.T) {
	svc, auditRepo, _ := newTestAuditService(t)

	// The row a real impersonation Start leaves behind: a dual-identity
	// admin.impersonation.started event whose changes column carries the
	// grant's shape and the operator's reason (the JSON shape
	// dbkit/audit's own persister writes -- see its changesJSON).
	changes, err := json.Marshal(audit.Diff{After: map[string]any{
		"target_tenant_id": "tenant-1",
		"reason":           "support ticket #42",
		"expires_at":       "2026-09-08T03:00:00Z",
	}})
	if err != nil {
		t.Fatalf("marshal changes: %v", err)
	}
	evt := &audit.AuditEvent{
		TenantID:   "tenant-a",
		Action:     AuditActionImpersonationStarted,
		Changes:    datatypes.JSON(changes),
		OccurredAt: time.Now().UTC(),
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1"})
	evt.SetOnBehalfOf(&pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"})
	evt.SetResource(audit.Resource{Type: "admin.impersonation_grant", ID: "grant-1"})
	evt.SetResult(audit.Result{Success: true})
	if err := auditRepo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() error = %v", err)
	}

	h := NewHandler(nil, nil, nil, svc, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/audit-events?tenantId=tenant-a&action=admin.impersonation.started&resource=admin.impersonation_grant", nil)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: "operator-1"}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	events, _ := body["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("response carries %d events, want the one started event", len(events))
	}
	started, _ := events[0].(map[string]any)
	changesOut, _ := started["changes"].(map[string]any)
	after, _ := changesOut["after"].(map[string]any)
	if got, _ := after["reason"].(string); got != "support ticket #42" {
		t.Errorf("changes.after.reason = %q, want the operator's own reason read back through the audit shell -- the reason must be reachable by the operator who wrote it, not a write-time formality", got)
	}
	if got, _ := started["onBehalfOfId"].(string); got != "admin-1" {
		t.Errorf("onBehalfOfId = %q, want the real administrator, so the operator can attribute the reason to its writer", got)
	}
}
