package admin

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// registerTestUser registers a real authn account through env's own
// *authn.Module, returning its id. locale is passed through verbatim
// (authn.RegisterInput.Locale, empty meaning "never chosen").
func registerTestUser(t *testing.T, env testAdminEnv, email, locale string) string {
	t.Helper()
	user, err := env.Authn.Service().Register(context.Background(), authn.RegisterInput{
		Email:       email,
		Password:    "a perfectly fine passphrase",
		DisplayName: "Locale Test Target",
		Locale:      locale,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return user.ID
}

// waitForSendRecord polls notification's own SendRecordRepository until a
// record for typeKey lands under tenant, or the deadline passes -- the
// observable proof that a REAL notification.Dispatch call actually got
// enqueued and the worker actually ran it: a Dispatch whose validate()
// refused an empty Locale never reaches the queue at all, so no send
// record -- succeeded, failed or skipped -- would ever exist for it.
func waitForSendRecord(t *testing.T, env testAdminEnv, tenant pkgcore.TenantID, typeKey string) *notification.SendRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		records, err := env.Notification.Deliveries().SendRecords().ListByFilter(context.Background(), notification.SendRecordFilter{
			TenantID: string(tenant),
			Limit:    10,
		})
		if err != nil {
			t.Fatalf("ListByFilter() error = %v", err)
		}
		for i := range records {
			if records[i].TypeKey == typeKey {
				return &records[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no send record for type %q landed under tenant %q within the deadline", typeKey, tenant)
	return nil
}

// TestImpersonationService_Start_NoLocale_ResolvesThroughAuthn_RealDispatch
// is P1-1's THE scenario: Start is driven through the REAL
// notification.DeliveryService (buildTestAdminModule's genuine wiring --
// no fakeNotifier anywhere), with StartInput.Locale left empty, against a
// real authn user who has never chosen a locale (Locale: "" at
// registration).
//
// On unfixed main this StartInput.Locale="" was forwarded straight into
// notification.Dispatch, whose own validate() refuses an empty Locale for
// a RecipientClassUser recipient -- Dispatch returns before ever touching
// the queue -- and notifyStarted's Warn-and-swallow let Start still return
// a grant (201) with the mandatory notification silently never even
// enqueued. This test would fail against that behaviour: it demands an
// actual, successfully-settled send record, which cannot exist for a
// dispatch validate() ever refused.
func TestImpersonationService_Start_NoLocale_ResolvesThroughAuthn_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-fallback")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Fallback Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "locale-fallback-target@example.com", "")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-1",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove the locale falls back to authn.DefaultLocale",
	})
	if err != nil {
		t.Fatalf("Start() error = %v, want success (the target's empty authn.User.Locale must resolve to authn.DefaultLocale)", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	rec := waitForSendRecord(t, env, tenant, NotificationTypeImpersonationStarted)
	if rec.Status != notification.SendRecordStatusSucceeded && rec.Status != notification.SendRecordStatusSkipped {
		t.Fatalf("send record status = %q, want %q or %q (a channel skip is legal -- fakeUserAddressResolver reports no email -- but the in-app channel must succeed)",
			rec.Status, notification.SendRecordStatusSucceeded, notification.SendRecordStatusSkipped)
	}
}

// TestImpersonationService_Start_ExplicitLocale_UsedVerbatim_RealDispatch
// is P1-1's second, unchanged leg -- "a request WITH a locale keeps
// working exactly as today": an explicit StartInput.Locale is trusted
// verbatim and never overridden by the target's own authn.User.Locale,
// against the same real, non-fake notification pipeline.
func TestImpersonationService_Start_ExplicitLocale_UsedVerbatim_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-explicit")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Explicit Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	// The target's OWN stored locale is en-US; the explicit request below
	// asks for zh-CN, which must be what actually gets used.
	targetID := registerTestUser(t, env, "locale-explicit-target@example.com", "en-US")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-2",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove an explicit locale is trusted verbatim",
		Locale:         "zh-CN",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	rec := waitForSendRecord(t, env, tenant, NotificationTypeImpersonationStarted)
	if rec.Status != notification.SendRecordStatusSucceeded && rec.Status != notification.SendRecordStatusSkipped {
		t.Fatalf("send record status = %q, want %q or %q", rec.Status, notification.SendRecordStatusSucceeded, notification.SendRecordStatusSkipped)
	}
}

// TestImpersonationService_Start_UnknownTarget_RefusedWithNoGrant proves
// the honest refusal contract for a target that cannot even be resolved:
// no real authn account exists for TargetUserID, so locale resolution
// itself fails with authn.ErrNotFound, Start refuses with
// ErrImpersonationTargetNotFound, and -- exactly like the SystemDomain
// and NotifierFailure refusal tests -- no grant row is ever written.
func TestImpersonationService_Start_UnknownTarget_RefusedWithNoGrant(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-unknown")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Unknown Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	_, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-3",
		TargetUserID:   "no-such-user",
		TargetTenantID: tenant,
		Reason:         "prove an unresolvable target is refused, not silently 201'd",
	})
	if !isCode(err, ErrImpersonationTargetNotFound.Code) {
		t.Fatalf("Start() error = %v, want %s", err, ErrImpersonationTargetNotFound.Code)
	}

	active, listErr := env.Admin.Impersonation().ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written for an unresolvable target", active)
	}
}

