//go:build integration

// The soft-delete leg of go/rbac's PostgreSQL integration tier: this
// round's own proof that RoleBinding's mark-delete/restore mechanism and
// its partial unique index (migrations/{sqlite,postgres}/
// 0002_add_soft_delete.sql) behave identically on the engine whose
// partial-index syntax and collation genuinely differ from SQLite's -- the
// unit tier's SQLite-only proof (assign_test.go's
// TestService_RevokeRole_ThenAssignRole_SameScope_Succeeds and
// TestService_RestoreRole_UndoesTheRevokeAndAnnouncesIt) cannot rule out a
// PostgreSQL-specific partial-index mistake shipping unnoticed.
//
// This mirrors go/org's own
// integration_test/postgres_softdelete_test.go, added as a SEPARATE,
// proactive file in this round -- the org round added its equivalent leg
// after the fact, once its first review pass flagged the gap; this module's
// round adds it from the start instead. See go/rbac/AGENTS.md's "Soft
// deletion" section for the round's full design record.
package rbac_test

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// TestPostgres_RevokeRole_ThenAssignRole_SameScope_Succeeds re-runs
// assign_test.go's TestService_RevokeRole_ThenAssignRole_SameScope_Succeeds
// against a real PostgreSQL server. Against the pre-round FULL unique index
// this AssignRole would fail with rbac.duplicate_role's binding-side
// twin -- a gorm.ErrDuplicatedKey the revoked row's occupied tuple would
// still be holding -- which is exactly the real functional regression the
// partial index exists to avoid.
func TestPostgres_RevokeRole_ThenAssignRole_SameScope_Succeeds(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "region-reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if err := svc.AssignRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("first AssignRole: %v", err)
	}
	if err := svc.RevokeRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}

	if err := svc.AssignRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("AssignRole immediately after RevokeRole at the identical scope: %v, want success", err)
	}

	allowed, err := svc.Can(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after reassign: %v", err)
	}
	if !allowed {
		t.Fatal("the fresh grant does not hold after a revoke-then-reassign at the identical scope")
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	var liveRows int64
	if err = sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM rbac_role_bindings WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		"tenant-a", "user-1").Scan(&liveRows); err != nil {
		t.Fatalf("counting live bindings: %v", err)
	}
	if liveRows != 1 {
		t.Fatalf("live binding rows = %d, want exactly 1", liveRows)
	}
	var totalRows int64
	if err = sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM rbac_role_bindings WHERE tenant_id = $1 AND user_id = $2`,
		"tenant-a", "user-1").Scan(&totalRows); err != nil {
		t.Fatalf("counting all bindings: %v", err)
	}
	if totalRows != 2 {
		t.Fatalf("total binding rows (live + mark-deleted) = %d, want 2 -- the revoke must mark, never physically delete", totalRows)
	}
}

// TestPostgres_RestoreRole_UndoesTheRevoke re-runs assign_test.go's
// TestService_RestoreRole_UndoesTheRevokeAndAnnouncesIt against a real
// PostgreSQL server: revoke, verify the decision path denies, restore,
// verify the decision path grants again with the binding's original scope
// intact.
func TestPostgres_RestoreRole_UndoesTheRevoke(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "region-reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if err := svc.AssignRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := svc.RevokeRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}

	denied, err := svc.Can(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after revoke: %v", err)
	}
	if denied {
		t.Fatal("the grant still holds after RevokeRole")
	}

	if restoreErr := svc.RestoreRole(tenantCtx, sub, "region-reader", rbac.Scope{NodeID: "node-7"}); restoreErr != nil {
		t.Fatalf("RestoreRole: %v, want success", restoreErr)
	}

	allowed, err := svc.Can(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after restore: %v", err)
	}
	if !allowed {
		t.Fatal("the grant does not hold after RestoreRole")
	}

	scope, err := svc.DataScope(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope after restore: %v", err)
	}
	if scope.TenantWide {
		t.Fatal("the restored binding reports tenant-wide scope, want the original node-7 scope preserved")
	}
}

// TestPostgres_RestoreRole_NothingToRestore_IsReported re-runs
// assign_test.go's TestService_RestoreRole_NothingToRestore_IsReported
// against a real PostgreSQL server, so the "nothing soft-deleted matches
// this tuple" path is proven on the dialect whose partial index actually
// enforces the narrowed uniqueness this round adds.
func TestPostgres_RestoreRole_NothingToRestore_IsReported(t *testing.T) {
	ctx := context.Background()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, pkgcore.NewMemoryEventBus())

	tenantCtx := tenantContext("tenant-a")
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if _, err := svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "region-reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}

	if err := svc.RestoreRole(tenantCtx, sub, "region-reader", rbac.Scope{}); err == nil {
		t.Fatal("RestoreRole with nothing ever granted at this scope = nil, want an error")
	}
}

// TestPostgres_RevokeOriginMarker_ScopesTheOrgRestorePaths re-proves the
// revoke-origin marker round (migrations/{sqlite,postgres}/
// 0003_add_revoke_origin.sql) against a real PostgreSQL server: the 0003
// ALTER applies, the origin-aware mark-deletes and the claim step write
// their values, and the org restore-side scoping -- a deliberate
// revocation surviving a member restore, and a node restore refusing a
// row the member-removal claimed -- behaves identically on the second
// dialect. The flows are driven through the same events the unit tier
// uses (the org event names are protocol strings rbac only listens to),
// with the rows and their origins read back through raw SQL.
func TestPostgres_RevokeOriginMarker_ScopesTheOrgRestorePaths(t *testing.T) {
	ctx := context.Background()
	bus := pkgcore.NewMemoryEventBus()
	db := openRBACPostgres(t, ctx, startPostgresContainer(t, ctx))
	svc := attachRBACService(t, db, bus)

	tenantCtx := tenantContext("tenant-a")
	define := func(key string) {
		t.Helper()
		if _, err := svc.DefineRole(tenantCtx, rbac.RoleDefinition{
			Key:            key,
			DescriptionKey: "rbac.role.member",
			Permissions:    []string{"notes:read"},
		}); err != nil {
			t.Fatalf("DefineRole(%s): %v", key, err)
		}
	}
	publish := func(eventType string, tenant string, payload any) {
		t.Helper()
		if err := bus.Publish(tenantContext(pkgcore.TenantID(tenant)), pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID(tenant),
			Payload:  payload,
		}); err != nil {
			t.Fatalf("publishing %s: %v", eventType, err)
		}
	}
	readOrigin := func(userID, nodeID string) string {
		t.Helper()
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatalf("db.DB(): %v", err)
		}
		var origin string
		if err = sqlDB.QueryRowContext(ctx,
			`SELECT revoke_origin FROM rbac_role_bindings
			 WHERE tenant_id = $1 AND user_id = $2 AND node_id = $3 AND deleted_at IS NOT NULL`,
			"tenant-a", userID, nodeID).Scan(&origin); err != nil {
			t.Fatalf("reading %s's revoked row origin: %v", userID, err)
		}
		return origin
	}

	define("reader")
	owner := rbac.Subject{TenantID: "tenant-a", UserID: "user-owner"}
	kept := rbac.Subject{TenantID: "tenant-a", UserID: "user-kept"}
	gone := rbac.Subject{TenantID: "tenant-a", UserID: "user-gone"}

	// Leg 1: a deliberate revocation survives a member removal-then-restore.
	if err := svc.AssignRole(tenantCtx, owner, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole(owner): %v", err)
	}
	if err := svc.RevokeRole(tenantCtx, owner, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("RevokeRole(owner): %v", err)
	}
	if origin := readOrigin(owner.UserID, ""); origin != "" {
		t.Fatalf("the deliberate revoke wrote revoke_origin %q, want the empty (deliberate) value", origin)
	}
	publish("org.member.removed", "tenant-a", map[string]any{"user_id": owner.UserID})
	publish("org.member.restored", "tenant-a", map[string]any{"user_id": owner.UserID})
	if ok, err := svc.Can(ctx, owner, "read", "notes"); err != nil || ok {
		t.Fatalf("the owner's deliberately revoked grant came back with the member restore: Can = %v, %v; want false", ok, err)
	}

	// Leg 2: a node restore re-instates the row the node deletion reaped
	// for a member who is still in the tenant, and refuses the row the
	// member-removal claimed after the member left.
	for _, sub := range []rbac.Subject{kept, gone} {
		if err := svc.AssignRole(tenantCtx, sub, "reader", rbac.Scope{NodeID: "node-1"}); err != nil {
			t.Fatalf("AssignRole(%s): %v", sub.UserID, err)
		}
	}
	publish("org.node.deleted", "tenant-a", map[string]any{"deleted_node_ids": []any{"node-1"}})
	if origin := readOrigin(kept.UserID, "node-1"); origin != "node-deletion" {
		t.Fatalf("the node-deletion reap wrote revoke_origin %q, want node-deletion", origin)
	}
	publish("org.member.removed", "tenant-a", map[string]any{"user_id": gone.UserID})
	if origin := readOrigin(gone.UserID, "node-1"); origin != "member-removal" {
		t.Fatalf("the claim left the departed member's row at revoke_origin %q, want member-removal", origin)
	}
	if origin := readOrigin(kept.UserID, "node-1"); origin != "node-deletion" {
		t.Fatalf("the claim touched the kept member's row: revoke_origin %q, want node-deletion", origin)
	}
	publish("org.node.restored", "tenant-a", map[string]any{"node_id": "node-1"})
	if ok, err := svc.Can(ctx, kept, "read", "notes"); err != nil || !ok {
		t.Fatalf("the kept member's reaped grant was not re-instated: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(ctx, gone, "read", "notes"); err != nil || ok {
		t.Fatalf("the departed member regained authorization with the node: Can = %v, %v; want false", ok, err)
	}
}
