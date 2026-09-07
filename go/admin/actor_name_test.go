package admin

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// registerNamedUser registers a real authn account through the env's own
// *authn.Module with an explicit DisplayName, returning it -- the fixture
// shape the P2-pkgcore-actor-1 regression tests below need, since a
// display name can only be resolved for an account that actually has one.
func registerNamedUser(t *testing.T, env testAdminEnv, email, displayName string) *authn.User {
	t.Helper()
	user, err := env.Authn.Service().Register(context.Background(), authn.RegisterInput{
		Email:       email,
		Password:    "a perfectly fine passphrase",
		DisplayName: displayName,
	})
	if err != nil {
		t.Fatalf("Register(%s) error = %v", email, err)
	}
	return user
}

// findRecordedAudit scans recorder for an audit.EventRecorded event whose
// Action matches, failing the test when none is found.
func findRecordedAudit(t *testing.T, recorder *eventCapture, action string) audit.RecordedEvent {
	t.Helper()
	for _, evt := range recorder.events() {
		if evt.Type != audit.EventRecorded {
			continue
		}
		recorded, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("audit.EventRecorded payload has type %T, want audit.RecordedEvent", evt.Payload)
		}
		if recorded.Action == action {
			return recorded
		}
	}
	t.Fatalf("no audit.EventRecorded event with Action %q was published; all events = %+v", action, recorder.events())
	return audit.RecordedEvent{}
}

// eventCapture records every audit.EventRecorded published on the env's
// own registry bus after capture begins.
type eventCapture struct {
	got []pkgcore.Event
}

func captureEvents(t *testing.T, env testAdminEnv) *eventCapture {
	t.Helper()
	c := &eventCapture{}
	env.Registry.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		c.got = append(c.got, evt)
		return nil
	})
	return c
}

func (c *eventCapture) events() []pkgcore.Event { return c.got }

// TestActorName_ImpersonationEventsCarryBothDisplayNames is P2-pkgcore-actor-1's
// impersonation regression (b): an admin.impersonation.started (and the
// matching .ended) audit record must carry BOTH the impersonated target
// user's display name (Actor) and the real administrator's (OnBehalfOf) --
// an investigator reading the row must be able to say who the target was
// and who entered their account without chasing a users-table lookup, the
// exact readability pkgcore.Actor.DisplayName exists for. It drives Start
// and End through the REAL module graph (buildTestAdminModule wires a real
// *authn.Service onto ImpersonationService.attach), with both actors real
// registered accounts carrying known display names. On unfixed main both
// records carry empty display names, so this test fails there.
func TestActorName_ImpersonationEventsCarryBothDisplayNames(t *testing.T) {
	env := buildTestAdminModule(t)
	// Start dispatches the mandatory impersonation-started notification
	// onto the real queue; registering the real delivery handler keeps that
	// job from bouncing as an unknown task type, exactly as the locale
	// tests do.
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	tenant := pkgcore.TenantID("tenant-actor-name")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Actor Name Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	adminUser := registerNamedUser(t, env, "admin@actor-name.example", "Ada Admin")
	targetUser := registerNamedUser(t, env, "target@actor-name.example", "Target Person")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetUser.ID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	capture := captureEvents(t, env)

	ctx := context.Background()
	grant, err := env.Admin.Impersonation().Start(ctx, StartInput{
		AdminUserID:    adminUser.ID,
		TargetUserID:   targetUser.ID,
		TargetTenantID: tenant,
		Reason:         "prove the impersonation audit records name both identities",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	started := findRecordedAudit(t, capture, AuditActionImpersonationStarted)
	if started.Actor.Type != pkgcore.ActorTypeUser || started.Actor.ID != targetUser.ID {
		t.Fatalf("started Actor = %+v, want {Type: user, ID: %s}", started.Actor, targetUser.ID)
	}
	if started.Actor.DisplayName != "Target Person" {
		t.Errorf("started Actor.DisplayName = %q, want %q (the impersonated target's own display name)",
			started.Actor.DisplayName, "Target Person")
	}
	if started.OnBehalfOf == nil || started.OnBehalfOf.Type != pkgcore.ActorTypePlatformAdmin || started.OnBehalfOf.ID != adminUser.ID {
		t.Fatalf("started OnBehalfOf = %+v, want the real administrator", started.OnBehalfOf)
	}
	if started.OnBehalfOf.DisplayName != "Ada Admin" {
		t.Errorf("started OnBehalfOf.DisplayName = %q, want %q (the real administrator's own display name)",
			started.OnBehalfOf.DisplayName, "Ada Admin")
	}

	if _, err := env.Admin.Impersonation().End(ctx, grant.ID, adminUser.ID); err != nil {
		t.Fatalf("End() error = %v", err)
	}

	ended := findRecordedAudit(t, capture, AuditActionImpersonationEnded)
	if ended.Actor.DisplayName != "Target Person" {
		t.Errorf("ended Actor.DisplayName = %q, want %q", ended.Actor.DisplayName, "Target Person")
	}
	if ended.OnBehalfOf == nil || ended.OnBehalfOf.DisplayName != "Ada Admin" {
		t.Errorf("ended OnBehalfOf = %+v, want the ending administrator's own display name %q", ended.OnBehalfOf, "Ada Admin")
	}
}

// TestActorName_TenantStatusChangeCarriesOperatorDisplayName is the same
// finding's tenant-ledger leg: an admin.tenant.status_changed record made
// by a real platform operator must carry that operator's display name on
// its Actor, resolved from the users table at record time by
// TenantService.recordAudit (the caller's HTTP layer knows the operator
// only as a Principal user id -- see handler.go's callerUserID). On
// unfixed main the record carries no display name, so this test fails
// there.
func TestActorName_TenantStatusChangeCarriesOperatorDisplayName(t *testing.T) {
	env := buildTestAdminModule(t)
	operator := registerNamedUser(t, env, "operator@actor-name.example", "Ada Admin")

	const tenantID = "tenant-status-actor-name"
	if err := env.Admin.Tenants().Create(context.Background(), &Tenant{TenantID: tenantID, CreatedBy: operator.ID}); err != nil {
		t.Fatalf("Tenants().Create() error = %v", err)
	}

	capture := captureEvents(t, env)

	status := tenancy.TenantStatusSuspended
	if _, err := env.Admin.Tenants().SetStatus(context.Background(), tenantID, TenantPatch{Status: &status},
		pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: operator.ID}); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}

	evt := findRecordedAudit(t, capture, AuditActionTenantStatusChanged)
	if evt.Actor.Type != pkgcore.ActorTypePlatformAdmin || evt.Actor.ID != operator.ID {
		t.Fatalf("Actor = %+v, want {Type: platform_admin, ID: %s}", evt.Actor, operator.ID)
	}
	if evt.Actor.DisplayName != "Ada Admin" {
		t.Errorf("Actor.DisplayName = %q, want %q (the operator's own display name, resolved from the users table at record time)",
			evt.Actor.DisplayName, "Ada Admin")
	}
}
