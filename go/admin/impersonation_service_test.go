package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
	rbacmigrations "github.com/vislake/speed/go/rbac/migrations"
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
// given notifier (nil is legal for these in-package tests: attach has run,
// so Start proceeds with its validate-and-notify pass skipped -- a shape
// only in-package wiring can produce, since Start refuses outright any
// service attach never ran on; see
// TestImpersonationService_Start_UnwiredService_Refused).
func newTestImpersonationService(t *testing.T, notifier Notifier) (*ImpersonationService, *pkgcore.Registry) {
	t.Helper()
	// RegisterSystemPurpose is process-global and idempotent -- see its own
	// doc comment -- so calling it here, in every test that exercises
	// Start's tenancy.WithSystemContext grant, is safe regardless of test
	// order or repetition.
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)
	db := testutil.NewDB(t)
	svc := newImpersonationService(NewImpersonationRepository(db))
	reg := newTestRegistry()
	if err := reg.AuditActions.Add(AuditActionImpersonationStarted, AuditActionImpersonationEnded); err != nil {
		t.Fatalf("register audit actions: %v", err)
	}
	// authnSvc and members are both deliberately nil here: every test in
	// this file supplies an explicit StartInput.Locale (startTestGrant
	// does), which resolveNotificationLocale's own nil-authnSvc branch
	// trusts verbatim without ever touching authnSvc -- see that method's
	// own doc comment -- and a nil members skips validateTargetMembership
	// entirely (its own doc comment). Resolving a real target's existence,
	// locale and tenant membership through genuine *authn.Service and
	// *org.MemberService instances is pinned separately, against real
	// authn/org/notification modules together, by the
	// TestImpersonationService_Start_* tests in
	// impersonation_service_start_dispatch_test.go.
	svc.attach(reg.EventBus(), reg.AuditActions, notifier, nil, nil)
	// rbacSvc is deliberately NOT left nil the way authnSvc and members
	// are: Start refuses while it is nil (its own doc comment), and nearly
	// every test in this file calls Start. Attach a real, Attach()-ed *rbac.Service over the same db,
	// exactly as a wired host's post-Bootstrap Module.AttachRBAC would;
	// the pre-attach refusal itself is pinned separately, by
	// TestImpersonationService_Start_BeforeAttachRBAC_Refused.
	svc.attachRBAC(newAttachedRBAC(t, db))
	return svc, reg
}

// newAttachedRBAC returns a real, Attach()-ed *rbac.Service over db --
// the minimal construction go/rbac permits (its own migrations applied
// from zero, a fresh Kernel.Bootstrap, and Module.Attach), the same shape
// buildTestAdminModule's env.RBAC goes through for the full-graph tests.
// The lightweight service tests need a non-nil rbacSvc only because
// Start's own gate demands one; nothing here ever invokes the service's
// methods, which is why this construction -- whose frozen catalog carries
// none of admin's declared permissions -- is sufficient.
func newAttachedRBAC(t *testing.T, db *gorm.DB) *rbac.Service {
	t.Helper()
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: "rbac", FS: rbacmigrations.FS})
	rbacModule := rbac.NewModule(db)
	reg, err := pkgcore.NewKernel().Bootstrap(t.Context(), rbacModule)
	if err != nil {
		t.Fatalf("bootstrap the rbac module: %v", err)
	}
	svc, err := rbacModule.Attach(reg)
	if err != nil {
		t.Fatalf("rbacModule.Attach() error = %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// startTestGrant starts a grant with an explicit Locale, so every test in
// this file that does not care about locale resolution itself never needs
// a real *authn.Service wired.
func startTestGrant(t *testing.T, svc *ImpersonationService) *ImpersonationGrant {
	t.Helper()
	grant, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         "support ticket #42",
		Locale:         "zh-CN",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return grant
}

// --- fail-closed while the rbac service is unattached ---

// TestImpersonationService_Start_BeforeAttachRBAC_Refused pins the
// fail-closed gate behind grant birth: Start with a nil rbacSvc must
// refuse with ErrRBACServiceRequired -- the same named error
// RoleService.require answers on the identical seam -- and write no grant,
// because the "impersonation grant must not outlive its administrator's
// admin:impersonate permission" guarantee (the endIfNoLongerPermitted
// machinery, onRoleBindingRevoked and onRoleChanged) runs only once
// Module.AttachRBAC has attached a real *rbac.Service, and a host that
// never called it would issue grants the module could never automatically
// end.
//
// The service is built through the in-package constructor and run through
// attach first -- the Module.Register path that flips the attached flag --
// with AttachRBAC deliberately NOT called, so Start passes the
// ErrImpersonationNotWired gate and reaches the rbac gate this test
// exists for. (An unattached service is refused by the earlier gate with
// a different code, pinned by TestImpersonationService_Start_UnwiredService_Refused.)
func TestImpersonationService_Start_BeforeAttachRBAC_Refused(t *testing.T) {
	db := testutil.NewDB(t)
	svc := newImpersonationService(NewImpersonationRepository(db))
	reg := newTestRegistry()
	svc.attach(reg.EventBus(), reg.AuditActions, nil, nil, nil)

	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         "support ticket #42",
		Locale:         "zh-CN",
	})
	if !apperr.HasCode(err, ErrRBACServiceRequired.Code) {
		t.Fatalf("Start() error = %v, want %s (a grant must not be born while the automatic permission-revocation end cannot run)",
			err, ErrRBACServiceRequired.Code)
	}

	active, listErr := svc.ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written while Module.AttachRBAC has not been called", active)
	}
}

