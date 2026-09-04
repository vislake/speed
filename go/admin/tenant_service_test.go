package admin

import (
	"context"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestRegistry returns a *pkgcore.Registry over an in-process bus, an
// in-memory KVStore and a console mailer -- everything TenantService and
// ImpersonationService need attached in these tests, mirroring every
// other module's identical construction for its own service tests.
func newTestRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

// TestTenantService_HandleOrgNodeCreated_RootNode_LazilyRegisters is D3's
// core proof: the event-driven lazy population path fires on a tenant's
// ROOT node -- org.OrgNode.IsRoot()'s real discriminator, ParentID == "",
// verified against go/org/model.go this round, NOT node depth as an
// earlier draft of docs/internal/23-admin.md assumed.
func TestTenantService_HandleOrgNodeCreated_RootNode_LazilyRegisters(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))
	reg := newTestRegistry()
	if err := reg.AuditActions.Add(AuditActionTenantStatusChanged); err != nil {
		t.Fatalf("register audit action: %v", err)
	}
	svc.attachAudit(reg.EventBus(), reg.AuditActions)

	evt := pkgcore.Event{
		Type:     org.EventNodeCreated,
		TenantID: "tenant-root-1",
		Payload: org.NodeCreated{
			NodeID:   "node-1",
			ParentID: "", // root
			Path:     "node-1",
			Depth:    0,
			Kind:     "workspace",
		},
	}
	if err := svc.handleOrgNodeCreated(context.Background(), evt); err != nil {
		t.Fatalf("handleOrgNodeCreated() error = %v", err)
	}

	got, err := svc.Get(context.Background(), "tenant-root-1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != TenantStatusActive {
		t.Fatalf("Get().Status = %q, want active", got.Status)
	}
}

// TestTenantService_HandleOrgNodeCreated_ChildNode_DoesNotRegister is the
// negative case: a non-root node (ParentID set) must NOT create a ledger
// row -- this is what proves the discriminator is really ParentID == "",
// not merely "any node event".
func TestTenantService_HandleOrgNodeCreated_ChildNode_DoesNotRegister(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))

	evt := pkgcore.Event{
		Type:     org.EventNodeCreated,
		TenantID: "tenant-child-1",
		Payload: org.NodeCreated{
			NodeID:   "node-2",
			ParentID: "node-1", // NOT a root
			Path:     "node-1/node-2",
			Depth:    1,
			Kind:     "team",
		},
	}
	if err := svc.handleOrgNodeCreated(context.Background(), evt); err != nil {
		t.Fatalf("handleOrgNodeCreated() error = %v", err)
	}

	_, err := svc.Get(context.Background(), "tenant-child-1")
	if !isCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Get() error = %v, want ErrTenantNotFound (no row should have been created)", err)
	}
}

// TestTenantService_HandleOrgNodeCreated_CrossReplicaMapPayload proves the
// subscriber's resilience contract: a payload delivered as a
// map[string]any (the shape pkgcore's Redis EventBus produces for a
// cross-replica delivery) is decoded exactly like the publisher's own
// struct would be, via decodeEventPayload's JSON round trip.
func TestTenantService_HandleOrgNodeCreated_CrossReplicaMapPayload(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))

	evt := pkgcore.Event{
		Type:     org.EventNodeCreated,
		TenantID: "tenant-map-1",
		Payload: map[string]any{
			"node_id":   "node-1",
			"parent_id": "",
			"path":      "node-1",
			"depth":     float64(0),
			"kind":      "workspace",
		},
	}
	if err := svc.handleOrgNodeCreated(context.Background(), evt); err != nil {
		t.Fatalf("handleOrgNodeCreated() error = %v", err)
	}
	if _, err := svc.Get(context.Background(), "tenant-map-1"); err != nil {
		t.Fatalf("Get() error = %v, want the row created from the map payload", err)
	}
}

// TestTenantService_HandleOrgNodeCreated_NoTenant_Skips proves the handler
// never panics or errors on an event with no tenant -- it is simply
// skipped, matching org's own handleUserCreated identical case.
func TestTenantService_HandleOrgNodeCreated_NoTenant_Skips(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))

	evt := pkgcore.Event{Type: org.EventNodeCreated, Payload: org.NodeCreated{ParentID: ""}}
	if err := svc.handleOrgNodeCreated(context.Background(), evt); err != nil {
		t.Fatalf("handleOrgNodeCreated() error = %v, want nil (skipped)", err)
	}
}