// TestImpersonationService_Start_TargetNotAMember_RefusedWithNoGrant is
// Finding P3-4's own regression test: a real, genuinely existing authn
// account that simply never joined the target tenant must be refused with
// ErrImpersonationTargetNotMember, not a 201 for a grant nobody could
// legitimately use against that tenant in the first place -- the exact gap
// the audit named ("幽灵用户 grant 照样 201", a grant for an account with no
// real standing in the tenant still succeeding).
func TestImpersonationService_Start_TargetNotAMember_RefusedWithNoGrant(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-locale-non-member")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Locale Non-Member Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	// A real account, registered successfully -- but never added as a
	// member of tenant via env.Org.Members().Add.
	targetID := registerTestUser(t, env, "locale-non-member-target@example.com", "en-US")

	_, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-locale-4",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove a real but non-member target is refused, not silently 201'd",
	})
	if !isCode(err, ErrImpersonationTargetNotMember.Code) {
		t.Fatalf("Start() error = %v, want %s", err, ErrImpersonationTargetNotMember.Code)
	}

	active, listErr := env.Admin.Impersonation().ListActive(context.Background())
	if listErr != nil {
		t.Fatalf("ListActive() error = %v", listErr)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive() = %+v, want no grant ever written for a non-member target", active)
	}
}