// TestImpersonationService_RoleRevokedBeforeAttachRBAC_Warns pins the
// event-side half of the same closure: a rbac revocation event delivered
// while rbacSvc is still nil must not be dropped without a trace. Each
// review path Warns, naming the cause (the rbac service is not attached)
// for the operator who can fix the wiring -- without the Warn, an unwired
// host would lose the automatic-end guarantee with nothing logged at all.
//
// The service is built through the in-package constructor and run through
// attach first, exactly like the Start-refusal test above: the shape that
// matches the Warn's scenario is a host whose Module.Register ran (the
// attached flag is set) but whose post-Bootstrap Module.AttachRBAC call
// never happened.
func TestImpersonationService_RoleRevokedBeforeAttachRBAC_Warns(t *testing.T) {
	db := testutil.NewDB(t)
	svc := newImpersonationService(NewImpersonationRepository(db))
	reg := newTestRegistry()
	svc.attach(reg.EventBus(), reg.AuditActions, nil, nil, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := obs.WithLogger(context.Background(), logger)

	// A role-binding revoke inside rbac.SystemDomain -- the event that
	// matters (onRoleBindingRevoked's own doc comment).
	revoked := pkgcore.Event{
		Type:    rbac.EventRoleBindingRevoked,
		Payload: rbac.RoleBindingChangedEvent{TenantID: string(rbac.SystemDomain), UserID: "admin-1"},
	}
	if err := svc.onRoleBindingRevoked(ctx, revoked); err != nil {
		t.Fatalf("onRoleBindingRevoked() error = %v, want nil (a subscriber must never fail its publisher)", err)
	}

	// A role redefinition in the same domain (rbac.EventRoleChanged names
	// no single subject -- its own doc comment).
	changed := pkgcore.Event{
		Type:    rbac.EventRoleChanged,
		Payload: rbac.RoleChangedEvent{TenantID: string(rbac.SystemDomain), RoleID: "role-1"},
	}
	if err := svc.onRoleChanged(ctx, changed); err != nil {
		t.Fatalf("onRoleChanged() error = %v, want nil (a subscriber must never fail its publisher)", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "rbac service is not attached") {
		t.Fatalf("log output = %q, want a Warn naming the unattached rbac service on the revocation path", logged)
	}
	if strings.Count(logged, "rbac service is not attached") != 2 {
		t.Fatalf("log output = %q, want BOTH review paths (role-binding revoke and role change) to Warn", logged)
	}
}

// --- Start: validation --------------------------------------------------

func TestImpersonationService_Start_EmptyReason_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1"})
	if !apperr.HasCode(err, ErrImpersonationReasonRequired.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationReasonRequired", err)
	}
}

func TestImpersonationService_Start_MissingTarget_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{AdminUserID: "admin-1", Reason: "x"})
	if !apperr.HasCode(err, ErrImpersonationTargetRequired.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationTargetRequired", err)
	}
}

