package admin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/tenancy"
)

// newTestRegistry returns a *pkgcore.ComponentRegistry over an in-process bus, an
// in-memory KVStore and a console mailer -- everything TenantService and
// ImpersonationService need attached in these tests, mirroring every
// other module's identical construction for its own service tests.
func newTestRegistry() *pkgcore.ComponentRegistry {
	return componenttest.NewRegistry()
}

// TestTenantService_HandleOrgNodeCreated_RootNode_LazilyRegisters is the
// lazy-population path's core proof: the subscriber fires on a tenant's
// ROOT node -- org.OrgNode.IsRoot()'s discriminator, ParentID == "",
// never node depth.
func TestTenantService_HandleOrgNodeCreated_RootNode_LazilyRegisters(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))
	reg := newTestRegistry()
	if err := componenttest.Declare(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(AuditActionTenantStatusChanged)
	}); err != nil {
		t.Fatalf("register audit action: %v", err)
	}
	svc.attachAudit(reg.EventBus(), reg.AuditActions, nil)

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
	if got.Status != tenancy.TenantStatusActive {
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
	if !apperr.HasCode(err, ErrTenantNotFound.Code) {
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

// TestTenantService_ListAllIDs_PagesPastSingleCallLimit pins the
// no-silent-omission contract: a single List(ctx, TenantFilter{Limit:
// maxTenantListLimit}) call silently drops every ledger row past the
// limit. The test seeds maxTenantListLimit+5 rows -- more than one page
// -- and asserts every one of them comes back, proving ListAllIDs
// actually pages through TenantRepository.List's Cursor mechanism instead
// of stopping at the first page (MembershipsOf and AuditService.Query
// both call this method instead of List directly).
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

// TestTenantService_ListAllRows_PagesPastSingleCallLimit is ListAllIDs'
// own paging test's counterpart for the row-returning read: every ledger
// row comes back past one page, each carrying the listing's own columns.
func TestTenantService_ListAllRows_PagesPastSingleCallLimit(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()

	const seeded = maxTenantListLimit + 5
	want := make(map[string]string, seeded)
	for i := 0; i < seeded; i++ {
		id := fmt.Sprintf("tenant-row-page-%05d", i)
		name := fmt.Sprintf("Row Page Co %05d", i)
		if err := repo.Create(ctx, &Tenant{TenantID: id, DisplayName: name}); err != nil {
			t.Fatalf("seed tenant %d: %v", i, err)
		}
		want[id] = name
	}

	rows, err := svc.ListAllRows(ctx)
	if err != nil {
		t.Fatalf("ListAllRows() error = %v", err)
	}
	if len(rows) != seeded {
		t.Fatalf("ListAllRows() returned %d rows, want %d (a single-page List call would silently cap at %d)",
			len(rows), seeded, maxTenantListLimit)
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		name, ok := want[row.TenantID]
		if !ok {
			t.Fatalf("ListAllRows() returned unexpected tenant %q", row.TenantID)
		}
		if row.DisplayName != name {
			t.Fatalf("row %q DisplayName = %q, want %q -- rows must carry the listing's own columns", row.TenantID, row.DisplayName, name)
		}
		if seen[row.TenantID] {
			t.Fatalf("ListAllRows() returned tenant %q more than once", row.TenantID)
		}
		seen[row.TenantID] = true
	}
}

// TestTenantService_ForEachLedgerTenant_EntersPerTenantSystemContext pins
// the walk's contract: fn runs once per ledger row in ledger order, each
// call under that tenant's own audited system-context grant -- one
// tenancy.system_context.entered event per tenant, attributed to the
// operator -- and the row handed to fn carries both the tenant scope and
// the listing's own columns.
func TestTenantService_ForEachLedgerTenant_EntersPerTenantSystemContext(t *testing.T) {
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	reg := newTestRegistry()
	svc.attachAudit(reg.EventBus(), reg.AuditActions, nil)

	ctx := context.Background()
	for i, id := range []string{"tenant-walk-a", "tenant-walk-b"} {
		if err := repo.Create(ctx, &Tenant{TenantID: id, DisplayName: fmt.Sprintf("Walk Co %d", i)}); err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
	}

	var entered []tenancy.SystemContextEnteredEvent
	reg.EventBus().Subscribe(tenancy.EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		var e tenancy.SystemContextEnteredEvent
		if err := pkgcore.DecodeEventPayload(evt.Payload, &e); err != nil {
			return err
		}
		entered = append(entered, e)
		return nil
	})

	var visited []string
	var names []string
	err := svc.forEachLedgerTenant(ctx, "operator-1", func(tenantCtx context.Context, row Tenant) error {
		tenant, ok := pkgcore.TenantFromContext(tenantCtx)
		if !ok {
			t.Errorf("fn received a context with no tenant for row %q", row.TenantID)
		} else if string(tenant) != row.TenantID {
			t.Errorf("fn context tenant = %q, want the row's own %q", tenant, row.TenantID)
		}
		if _, ok := pkgcore.SystemReasonFromContext(tenantCtx); !ok {
			t.Errorf("fn context for %q carries no system reason -- the walk must grant each tenant's system context", row.TenantID)
		}
		visited = append(visited, row.TenantID)
		names = append(names, row.DisplayName)
		return nil
	})
	if err != nil {
		t.Fatalf("forEachLedgerTenant() error = %v", err)
	}

	if len(visited) != 2 || visited[0] != "tenant-walk-a" || visited[1] != "tenant-walk-b" {
		t.Fatalf("visited = %v, want the ledger order [tenant-walk-a tenant-walk-b]", visited)
	}
	if names[0] != "Walk Co 0" || names[1] != "Walk Co 1" {
		t.Errorf("row display names = %v, want each row's own ledger columns", names)
	}
	if len(entered) != 2 {
		t.Fatalf("published %d tenancy.system_context.entered events, want one per tenant", len(entered))
	}
	for i, e := range entered {
		if e.Actor != "operator-1" || e.Purpose != SystemPurposeAdminCrossTenant {
			t.Errorf("system-context event %d = %+v, want Actor operator-1 under %s", i, e, SystemPurposeAdminCrossTenant)
		}
	}
}

// TestTenantService_ForEachLedgerTenant_AbortsOnFnError pins the
// no-silent-omission edge the walk's cross-tenant callers rely on: an fn
// failure stops the walk at that tenant -- every later tenant is skipped
// -- and the error is returned unchanged.
func TestTenantService_ForEachLedgerTenant_AbortsOnFnError(t *testing.T) {
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	reg := newTestRegistry()
	svc.attachAudit(reg.EventBus(), reg.AuditActions, nil)

	ctx := context.Background()
	for _, id := range []string{"tenant-stop-1", "tenant-stop-2", "tenant-stop-3"} {
		if err := repo.Create(ctx, &Tenant{TenantID: id}); err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
	}

	wantErr := errors.New("per-tenant read failed (test)")
	var visited []string
	err := svc.forEachLedgerTenant(ctx, "operator-1", func(_ context.Context, row Tenant) error {
		visited = append(visited, row.TenantID)
		if row.TenantID == "tenant-stop-2" {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("forEachLedgerTenant() error = %v, want the fn's own error returned unchanged", err)
	}
	if len(visited) != 2 || visited[0] != "tenant-stop-1" || visited[1] != "tenant-stop-2" {
		t.Fatalf("visited = %v, want the walk to stop at the failing tenant", visited)
	}
}

// TestTenantService_SetStatus_RecordsAuditEvent pins the ledger's audit
// trail: a tenant-ledger edit records admin.tenant.status_changed with
// the operator as Actor.
func TestTenantService_SetStatus_RecordsAuditEvent(t *testing.T) {
	db := testutil.NewDB(t)
	tenantRepo := NewTenantRepository(db)
	svc := NewTenantService(tenantRepo)
	reg := newTestRegistry()
	if err := componenttest.Declare(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(AuditActionTenantStatusChanged)
	}); err != nil {
		t.Fatalf("register audit action: %v", err)
	}
	svc.attachAudit(reg.EventBus(), reg.AuditActions, nil)

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

	suspended := tenancy.TenantStatusSuspended
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

// TestTenantService_Status_UnknownTenant_IsActiveNeverSuspended is the
// status seam's core safety property: a tenant the ledger has never heard
// of -- whose event-driven lazy registration has not landed yet, or one
// nobody has recorded here at all -- must never be treated as suspended,
// or the ledger's own eventual-consistency lag would turn into an outage
// for a perfectly legitimate, brand-new tenant.
func TestTenantService_Status_UnknownTenant_IsActiveNeverSuspended(t *testing.T) {
	db := testutil.NewDB(t)
	svc := NewTenantService(NewTenantRepository(db))

	status, err := svc.Status(context.Background(), "tenant-never-seen")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status != tenancy.TenantStatusActive {
		t.Fatalf("Status() = %q, want %q", status, tenancy.TenantStatusActive)
	}
}

// TestTenantService_Status_ActiveTenant_ReportsActive pins the ordinary
// case: an explicitly active ledger row reports active.
func TestTenantService_Status_ActiveTenant_ReportsActive(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-active-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	status, err := svc.Status(ctx, "tenant-active-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status != tenancy.TenantStatusActive {
		t.Fatalf("Status() = %q, want %q", status, tenancy.TenantStatusActive)
	}
}

// TestTenantService_Status_SuspendedTenant_ReportsSuspended pins the
// suspension read: a ledger row SetStatus suspended reports suspended
// through this same seam, exactly the fact tenancy.Middleware needs to
// actually refuse a request.
func TestTenantService_Status_SuspendedTenant_ReportsSuspended(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-suspended-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	suspended := tenancy.TenantStatusSuspended
	if _, err := svc.SetStatus(ctx, "tenant-suspended-1", TenantPatch{Status: &suspended}, pkgcore.Actor{ID: "op"}); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	status, err := svc.Status(ctx, "tenant-suspended-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status != tenancy.TenantStatusSuspended {
		t.Fatalf("Status() = %q, want %q", status, tenancy.TenantStatusSuspended)
	}
}

// TestTenantService_Status_ResumedTenant_ReportsActiveAgain pins the
// resume read: a resumed tenant's next Status call flips back to active
// immediately -- the "takes effect on the next request through the
// resolver" behavior, at the TenantService layer (the reference app's
// flowtests/admin_flow_test.go covers the same behavior through a real
// tenancy.Middleware round trip).
func TestTenantService_Status_ResumedTenant_ReportsActiveAgain(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-resumed-1"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	suspended := tenancy.TenantStatusSuspended
	if _, err := svc.SetStatus(ctx, "tenant-resumed-1", TenantPatch{Status: &suspended}, pkgcore.Actor{ID: "op"}); err != nil {
		t.Fatalf("suspend SetStatus() error = %v", err)
	}
	active := tenancy.TenantStatusActive
	if _, err := svc.SetStatus(ctx, "tenant-resumed-1", TenantPatch{Status: &active}, pkgcore.Actor{ID: "op"}); err != nil {
		t.Fatalf("resume SetStatus() error = %v", err)
	}

	status, err := svc.Status(ctx, "tenant-resumed-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status != tenancy.TenantStatusActive {
		t.Fatalf("Status() after resume = %q, want %q", status, tenancy.TenantStatusActive)
	}
}

// TestTenantService_Status_UnknownLedgerStatus_ReportedAsIsNotActive
// pins the no-translation invariant: the ledger's Status column is typed
// with tenancy.TenantStatus itself, so TenantService.Status reports a
// stored value as-is -- it must NEVER translate an unrecognized status to
// TenantStatusActive ("anything not suspended means active"). tenancy's
// own gate refuses every status other than TenantStatusActive, so an
// unrecognized stored value reported faithfully is refused; one
// translated to active would silently keep serving requests for a tenant
// a future third state was meant to block.
func TestTenantService_Status_UnknownLedgerStatus_ReportedAsIsNotActive(t *testing.T) {
	db := testutil.NewDB(t)
	repo := NewTenantRepository(db)
	svc := NewTenantService(repo)
	ctx := context.Background()
	unknown := tenancy.TenantStatus("archived")
	if err := repo.Create(ctx, &Tenant{TenantID: "tenant-future-state", Status: unknown}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	status, err := svc.Status(ctx, "tenant-future-state")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status != unknown {
		t.Fatalf("Status() = %q, want %q -- reported as-is, never translated to active", status, unknown)
	}
}
