package admin

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeNotifier records every Dispatch call it receives, standing in for a
// real *notification.DeliveryService (Notifier's own doc comment explains
// why this interface exists).
type fakeNotifier struct {
	dispatches []notification.Dispatch
	contexts   []context.Context
	failWith   error
}

func (f *fakeNotifier) Dispatch(ctx context.Context, d notification.Dispatch) (jobs.JobID, error) {
	if f.failWith != nil {
		return "", f.failWith
	}
	f.dispatches = append(f.dispatches, d)
	f.contexts = append(f.contexts, ctx)
	return "job-1", nil
}

// newTestImpersonationService wires an ImpersonationService over a fresh
// database and a real in-process EventBus/AuditActionRegistrar, with the
// given notifier (nil is legal: Start then simply skips the notification).
func newTestImpersonationService(t *testing.T, notifier Notifier) (*ImpersonationService, *pkgcore.Registry) {
	t.Helper()
	// RegisterSystemPurpose is process-global and idempotent -- see its own
	// doc comment -- so calling it here, in every test that exercises
	// Start's tenancy.WithSystemContext grant, is safe regardless of test
	// order or repetition.
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)
	db := testutil.NewDB(t)
	svc := NewImpersonationService(NewImpersonationRepository(db))
	reg := newTestRegistry()
	if err := reg.AuditActions.Add(AuditActionImpersonationStarted, AuditActionImpersonationEnded); err != nil {
		t.Fatalf("register audit actions: %v", err)
	}
	svc.attach(reg.EventBus(), reg.AuditActions, notifier)
	return svc, reg
}

func startTestGrant(t *testing.T, svc *ImpersonationService) *ImpersonationGrant {
	t.Helper()
	grant, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         "support ticket #42",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return grant
}

// --- Start: validation --------------------------------------------------

func TestImpersonationService_Start_EmptyReason_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1"})
	if !isCode(err, ErrImpersonationReasonRequired.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationReasonRequired", err)
	}
}

func TestImpersonationService_Start_MissingTarget_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{AdminUserID: "admin-1", Reason: "x"})
	if !isCode(err, ErrImpersonationTargetRequired.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationTargetRequired", err)
	}
}

func TestImpersonationService_Start_SelfTarget_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "admin-1", TargetTenantID: "tenant-1", Reason: "x",
	})
	if !isCode(err, ErrImpersonationSelfNotAllowed.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationSelfNotAllowed", err)
	}
}

// --- Property (d): dual-identity audit on start and end -----------------

func TestImpersonationService_Start_RecordsDualIdentityAuditEvent(t *testing.T) {
	svc, reg := newTestImpersonationService(t, &fakeNotifier{})
	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	grant := startTestGrant(t, svc)

	if len(recorded) != 1 {
		t.Fatalf("got %d recorded audit events on Start, want exactly 1", len(recorded))
	}
	evt := recorded[0]
	if evt.Action != AuditActionImpersonationStarted {
		t.Fatalf("Action = %q, want %q", evt.Action, AuditActionImpersonationStarted)
	}
	// Property (d): Actor is the impersonated (target) user, OnBehalfOf is
	// the real administrator -- never the other way around.
	if evt.Actor.Type != pkgcore.ActorTypeUser || evt.Actor.ID != "user-1" {
		t.Fatalf("Actor = %+v, want {Type: user, ID: user-1}", evt.Actor)
	}
	if evt.OnBehalfOf == nil || evt.OnBehalfOf.Type != pkgcore.ActorTypePlatformAdmin || evt.OnBehalfOf.ID != "admin-1" {
		t.Fatalf("OnBehalfOf = %+v, want {Type: platform_admin, ID: admin-1}", evt.OnBehalfOf)
	}
	if evt.Resource.ID != grant.ID {
		t.Fatalf("Resource.ID = %q, want the grant id %q", evt.Resource.ID, grant.ID)
	}
}

func TestImpersonationService_End_RecordsDualIdentityAuditEvent(t *testing.T) {
	svc, reg := newTestImpersonationService(t, &fakeNotifier{})
	grant := startTestGrant(t, svc)

	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	if _, err := svc.End(context.Background(), grant.ID, "admin-1"); err != nil {
		t.Fatalf("End() error = %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("got %d recorded audit events on End, want exactly 1", len(recorded))
	}
	evt := recorded[0]
	if evt.Action != AuditActionImpersonationEnded {
		t.Fatalf("Action = %q, want %q", evt.Action, AuditActionImpersonationEnded)
	}
	if evt.Actor.Type != pkgcore.ActorTypeUser || evt.Actor.ID != "user-1" {
		t.Fatalf("Actor = %+v, want {Type: user, ID: user-1} (still the impersonated user, even on End)", evt.Actor)
	}
	if evt.OnBehalfOf == nil || evt.OnBehalfOf.ID != "admin-1" {
		t.Fatalf("OnBehalfOf = %+v, want the ending admin", evt.OnBehalfOf)
	}
}

// --- Property (e): mandatory notification on Start -----------------------

func TestImpersonationService_Start_DispatchesMandatoryNotification(t *testing.T) {
	notifier := &fakeNotifier{}
	svc, _ := newTestImpersonationService(t, notifier)

	startTestGrant(t, svc)

	if len(notifier.dispatches) != 1 {
		t.Fatalf("got %d notification dispatches, want exactly 1", len(notifier.dispatches))
	}
	d := notifier.dispatches[0]
	if d.TypeKey != NotificationTypeImpersonationStarted {
		t.Fatalf("TypeKey = %q, want %q", d.TypeKey, NotificationTypeImpersonationStarted)
	}
	if d.Recipient.Class != notification.RecipientClassUser || d.Recipient.UserID != "user-1" {
		t.Fatalf("Recipient = %+v, want the target user", d.Recipient)
	}
	// The dispatch must run under the TARGET tenant's context (D2's
	// mechanism: a system context scoped to the target tenant), never the
	// administrator's own ambient tenant.
	tenant, ok := pkgcore.TenantFromContext(notifier.contexts[0])
	if !ok || tenant != "tenant-1" {
		t.Fatalf("dispatch tenant = %q, ok=%v, want tenant-1", tenant, ok)
	}
}

func TestImpersonationService_Start_NotifierFailure_DoesNotFailStart(t *testing.T) {
	notifier := &fakeNotifier{failWith: context.DeadlineExceeded}
	svc, _ := newTestImpersonationService(t, notifier)

	// Start must still succeed even though the notifier fails -- see
	// notifyStarted's own doc comment for why a notification failure must
	// never undo an already-created grant.
	grant := startTestGrant(t, svc)
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}
}