func TestImpersonationService_Start_SelfTarget_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "admin-1", TargetTenantID: "tenant-1", Reason: "x",
	})
	if !apperr.HasCode(err, ErrImpersonationSelfNotAllowed.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationSelfNotAllowed", err)
	}
}

// TestImpersonationService_Start_SystemDomainTarget_Refused pins the
// privilege-escalation refusal: an operator holding only
// admin:impersonate could name rbac.SystemDomain ("system") as
// TargetTenantID against another platform-staff account's user id, and
// the substituted Principal ImpersonationMiddleware installs would then
// be evaluated against every admin:* permission THAT account holds -- not
// merely whatever an ordinary impersonation target should ever grant.
// This must be refused before any grant row is ever written.
func TestImpersonationService_Start_SystemDomainTarget_Refused(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "platform-staff-2",
		TargetTenantID: "system", // rbac.SystemDomain's literal value
		Reason:         "x",
	})
	if !apperr.HasCode(err, ErrImpersonationTargetForbidden.Code) {
		t.Fatalf("Start() error = %v, want ErrImpersonationTargetForbidden", err)
	}

	active, listErr := svc.ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written for a refused system-domain target", active)
	}
}

// --- dual-identity audit on start and end -----------------

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
	// Actor is the impersonated (target) user, OnBehalfOf is the real
	// administrator -- never the other way around.
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

