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

// insertImpersonationEvent inserts one audit event written during an
// impersonation session: Actor is the impersonated user the session
// substituted, OnBehalfOf the real administrator behind it (pkgcore's
// dual-identity rule) -- the row shape admin's own impersonation
// pipeline writes (go/admin/pipeline.go).
func insertImpersonationEvent(t *testing.T, repo *audit.Repository, tenant, actorID, onBehalfOfID, resourceType, action string, occurredAt time.Time, success bool) {
	t.Helper()
	evt := &audit.AuditEvent{
		TenantID:   tenant,
		Action:     action,
		OccurredAt: occurredAt,
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID, DisplayName: actorID})
	evt.SetOnBehalfOf(&pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: onBehalfOfID, DisplayName: onBehalfOfID})
	evt.SetResource(audit.Resource{Type: resourceType, ID: "r-1", DisplayName: "r-1"})
	evt.SetResult(audit.Result{Success: success})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert impersonation audit event: %v", err)
	}
}

// TestAuditQuery_Query_FiltersByOnBehalfOf is the regression test for
// the impersonation-accountability read dimension: an audit row written
// during an impersonation session carries the impersonated user as Actor
// and the real administrator as OnBehalfOf (pkgcore's dual-identity
// rule), so a QueryFilter on OnBehalfOf must return exactly that
// administrator's impersonation-era rows and no others -- not a second
// administrator's impersonation rows, and not the administrator's own
// non-impersonation rows, which carry no OnBehalfOf identity at all.
// A read surface stopping at Actor could not express this query, and an
// administrator -- who never appears as Actor on an impersonation-era row
// -- would be unfindable on the read side the dual-identity rule exists
// to serve.
func TestAuditQuery_Query_FiltersByOnBehalfOf(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	base := time.Now().Add(-time.Hour)
	insertImpersonationEvent(t, repo, "tenant-a", "impersonated-user-a", "admin-1", "note", "notes.note.create", base, true)
	insertImpersonationEvent(t, repo, "tenant-a", "impersonated-user-b", "admin-1", "note", "notes.note.update", base.Add(time.Minute), true)
	insertImpersonationEvent(t, repo, "tenant-a", "impersonated-user-c", "admin-2", "note", "notes.note.delete", base.Add(2*time.Minute), true)
	insertAuditEvent(t, repo, "tenant-a", "admin-1", "note", "notes.note.read", base.Add(3*time.Minute), true)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	events, err := q.Query(ctx, QueryFilter{OnBehalfOf: "admin-1"})
	if err != nil {
		t.Fatalf("Query(OnBehalfOf=admin-1): %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Query(OnBehalfOf=admin-1) = %d events, want exactly 2 -- admin-1's two impersonation-era rows and no others", len(events))
	}
	actors := make(map[string]bool, len(events))
	for _, evt := range events {
		onBehalfOf, ok := evt.OnBehalfOf()
		if !ok || onBehalfOf.ID != "admin-1" {
			t.Errorf("event %s matches OnBehalfOf=admin-1 yet reads back on_behalf_of %v (ok=%v)", evt.ID, onBehalfOf, ok)
			continue
		}
		actors[evt.ActorID] = true
	}
	if !actors["impersonated-user-a"] || !actors["impersonated-user-b"] {
		t.Errorf("Query(OnBehalfOf=admin-1) actors = %v, want impersonated-user-a and impersonated-user-b -- admin-2's impersonation row and admin-1's own direct row must be excluded", actors)
	}
}

// TestAuditQuery_Query_SameTimestampEventsOrderDeterministically is the
// regression test for the sort's ID tiebreaker: several events sharing an
// identical OccurredAt must come back in a fixed, deterministic order --
// by ID descending, per filterAndSort's documented total order -- on every
// query, so a caller paging over the returned slice never sees
// same-timestamp events reorder between requests. dbkit/audit's own
// ListByTenant orders by occurred_at alone and sort.Slice is not stable,
// so without the tiebreaker the order of tied rows is whatever the
// database's index scan returns (SQLite returns them newest-rowid-first,
// and a second database or query plan could return them differently) --
// the instability this test pins the documented total order against.
func TestAuditQuery_Query_SameTimestampEventsOrderDeterministically(t *testing.T) {
	repo := audit.NewRepository(newTestAuditDB(t))
	q := NewAuditQuery(repo)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	// Three events sharing one exact timestamp, inserted in an order that
	// deliberately disagrees with the documented ID-descending tiebreak,
	// plus one newer event that must sort ahead of all three. Inserting
	// ties in descending ID order matters: dbkit/audit's own ListByTenant
	// orders by occurred_at alone, and SQLite's index scan returns tied
	// rows newest-rowid-first, so without the tiebreaker the sort's
	// insertion-order-preserving behavior on equal keys would hand these
	// back ascending -- exactly the reorder this test pins against.
	for _, id := range []string{"tie-c", "tie-b", "tie-a"} {
		evt := &audit.AuditEvent{
			ID:         id,
			TenantID:   "tenant-a",
			Action:     "notes.note.create",
			OccurredAt: at,
		}
		evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "user-1"})
		evt.SetResource(audit.Resource{Type: "note", ID: "r-" + id, DisplayName: "r-" + id})
		evt.SetResult(audit.Result{Success: true})
		if err := repo.Insert(context.Background(), evt); err != nil {
			t.Fatalf("insert audit event %q: %v", id, err)
		}
	}
	insertAuditEvent(t, repo, "tenant-a", "user-1", "note", "notes.note.create", at.Add(time.Minute), true)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	readIDs := func() []string {
		events, err := q.Query(ctx, QueryFilter{})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		got := make([]string, 0, len(events))
		for _, evt := range events {
			got = append(got, evt.ID)
		}
		return got
	}

	// Two separate queries (what two pagination requests over the same
	// underlying rows would do) must return the identical order, newest
	// first with the same-timestamp events broken by ID descending.
	first := readIDs()
	second := readIDs()
	if len(first) != 4 {
		t.Fatalf("Query returned %d events, want 4", len(first))
	}
	want := []string{"tie-c", "tie-b", "tie-a"}
	for i, evt := range first {
		if i == 0 {
			if evt == "tie-a" || evt == "tie-b" || evt == "tie-c" {
				t.Errorf("first event = %q, want the newer (one-minute-later) event first", evt)
			}
			continue
		}
		if evt != want[i-1] {
			t.Errorf("event %d = %q, want %q -- same-timestamp events must order by ID descending", i, evt, want[i-1])
		}
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("two queries returned different orders: %v then %v -- pagination over the slice can reorder same-timestamp events", first, second)
		}
	}
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
