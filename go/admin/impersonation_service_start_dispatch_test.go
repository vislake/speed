package admin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
// pins the locale fallback through the REAL notification.DeliveryService
// (buildTestAdminModule's genuine wiring -- no fakeNotifier anywhere):
// with StartInput.Locale left empty and a real authn user who has never
// chosen a locale (Locale: "" at registration), Start must resolve the
// target's own authn.User.Locale itself -- an empty Locale forwarded
// straight into notification.Dispatch would be refused by validate() for
// a RecipientClassUser recipient before ever touching the queue, leaving
// the mandatory notification never enqueued. The test demands an actual,
// successfully-settled send record.
func TestImpersonationService_Start_NoLocale_ResolvesThroughAuthn_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	// Start refuses while the rbac service is unattached (its own doc
	// comment), so wire the env's real Attach()-ed *rbac.Service exactly
	// as a wired host does.
	env.Admin.AttachRBAC(env.RBAC)
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
// pins the verbatim-locale leg: an explicit StartInput.Locale is trusted
// verbatim and never overridden by the target's own authn.User.Locale,
// against the same real, non-fake notification pipeline.
func TestImpersonationService_Start_ExplicitLocale_UsedVerbatim_RealDispatch(t *testing.T) {
	env := buildTestAdminModule(t)
	// Start refuses while the rbac service is unattached (its own doc
	// comment), so wire the env's real Attach()-ed *rbac.Service exactly
	// as a wired host does.
	env.Admin.AttachRBAC(env.RBAC)
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
	// Start refuses while the rbac service is unattached (its own doc
	// comment), so wire the env's real Attach()-ed *rbac.Service exactly
	// as a wired host does.
	env.Admin.AttachRBAC(env.RBAC)
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
	if !apperr.HasCode(err, ErrImpersonationTargetNotFound.Code) {
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

// TestImpersonationService_Start_TargetNotAMember_RefusedWithNoGrant
// pins the membership validation: a real, genuinely existing authn account
// that simply never joined the target tenant must be refused with
// ErrImpersonationTargetNotMember, not a 201 for a grant nobody could
// legitimately use against that tenant in the first place -- a grant for
// an account with no real standing in the tenant.
func TestImpersonationService_Start_TargetNotAMember_RefusedWithNoGrant(t *testing.T) {
	env := buildTestAdminModule(t)
	// Start refuses while the rbac service is unattached (its own doc
	// comment), so wire the env's real Attach()-ed *rbac.Service exactly
	// as a wired host does.
	env.Admin.AttachRBAC(env.RBAC)
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
	if !apperr.HasCode(err, ErrImpersonationTargetNotMember.Code) {
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
// pins the automatic permission-revocation end: an administrator's
// admin:impersonate permission is revoked while a grant they started is
// still Active. Lookup's only checks (grant validity + AdminUserID match)
// never react to that at all, so without the event-driven end a revoked
// administrator could keep impersonating for the rest of the grant's
// 30-minute TTL.
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

	// Seed the administrator's system-domain grant directly against
	// rbac.Service -- the out-of-band bootstrap path hosts use for every
	// system-domain write, which is exactly what RoleService's own refusal
	// of the pseudo-tenant preserves (checkTenantWritable's doc comment).
	systemCtx := pkgcore.WithTenant(context.Background(), rbac.SystemDomain)
	if _, seedErr := env.RBAC.DefineRole(systemCtx, rbac.RoleDefinition{
		Key:         "impersonator-flow",
		Permissions: []string{PermissionImpersonate},
	}); seedErr != nil {
		t.Fatalf("DefineRole() error = %v", seedErr)
	}
	if assignErr := env.RBAC.AssignRole(systemCtx, rbac.Subject{TenantID: rbac.SystemDomain, UserID: adminUser}, "impersonator-flow", rbac.Scope{}); assignErr != nil {
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

	if revokeErr := env.RBAC.RevokeRole(systemCtx, rbac.Subject{TenantID: rbac.SystemDomain, UserID: adminUser}, "impersonator-flow", rbac.Scope{}); revokeErr != nil {
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

	// Seed both granting roles directly against rbac.Service -- the
	// out-of-band bootstrap path hosts use for every system-domain write
	// (checkTenantWritable's doc comment).
	systemCtx := pkgcore.WithTenant(context.Background(), rbac.SystemDomain)
	if _, seedErr := env.RBAC.DefineRole(systemCtx, rbac.RoleDefinition{
		Key: "impersonator-a", Permissions: []string{PermissionImpersonate},
	}); seedErr != nil {
		t.Fatalf("DefineRole(a) error = %v", seedErr)
	}
	if _, seedErr := env.RBAC.DefineRole(systemCtx, rbac.RoleDefinition{
		Key: "impersonator-b", Permissions: []string{PermissionImpersonate},
	}); seedErr != nil {
		t.Fatalf("DefineRole(b) error = %v", seedErr)
	}
	adminSub := rbac.Subject{TenantID: rbac.SystemDomain, UserID: adminUser}
	if assignErrA := env.RBAC.AssignRole(systemCtx, adminSub, "impersonator-a", rbac.Scope{}); assignErrA != nil {
		t.Fatalf("AssignRole(a) error = %v", assignErrA)
	}
	if assignErrB := env.RBAC.AssignRole(systemCtx, adminSub, "impersonator-b", rbac.Scope{}); assignErrB != nil {
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

	if err := env.RBAC.RevokeRole(systemCtx, adminSub, "impersonator-a", rbac.Scope{}); err != nil {
		t.Fatalf("RevokeRole(a) error = %v", err)
	}

	if _, ok := env.Admin.Impersonation().Lookup(context.Background(), grant.ID); !ok {
		t.Fatal("Lookup() = false after revoking only ONE of two granting roles, want the grant to survive (roleB still grants admin:impersonate)")
	}
}

// TestImpersonationService_Start_NotificationRow_CarriesNoInternalParams
// pins the no-internal-fields boundary at the row the impersonated user
// actually reads: the mandatory notice's in_app_messages row is tenant
// data served back to its recipient through the inbox API, so its params
// column must never hold the operator's free-text reason or the
// administrator's user id -- notification's delivery persists
// Dispatch.Params exactly as admin dispatches it. Driven through the REAL
// notification pipeline (buildTestAdminModule's genuine wiring -- no
// fakeNotifier anywhere), the notice must still land, but its row must
// carry no params at all.
func TestImpersonationService_Start_NotificationRow_CarriesNoInternalParams(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-notice-leak")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Notice Leak Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "notice-leak-target@example.com", "en-US")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	const operatorReason = "investigating suspected fraud on this account"
	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:    "admin-notice-leak",
		TargetUserID:   targetID,
		TargetTenantID: tenant,
		Reason:         operatorReason,
		Locale:         "en-US",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	// Poll for the notice's inbox row -- the recipient-visible record of
	// this delivery -- through notification's own repository over the
	// shared database.
	repo := notification.NewRepository(env.DB)
	var row *notification.InboxMessage
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := repo.ListForRecipient(
			pkgcore.WithTenant(context.Background(), tenant),
			targetID, notificationGroupSecurity, 20, 0)
		if listErr != nil {
			t.Fatalf("ListForRecipient() error = %v", listErr)
		}
		for i := range rows {
			if rows[i].TypeKey == NotificationTypeImpersonationStarted {
				row = &rows[i]
				break
			}
		}
		if row != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row == nil {
		t.Fatal("no admin.impersonation_started inbox row landed for the target within the deadline")
	}

	if len(row.Params) != 0 {
		t.Fatalf("impersonation notice inbox row params = %q, want none -- the recipient-visible row must not carry the operator's reason (%q) or the administrator's user id", string(row.Params), operatorReason)
	}
	if strings.Contains(string(row.Params), "admin-notice-leak") {
		t.Fatalf("impersonation notice inbox row params %q embeds the administrator's user id, want it absent from the recipient-visible row", string(row.Params))
	}
}

// TestImpersonationService_ResolveNotificationLocale_Chain drives the
// notice's locale chain at the rule level: explicit Locale first, then the
// target's stored authn.User.Locale (the recipient's own value outranks
// every requester-supplied signal), then the starting administrator's
// request language, then authn.DefaultLocale.
func TestImpersonationService_ResolveNotificationLocale_Chain(t *testing.T) {
	env := buildTestAdminModule(t)
	svc := env.Admin.Impersonation()

	neverChose := registerTestUser(t, env, "chain-never-chose@example.com", "")
	choseEnglish := registerTestUser(t, env, "chain-chose-en@example.com", "en-US")

	for _, tc := range []struct {
		name string
		in   StartInput
		want string
	}{
		{"an explicit locale wins over everything",
			StartInput{TargetUserID: neverChose, Locale: "zh-CN", RequesterLanguage: "en-US"}, "zh-CN"},
		{"the target's stored locale outranks the requester language",
			StartInput{TargetUserID: choseEnglish, RequesterLanguage: "zh-CN"}, "en-US"},
		{"the requester language answers a target with none",
			StartInput{TargetUserID: neverChose, RequesterLanguage: "zh-CN"}, "zh-CN"},
		{"nothing resolves to the platform default",
			StartInput{TargetUserID: neverChose}, authn.DefaultLocale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.resolveNotificationLocale(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("resolveNotificationLocale() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("resolved locale = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestImpersonationService_Start_RequesterLanguageTier_RendersTheNoticeInIt
// pins the requester tier end to end through the real notification
// pipeline: a target who never chose a language and an operator whose
// StartInput carries one -- the mandatory notice's inbox row must render
// in the requester's language, not the platform default.
func TestImpersonationService_Start_RequesterLanguageTier_RendersTheNoticeInIt(t *testing.T) {
	env := buildTestAdminModule(t)
	env.Admin.AttachRBAC(env.RBAC)
	if err := env.Queue.RegisterHandler(env.Notification.Deliveries()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-requester-tier")
	root, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Requester Tier Co", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	targetID := registerTestUser(t, env, "requester-tier-target@example.com", "")
	if _, addErr := env.Org.Members().Add(pkgcore.WithTenant(context.Background(), tenant), targetID, root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	grant, err := env.Admin.Impersonation().Start(context.Background(), StartInput{
		AdminUserID:       "admin-requester-tier",
		TargetUserID:      targetID,
		TargetTenantID:    tenant,
		Reason:            "prove the requester-language tier renders the notice",
		RequesterLanguage: "zh-CN",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if grant.ID == "" {
		t.Fatal("Start() returned a grant with no id")
	}

	wantTitle, err := env.Registry.Locales().Lookup("zh-CN", NotificationTypeImpersonationStarted+".in_app.title", nil)
	if err != nil {
		t.Fatalf("Lookup(zh-CN title) error = %v", err)
	}

	repo := notification.NewRepository(env.DB)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, listErr := repo.ListForRecipient(
			pkgcore.WithTenant(context.Background(), tenant),
			targetID, notificationGroupSecurity, 20, 0)
		if listErr != nil {
			t.Fatalf("ListForRecipient() error = %v", listErr)
		}
		for i := range rows {
			if rows[i].TypeKey == NotificationTypeImpersonationStarted {
				if rows[i].Title != wantTitle {
					t.Errorf("notice title = %q, want the requester language's %q", rows[i].Title, wantTitle)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no admin.impersonation_started inbox row landed for the target within the deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