// TestTenantService_HandleOrgNodeCreated_UnrecognizedPayload_Skips proves
// the handler tolerates a payload it cannot decode at all, logging and
// returning nil rather than propagating an error back to org's own
// Publish call.
func TestTenantService_HandleOrgNodeCreated_UnrecognizedPayload_Skips(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))

	evt := pkgcore.Event{Type: org.EventNodeCreated, TenantID: "tenant-x", Payload: make(chan int)}
	if err := svc.handleOrgNodeCreated(context.Background(), evt); err != nil {
		t.Fatalf("handleOrgNodeCreated() error = %v, want nil (unrecognized payload tolerated)", err)
	}
}

// TestTenantService_HandleOrgNodeCreated_Redelivery_IsIdempotent proves a
// redelivered event (an at-least-once broker's guarantee) creates no
// second row.
func TestTenantService_HandleOrgNodeCreated_Redelivery_IsIdempotent(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))
	ctx := context.Background()

	evt := pkgcore.Event{
		Type:     org.EventNodeCreated,
		TenantID: "tenant-redeliver",
		Payload:  org.NodeCreated{NodeID: "node-1", ParentID: ""},
	}
	for i := 0; i < 3; i++ {
		if err := svc.handleOrgNodeCreated(ctx, evt); err != nil {
			t.Fatalf("handleOrgNodeCreated() call %d error = %v", i, err)
		}
	}

	rows, err := svc.List(ctx, TenantFilter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List() returned %d rows after 3 redeliveries, want exactly 1", len(rows))
	}
}

// TestTenantService_ListAllIDs_PagesPastSingleCallLimit reproduces the bug
// a single List(ctx, TenantFilter{Limit: maxTenantListLimit}) call has:
// silently dropping every ledger row past the 500th. It seeds
// maxTenantListLimit+5 rows -- more than one page -- and asserts every one
// of them comes back, proving ListAllIDs actually pages through
// TenantRepository.List's Cursor mechanism instead of stopping at the
// first page the way D6's MembershipsOf and D7's AuditService.Query used
// to (both now call this method instead of List directly).
func TestTenantService_ListAllIDs_PagesPastSingleCallLimit(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()

	const seeded = maxTenantListLimit + 5
	want := make(map[string]bool, seeded)
	for i := 0; i < seeded; i++ {
		// Zero-padded so lexicographic TenantID ordering (List's own
		// Cursor contract) matches numeric order, keeping this
		// deterministic regardless of how many digits maxTenantListLimit
		// needs.
		id := fmt.Sprintf("tenant-page-%05d", i)
		if err := repo.Create(ctx, &Tenant{TenantID: id}); err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
		want[id] = true
	}

	got, err := svc.ListAllIDs(ctx)
	if err != nil {
		t.Fatalf("ListAllIDs() error = %v", err)
	}
	if len(got) != seeded {
		t.Fatalf("ListAllIDs() returned %d ids, want %d (a single-page List call would silently cap at %d)",
			len(got), seeded, maxTenantListLimit)
	}
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		if !want[id] {
			t.Fatalf("ListAllIDs() returned unexpected id %q", id)
		}
		if seen[id] {
			t.Fatalf("ListAllIDs() returned id %q more than once", id)
		}
		seen[id] = true
	}
}

// TestTenantService_SetStatus_RecordsAuditEvent proves D3's round-1 audit
// trail: a tenant-ledger edit records admin.tenant.status_changed with the
// operator as Actor.
func TestTenantService_SetStatus_RecordsAuditEvent(t *testing.T) {
	db := testutil.NewDB(t)
	tenantRepo := NewTenantRepository(db)
	svc := NewTenantService(tenantRepo)
	reg := newTestRegistry()
	if err := reg.AuditActions.Add(AuditActionTenantStatusChanged); err != nil {
		t.Fatalf("register audit action: %v", err)
	}
	svc.attachAudit(reg.EventBus(), reg.AuditActions)

	ctx := context.Background()
	if err := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-audit-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	suspended := TenantStatusSuspended
	actor := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "operator-1"}
	if _, err := svc.SetStatus(ctx, "tenant-audit-1", TenantPatch{Status: &suspended}, actor); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("got %d recorded audit events, want exactly 1", len(recorded))
	}
	if recorded[0].Action != AuditActionTenantStatusChanged {
		t.Fatalf("Action = %q, want %q", recorded[0].Action, AuditActionTenantStatusChanged)
	}
	if recorded[0].Actor != actor {
		t.Fatalf("Actor = %+v, want %+v", recorded[0].Actor, actor)
	}
}