// TestImpersonationService_Start_AdminRoleRevoked_LiveGrantAutomaticallyEnded
// is Finding P2-3's regression test: the exact scenario the audit named --
// an administrator's admin:impersonate permission is revoked while a grant
// they started is still Active, and the audit's own claim was that
// Lookup's only checks (grant validity + AdminUserID match) never react to
// that at all, so a revoked administrator could keep impersonating for the
// rest of the grant's 30-minute TTL.
//
// Driven entirely against real, Attach()-ed rbac.Service and Bootstrap-
// wired admin modules (buildTestAdminModule, mirroring role_test.go's own
// pattern): a real DefineRole/AssignRole grants admin-impersonate-flow the
// admin:impersonate permission under rbac.SystemDomain, Start opens a real
// grant, RevokeRole withdraws it through rbac's own real revoke path (never
// a bypass), and rbac's own EventRoleBindingRevoked -- published
// synchronously on the in-process bus this test's Bootstrap wires
// everything onto -- reaches admin's subscriber before RevokeRole even
// returns, with no polling needed.
func TestImpersonationService_Start_AdminRoleRevoked_LiveGrantAutomaticallyEnded(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	roles := env.Admin.Roles()
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const adminUser = "admin-revoke-flow"
	const tenant = pkgcore.TenantID("tenant-revoke-flow")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Revoke Flow Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "revoke-flow-target@example.com", "en-US")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	role, err := roles.DefineRole(context.Background(), string(rbac.SystemDomain), rbac.RoleDefinition{
		Key:         "impersonator-flow",
		Permissions: []string{PermissionImpersonate},
	})
	if err != nil {
		t.Fatalf("DefineRole() error = %v", err)
	}
	if assignErr := roles.AssignRole(context.Background(), string(rbac.SystemDomain), adminUser, role.Key, ""); assignErr != nil {
		t.Fatalf("AssignRole() error = %v", assignErr)
	}

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    adminUser,
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove a revoked admin's live grant is ended automatically",
		Locale:         "en-US",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, ok := env.Admin.Impersonation().Lookup(context.Background(), grant.ID); !ok {
		t.Fatal("Lookup() = false immediately after Start, want the fresh grant Active")
	}

	if revokeErr := roles.RevokeRole(context.Background(), string(rbac.SystemDomain), adminUser, role.Key, ""); revokeErr != nil {
		t.Fatalf("RevokeRole() error = %v", revokeErr)
	}

	if _, ok := env.Admin.Impersonation().Lookup(context.Background(), grant.ID); ok {
		t.Fatal("Lookup() = true after the administrator's admin:impersonate permission was revoked, want the grant ended automatically")
	}

	active, err := env.Admin.Impersonation().ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive() error = %v", err)
	}
	for _, g := range active {
		if g.ID == grant.ID {
			t.Fatalf("ListActive() still includes %q after the administrator's permission was revoked", grant.ID)
		}
	}
}

// TestImpersonationService_Start_AdminKeepsPermissionViaOtherRole_GrantSurvives
// is the negative-case counterpart: revoking one of TWO roles that each
// independently grant admin:impersonate must NOT end the grant, since the
// administrator genuinely still holds the permission through the other
// role -- proving endIfNoLongerPermitted re-checks rbac.Service.Can rather
// than assuming the answer from the revoke event alone.
func TestImpersonationService_Start_AdminKeepsPermissionViaOtherRole_GrantSurvives(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	roles := env.Admin.Roles()
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const adminUser = "admin-dual-role-flow"
	const tenant = pkgcore.TenantID("tenant-dual-role-flow")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Dual Role Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "dual-role-target@example.com", "en-US")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	roleA, err := roles.DefineRole(context.Background(), string(rbac.SystemDomain), rbac.RoleDefinition{
		Key: "impersonator-a", Permissions: []string{PermissionImpersonate},
	})
	if err != nil {
		t.Fatalf("DefineRole(a) error = %v", err)
	}
	roleB, err := roles.DefineRole(context.Background(), string(rbac.SystemDomain), rbac.RoleDefinition{
		Key: "impersonator-b", Permissions: []string{PermissionImpersonate},
	})
	if err != nil {
		t.Fatalf("DefineRole(b) error = %v", err)
	}
	if assignErrA := roles.AssignRole(context.Background(), string(rbac.SystemDomain), adminUser, roleA.Key, ""); assignErrA != nil {
		t.Fatalf("AssignRole(a) error = %v", assignErrA)
	}
	if assignErrB := roles.AssignRole(context.Background(), string(rbac.SystemDomain), adminUser, roleB.Key, ""); assignErrB != nil {
		t.Fatalf("AssignRole(b) error = %v", assignErrB)
	}

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    adminUser,
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         "prove a grant survives a revoke that leaves another granting role intact",
		Locale:         "en-US",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := roles.RevokeRole(context.Background(), string(rbac.SystemDomain), adminUser, roleA.Key, ""); err != nil {
		t.Fatalf("RevokeRole(a) error = %v", err)
	}

	if _, ok := env.Admin.Impersonation().Lookup(context.Background(), grant.ID); !ok {
		t.Fatal("Lookup() = false after revoking only ONE of two granting roles, want the grant to survive (roleB still grants admin:impersonate)")
	}
}