// TestImpersonationService_Start_AuditChanges_CarryTheReason pins the
// reason's place on the audit record: the mandatory reason an operator
// must write to start an impersonation grant -- the reason itself is part
// of the audit -- must arrive on the started event's after map, next to
// the grant's shape, so the dual-identity audit trail is where the
// justification outlives the grant. Without it the reason would live only
// on the grant row, readable while the grant is active and nowhere once
// it ended.
func TestImpersonationService_Start_AuditChanges_CarryTheReason(t *testing.T) {
	svc, reg := newTestImpersonationService(t, &fakeNotifier{})
	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	const reason = "support ticket #42"
	grant, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         reason,
		Locale:         "zh-CN",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(recorded) != 1 {
		t.Fatalf("got %d recorded audit events on Start, want exactly 1", len(recorded))
	}
	evt := recorded[0]
	if evt.Action != AuditActionImpersonationStarted {
		t.Fatalf("Action = %q, want %q", evt.Action, AuditActionImpersonationStarted)
	}
	if evt.Changes == nil {
		t.Fatal("started event carries no Changes, want the after map with the grant's shape and the operator's reason")
	}
	after := evt.Changes.After
	if got, _ := after["reason"].(string); got != reason {
		t.Errorf("after[\"reason\"] = %q, want the operator's own justification %q -- the reason must arrive on the audit record, not only on the grant row", got, reason)
	}
	if got, _ := after["target_tenant_id"].(string); got != grant.TargetTenantID {
		t.Errorf("after[\"target_tenant_id\"] = %v, want %q", after["target_tenant_id"], grant.TargetTenantID)
	}
	if _, ok := after["expires_at"]; !ok {
		t.Error("after[\"expires_at\"] missing, want the grant's expiry alongside the reason")
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

// --- mandatory notification on Start -----------------------

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
	// The dispatch must run under the TARGET tenant's context (a system
	// context scoped to the target tenant), never the administrator's own
	// ambient tenant.
	tenant, ok := pkgcore.TenantFromContext(notifier.contexts[0])
	if !ok || tenant != "tenant-1" {
		t.Fatalf("dispatch tenant = %q, ok=%v, want tenant-1", tenant, ok)
	}
}

// TestImpersonationService_Start_NotificationParams_CarryNoInternalFields
// pins the dispatch's construction-site boundary: the mandatory security
// notice's Params must never carry the platform operator's free-text
// reason or the administrator's user id -- every downstream surface (the
// persistent tenant-data inbox row, the inbox API) serves Params as
// received, so the impersonated user themselves would read the operator's
// "why am I looking at this account" justification and one party's
// identity data. The dispatch therefore carries NO parameters at all: the
// type's copy is static, its declaration marks zero recipient-visible
// params (module.go's reg.Notifications.Add call), so there is nothing
// legitimate for Params to carry. And because the delivery key would
// otherwise dedupe identical notices into the previous start's row, each
// start names its own fresh OccurrenceID -- the first-class per-delivery
// marker notification provides -- so the mandatory notice still arrives
// once per start, never deduped.
func TestImpersonationService_Start_NotificationParams_CarryNoInternalFields(t *testing.T) {
	notifier := &fakeNotifier{}
	svc, _ := newTestImpersonationService(t, notifier)

	start := func(reason string) {
		t.Helper()
		if _, err := svc.Start(context.Background(), StartInput{
			AdminUserID:    "admin-1",
			TargetUserID:   "user-1",
			TargetTenantID: "tenant-1",
			Reason:         reason,
			Locale:         "zh-CN",
		}); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	}
	start("investigating suspected fraud on this account")
	start("investigating suspected fraud on this account")

	if len(notifier.dispatches) != 2 {
		t.Fatalf("got %d notification dispatches, want exactly 2", len(notifier.dispatches))
	}
	for i, d := range notifier.dispatches {
		if len(d.Params) != 0 {
			t.Fatalf("dispatch %d Params = %v, want none -- the impersonated user must never receive the operator's reason or the administrator's user id through the notification params channel", i, d.Params)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal dispatch %d: %v", i, err)
		}
		payload := string(raw)
		if strings.Contains(payload, "fraud") {
			t.Fatalf("dispatch %d payload %s embeds the operator's reason text, want the internal justification to travel nowhere the recipient can read", i, payload)
		}
		if strings.Contains(payload, "admin-1") {
			t.Fatalf("dispatch %d payload %s embeds the administrator's user id, want it absent from the recipient-visible dispatch", i, payload)
		}
	}
	// Two starts of identical content are two deliveries: each carries a
	// fresh occurrence marker, or the second simulated login would be
	// deduped into the first start's row and never announced at all.
	if notifier.dispatches[0].OccurrenceID == "" {
		t.Fatal("dispatch OccurrenceID is empty, want a fresh per-start occurrence marker so every simulated login sends its own notice")
	}
	if notifier.dispatches[0].OccurrenceID == notifier.dispatches[1].OccurrenceID {
		t.Fatalf("two starts share OccurrenceID %q, want distinct markers per start", notifier.dispatches[0].OccurrenceID)
	}
}

// TestImpersonationService_Start_NotifierFailure_RefusesStart pins the
// dispatch-or-refuse contract: a synchronous dispatch failure (the
// notification could not even be enqueued) refuses the whole Start call,
// and no grant is ever written -- a "mandatory" notification that fails
// after the grant already succeeded could never be un-sent.
func TestImpersonationService_Start_NotifierFailure_RefusesStart(t *testing.T) {
	notifier := &fakeNotifier{failWith: context.DeadlineExceeded}
	svc, _ := newTestImpersonationService(t, notifier)

	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID: "admin-1", TargetUserID: "user-1", TargetTenantID: "tenant-1",
		Reason: "support ticket #42", Locale: "zh-CN",
	})
	if !apperr.HasCode(err, ErrImpersonationNotificationUnavailable.Code) {
		t.Fatalf("Start() error = %v, want %s", err, ErrImpersonationNotificationUnavailable.Code)
	}

	active, listErr := svc.ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written when the mandatory notification could not be dispatched", active)
	}
}

func TestImpersonationService_Start_NilNotifier_StillSucceeds(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	grant := startTestGrant(t, svc)
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}
}

