package compliance

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// insertAuditEvent inserts one AuditEvent directly through repo, for
// AuditQuery tests that need rows already on the table rather than routed
// through Emit.
func insertAuditEvent(t *testing.T, repo *audit.Repository, tenant, actorID, resourceType, action string, occurredAt time.Time, success bool) {
	t.Helper()
	evt := &audit.AuditEvent{
		TenantID:   tenant,
		Action:     action,
		OccurredAt: occurredAt,
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID, DisplayName: actorID})
	evt.SetResource(audit.Resource{Type: resourceType, ID: "r-1", DisplayName: "r-1"})
	evt.SetResult(audit.Result{Success: success})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
}

// TestAuditQuery_Query_ScopedToCtxTenant proves the tenant-scoped read
// path: a caller's ctx tenant selects which events Query can ever see,
// and a caller-supplied tenant is not possible -- there is no parameter
// for one.
func TestAuditQuery_Query_ScopedToCtxTenant(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	now := time.Now()
	insertAuditEvent(t, repo, "tenant-a", "user-1", "note", "notes.note.create", now, true)
	insertAuditEvent(t, repo, "tenant-b", "user-2", "note", "notes.note.create", now, true)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	events, err := q.Query(ctx, QueryFilter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(events) != 1 || events[0].TenantID != "tenant-a" {
		t.Fatalf("Query(tenant-a) = %+v, want exactly tenant-a's one event", events)
	}
}

// TestAuditQuery_Query_NoTenantInContextFails pins the fail-closed rule.
func TestAuditQuery_Query_NoTenantInContextFails(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	if _, err := q.Query(context.Background(), QueryFilter{}); err == nil {
		t.Error("Query with no tenant in context = nil error, want pkgcore.ErrNoTenant")
	}
}

// TestAuditQuery_Query_FiltersByEveryField proves each QueryFilter field
// narrows the result independently.
func TestAuditQuery_Query_FiltersByEveryField(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	base := time.Now().Add(-time.Hour)
	insertAuditEvent(t, repo, "tenant-a", "user-1", "note", "notes.note.create", base, true)
	insertAuditEvent(t, repo, "tenant-a", "user-2", "org.member", "org.member.remove", base.Add(time.Minute), false)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	t.Run("by actor", func(t *testing.T) {
		events, err := q.Query(ctx, QueryFilter{Actor: "user-1"})
		if err != nil || len(events) != 1 || events[0].ActorID != "user-1" {
			t.Errorf("Query(Actor=user-1) = %+v, err=%v", events, err)
		}
	})
	t.Run("by resource", func(t *testing.T) {
		events, err := q.Query(ctx, QueryFilter{Resource: "org.member"})
		if err != nil || len(events) != 1 || events[0].ResourceType != "org.member" {
			t.Errorf("Query(Resource=org.member) = %+v, err=%v", events, err)
		}
	})
	t.Run("by action", func(t *testing.T) {
		events, err := q.Query(ctx, QueryFilter{Action: "notes.note.create"})
		if err != nil || len(events) != 1 {
			t.Errorf("Query(Action=notes.note.create) = %+v, err=%v", events, err)
		}
	})
	t.Run("by success", func(t *testing.T) {
		fail := false
		events, err := q.Query(ctx, QueryFilter{Success: &fail})
		if err != nil || len(events) != 1 || events[0].Success {
			t.Errorf("Query(Success=false) = %+v, err=%v", events, err)
		}
	})
	t.Run("by time range", func(t *testing.T) {
		events, err := q.Query(ctx, QueryFilter{From: base.Add(30 * time.Second)})
		if err != nil || len(events) != 1 {
			t.Errorf("Query(From after first event) = %+v, err=%v", events, err)
		}
	})
	t.Run("newest first", func(t *testing.T) {
		events, err := q.Query(ctx, QueryFilter{})
		if err != nil || len(events) != 2 {
			t.Fatalf("Query() = %+v, err=%v", events, err)
		}
		if !events[0].OccurredAt.After(events[1].OccurredAt) {
			t.Errorf("events not newest-first: %+v", events)
		}
	})
}

// TestAuditQuery_QueryAcrossTenants_RequiresSystemContext pins the gate.
func TestAuditQuery_QueryAcrossTenants_RequiresSystemContext(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	_, err := q.QueryAcrossTenants(context.Background(), []string{"tenant-a", "tenant-b"}, QueryFilter{})
	if !hasCode(err, ErrAuditQueryRequiresSystemContext.Code) {
		t.Fatalf("QueryAcrossTenants without a system context error = %v, want %s", err, ErrAuditQueryRequiresSystemContext.Code)
	}
}

// TestAuditQuery_QueryAcrossTenants_MergesNamedTenants proves the
// platform-admin read path merges results from every named tenant.
func TestAuditQuery_QueryAcrossTenants_MergesNamedTenants(t *testing.T) {
	const purpose pkgcore.SystemPurpose = "compliance_test.audit_query"
	pkgcore.RegisterSystemPurpose(purpose)

	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	now := time.Now()
	insertAuditEvent(t, repo, "tenant-a", "user-1", "note", "notes.note.create", now, true)
	insertAuditEvent(t, repo, "tenant-b", "user-2", "note", "notes.note.create", now.Add(time.Minute), true)
	insertAuditEvent(t, repo, "tenant-c", "user-3", "note", "notes.note.create", now.Add(2*time.Minute), true)

	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Actor: "platform-admin", Purpose: purpose})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}

	events, err := q.QueryAcrossTenants(sysCtx, []string{"tenant-a", "tenant-b"}, QueryFilter{})
	if err != nil {
		t.Fatalf("QueryAcrossTenants: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("QueryAcrossTenants([a,b]) = %d events, want 2 (tenant-c excluded)", len(events))
	}
}

// TestAuditQuery_Get_ReturnsNilForAMissingID pins the passthrough
// behavior.
func TestAuditQuery_Get_ReturnsNilForAMissingID(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	evt, err := q.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if evt != nil {
		t.Errorf("Get(missing) = %+v, want nil", evt)
	}
}
