package audit

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

func TestAuditEvent_TableName(t *testing.T) {
	if want := "audit_events"; (AuditEvent{}).TableName() != want {
		t.Fatalf("AuditEvent{}.TableName() = %q, want %q", (AuditEvent{}).TableName(), want)
	}
}

func TestAuditEvent_SetActor_Actor_RoundTrip(t *testing.T) {
	want := pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"}
	var evt AuditEvent
	evt.SetActor(want)
	if got := evt.Actor(); got != want {
		t.Errorf("Actor() = %+v, want %+v", got, want)
	}
}

func TestAuditEvent_SetOnBehalfOf_OnBehalfOf_RoundTrip(t *testing.T) {
	want := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1", DisplayName: "Grace"}
	var evt AuditEvent
	evt.SetOnBehalfOf(&want)

	got, ok := evt.OnBehalfOf()
	if !ok {
		t.Fatalf("OnBehalfOf() ok = false, want true after SetOnBehalfOf(&want)")
	}
	if got != want {
		t.Errorf("OnBehalfOf() = %+v, want %+v", got, want)
	}
}

func TestAuditEvent_OnBehalfOf_AbsentByDefault(t *testing.T) {
	var evt AuditEvent
	_, ok := evt.OnBehalfOf()
	if ok {
		t.Errorf("OnBehalfOf() ok = true on a zero-value AuditEvent, want false -- no impersonation must mean NULL, not a zero Actor")
	}
}

func TestAuditEvent_SetOnBehalfOf_Nil_ClearsIt(t *testing.T) {
	admin := pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"}
	var evt AuditEvent
	evt.SetOnBehalfOf(&admin)
	evt.SetOnBehalfOf(nil)

	if _, ok := evt.OnBehalfOf(); ok {
		t.Errorf("OnBehalfOf() ok = true after SetOnBehalfOf(nil), want false")
	}
	if evt.OnBehalfOfType != nil || evt.OnBehalfOfID != nil || evt.OnBehalfOfDisplayName != nil {
		t.Errorf("SetOnBehalfOf(nil) left a non-nil OnBehalfOf* column: type=%v id=%v displayName=%v, want all three nil",
			evt.OnBehalfOfType, evt.OnBehalfOfID, evt.OnBehalfOfDisplayName)
	}
}

func TestAuditEvent_SetResource_Resource_RoundTrip(t *testing.T) {
	want := Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"}
	var evt AuditEvent
	evt.SetResource(want)
	if got := evt.Resource(); got != want {
		t.Errorf("Resource() = %+v, want %+v", got, want)
	}
}

func TestAuditEvent_SetResult_Result_RoundTrip(t *testing.T) {
	want := Result{Success: false, FailureReason: "permission denied"}
	var evt AuditEvent
	evt.SetResult(want)
	if got := evt.Result(); got != want {
		t.Errorf("Result() = %+v, want %+v", got, want)
	}
}

// TestAuditEvent_DoesNotImplementTenantScoped and
// TestAuditEvent_VisibilityDoesNotDependOnTenantContext together prove the
// reverse-and-equally-important property this round's scope-freeze report
// calls for: audit_events is platform data, not tenant data, so dbkit's
// tenant-isolation plugin must leave it genuinely unaffected -- exactly
// like go/config's row and go/jobs's jobRecord already prove of
// themselves.
//
// Both files that ship that exact proof (go/config/model_test.go,
// go/jobs/store_test.go) call tenancytest.AssertNotTenantScoped, from
// go/tenancy/tenancytest. This package cannot do the same:
// go/tenancy/tenancytest imports go/dbkit itself (its AssertIsolated and
// AssertNotTenantScoped both drive dbkit.Repository[T] and
// dbkit.TenantScoped), and go/tenancy sits ABOVE go/dbkit in the module
// dependency graph (pkgcore -> dbkit -> tenancy -> config/jobs -> ...).
// go/dbkit/audit is a subpackage of dbkit itself, so importing
// go/tenancy/tenancytest from here would make dbkit depend on tenancy --
// inverting the direction CLAUDE.md's "Dependencies flow strictly
// bottom-up" rule requires, and reintroducing exactly the kind of module
// cycle (tenancy already requires dbkit) that rule exists to rule out.
// These two tests reproduce AssertNotTenantScoped's two checks by hand
// instead, using only dbkit and pkgcore -- the same two modules this
// round's scope-freeze report says go/dbkit/audit may depend on.
func TestAuditEvent_DoesNotImplementTenantScoped(t *testing.T) {
	if _, ok := any(AuditEvent{}).(dbkit.TenantScoped); ok {
		t.Fatalf("AuditEvent implements dbkit.TenantScoped; audit_events is platform data (see model.go's own doc comment) and must not be tenant-scoped")
	}
	if _, ok := any(&AuditEvent{}).(dbkit.TenantScoped); ok {
		t.Fatalf("*AuditEvent implements dbkit.TenantScoped; audit_events is platform data (see model.go's own doc comment) and must not be tenant-scoped")
	}
}

func TestAuditEvent_VisibilityDoesNotDependOnTenantContext(t *testing.T) {
	db := openAuditTestDB(t)
	repo := NewRepository(db)

	evt := sampleEvent()
	evt.TenantID = ""
	evt.Action = "audit.visibility.probe"

	createdUnder := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	if err := repo.Insert(createdUnder, evt); err != nil {
		t.Fatalf("Insert() under a tenant-a context error = %v", err)
	}

	readContexts := map[string]context.Context{
		"the same tenant it was created under": createdUnder,
		"a different tenant":                   pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-b")),
		"no tenant at all":                     context.Background(),
	}
	for name, ctx := range readContexts {
		got, err := repo.Get(ctx, evt.ID)
		if err != nil {
			t.Fatalf("Get() under %s error = %v", name, err)
		}
		if got == nil {
			t.Errorf("Get() under %s = nil, want the row: a genuinely non-tenant-scoped model must be visible regardless of what tenant, if any, is in the caller's context", name)
		}
	}
}