// --- fail-closed on invalid/expired/ended grants -----------

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
	if !apperr.HasCode(err, ErrImpersonationGrantEnded.Code) {
		t.Fatalf("second End() error = %v, want ErrImpersonationGrantEnded", err)
	}
}

func TestImpersonationService_End_UnknownID_ReportsNotFound(t *testing.T) {
	svc, _ := newTestImpersonationService(t, nil)
	_, err := svc.End(context.Background(), "does-not-exist", "admin-1")
	if !apperr.HasCode(err, ErrGrantNotFound.Code) {
		t.Fatalf("End() error = %v, want ErrGrantNotFound", err)
	}
}

// TestImpersonationService_End_ConcurrentEnd_OnlyOneSucceeds pins the
// guarded end: two concurrent End calls on the SAME grant id -- an
// operator's own DELETE racing another operator's DELETE, or racing the
// automatic permission-revocation end below -- must not both silently
// succeed and both emit an admin.impersonation.ended audit event for one
// logical end.
//
// The race is forced deterministically: both goroutines are handed their
// own copy of the SAME grant, read once via Start's own return value
// before either write happens (exactly what two callers who both read the
// row moments apart would each observe), and call the unexported endGrant
// directly so the database's own SaveGuarded conditional UPDATE -- not
// incidental goroutine scheduling -- is what decides which one lands.
func TestImpersonationService_End_ConcurrentEnd_OnlyOneSucceeds(t *testing.T) {
	svc, reg := newTestImpersonationService(t, &fakeNotifier{})
	grant := startTestGrant(t, svc)

	var recorded []audit.RecordedEvent
	reg.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	copy1 := *grant
	copy2 := *grant

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = svc.endGrant(context.Background(), &copy1, "admin-1", pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, errs[1] = svc.endGrant(context.Background(), &copy2, "admin-2", pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-2"})
	}()
	close(start)
	wg.Wait()

	succeeded, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case apperr.HasCode(err, ErrImpersonationGrantEnded.Code):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("got succeeded=%d refused=%d (errs=%v), want exactly one success and one ErrImpersonationGrantEnded refusal", succeeded, refused, errs)
	}
	if len(recorded) != 1 {
		t.Fatalf("got %d recorded admin.impersonation.ended audit events, want exactly 1 (no duplicate for the losing concurrent End)", len(recorded))
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

// TestImpersonationService_Start_UnwiredService_Refused pins the
// pre-attach refusal: an ImpersonationService reached before Module.Register's
// attach has wired its mandatory host seams must refuse Start with the
// named ErrImpersonationNotWired, never start a grant silently with the
// whole validate-and-notify pass skipped (no target-existence/locale
// resolution, no target-membership validation, no mandatory security
// notification). The only public path to such a service is
// Module.Impersonation() before Register has run, so the test drives that
// path; the second leg drives the unexported repo-only constructor
// directly, the deepest degraded shape, which stays reachable in-package
// only.
func TestImpersonationService_Start_UnwiredService_Refused(t *testing.T) {
	// Leg 1: the one public path to an unattached service --
	// Module.Impersonation() before Register has ever run.
	svc := NewModule(testutil.NewDB(t)).Impersonation()
	_, err := svc.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         "support ticket #42",
	})
	if !apperr.HasCode(err, ErrImpersonationNotWired.Code) {
		t.Fatalf("Start() error = %v, want %s", err, ErrImpersonationNotWired.Code)
	}
	active, listErr := svc.ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant row: a refused Start must not write a grant", active)
	}

	// Leg 2: the bare repo-only constructor -- reachable in-package only,
	// since the constructor is unexported.
	bare := newImpersonationService(NewImpersonationRepository(testutil.NewDB(t)))
	_, err = bare.Start(context.Background(), StartInput{
		AdminUserID:    "admin-1",
		TargetUserID:   "user-1",
		TargetTenantID: "tenant-1",
		Reason:         "support ticket #42",
	})
	if !apperr.HasCode(err, ErrImpersonationNotWired.Code) {
		t.Fatalf("Start() on a bare constructor error = %v, want %s", err, ErrImpersonationNotWired.Code)
	}
}