func TestImpersonationService_Start_NilNotifier_StillSucceeds(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	grant := startTestGrant(t, svc)
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}
}

// --- Property (c): fail-closed on invalid/expired/ended grants -----------

func TestImpersonationService_Lookup_ValidActiveGrant_Found(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	grant := startTestGrant(t, svc)

	got, ok := svc.Lookup(context.Background(), grant.ID)
	if !ok || got.ID != grant.ID {
		t.Fatalf("Lookup() = %+v, %v, want the started grant", got, ok)
	}
}

func TestImpersonationService_Lookup_UnknownID_NotFoundNeverError(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	got, ok := svc.Lookup(context.Background(), "does-not-exist")
	if ok || got != nil {
		t.Fatalf("Lookup() = %+v, %v, want (nil, false)", got, ok)
	}
}

func TestImpersonationService_Lookup_EmptyID_NotFound(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	got, ok := svc.Lookup(context.Background(), "")
	if ok || got != nil {
		t.Fatalf("Lookup(\"\") = %+v, %v, want (nil, false)", got, ok)
	}
}

func TestImpersonationService_Lookup_ExpiredGrant_NotFound(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }

	grant := startTestGrant(t, svc)

	// Advance the clock past the grant's natural expiry.
	svc.now = func() time.Time { return frozen.Add(defaultGrantTTL + time.Minute) }

	got, ok := svc.Lookup(context.Background(), grant.ID)
	if ok || got != nil {
		t.Fatalf("Lookup() on an expired grant = %+v, %v, want (nil, false) -- must fall back to the admin's own identity, never impersonate", got, ok)
	}
}

func TestImpersonationService_Lookup_EndedGrant_NotFound(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	grant := startTestGrant(t, svc)

	if _, err := svc.End(context.Background(), grant.ID, "admin-1"); err != nil {
		t.Fatalf("End() error = %v", err)
	}

	got, ok := svc.Lookup(context.Background(), grant.ID)
	if ok || got != nil {
		t.Fatalf("Lookup() on an ended grant = %+v, %v, want (nil, false)", got, ok)
	}
}

// --- End: idempotence and errors ------------------------------------------

func TestImpersonationService_End_AlreadyEnded_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	grant := startTestGrant(t, svc)

	if _, err := svc.End(context.Background(), grant.ID, "admin-1"); err != nil {
		t.Fatalf("first End() error = %v", err)
	}
	_, err := svc.End(context.Background(), grant.ID, "admin-1")
	if !isCode(err, ErrImpersonationGrantEnded.Code) {
		t.Fatalf("second End() error = %v, want ErrImpersonationGrantEnded", err)
	}
}

func TestImpersonationService_End_UnknownID_ReportsNotFound(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.End(context.Background(), "does-not-exist", "admin-1")
	if !isCode(err, ErrGrantNotFound.Code) {
		t.Fatalf("End() error = %v, want ErrGrantNotFound", err)
	}
}

// --- ListActive ------------------------------------------------------------

func TestImpersonationService_ListActive_ExcludesEnded(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)

	active := startTestGrant(t, svc)
	ended, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "user-2", TargetTenantID: "tenant-1", Reason: "x",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, endErr := svc.End(context.Background(), ended.ID, "admin-1"); endErr != nil {
		t.Fatalf("End() error = %v", endErr)
	}

	rows, err := svc.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}
	if len(rows) != 1 || rows[0].ID != active.ID {
		t.Fatalf("ListActive() = %+v, want exactly the still-active grant %q", rows, active.ID)
	}
}

func TestImpersonationService_ListActive_ExcludesExpired(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	frozen := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return frozen }

	expired, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "user-2", TargetTenantID: "tenant-1", Reason: "x",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	svc.now = func() time.Time { return frozen.Add(defaultGrantTTL + time.Minute) }
	stillActive, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "user-3", TargetTenantID: "tenant-1", Reason: "x",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	rows, err := svc.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}
	if len(rows) != 1 || rows[0].ID != stillActive.ID {
		t.Fatalf("ListActive() = %+v, want exactly %q (the expired grant %q must be excluded)", rows, stillActive.ID, expired.ID)
	}
}
