package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/admin"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/demoseed"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// This file is admin's counterpart to demo_subject.go and demo_users.go:
// where a platform-staff Subject comes from, which admin permission gates
// which admin sub-route, and the demo platform-staff account seeded at
// startup -- the real, first-consumer proof of go/admin's console
// surface.

// DemoPlatformStaffEmail is the real account this app registers to
// demonstrate the admin console end to end. Unlike DemoOwnerUserID and
// friends (demo_subject.go), there is no bare-header twin for this actor:
// admin's own routes read the caller's identity from the verified
// authn.Principal only (Handler's own doc comment), never from
// DemoUserHeader, so a real registered-and-signed-in account is the only
// way to reach them at all.
const DemoPlatformStaffEmail = "demo-platform-staff@example.com"

// adminRoutePath mirrors notesRoutePath's own situation: admin keeps its
// mount-point constant unexported, so this app names it again here to
// keep demoRouteGuards and GuardModuleRoute in step with it.
const adminRoutePath = "/api/v1/admin"

// The admin sub-paths adminPermissionFor tells apart. admin mounts
// its whole surface as one Handler under adminRoutePath (mountModuleRoutes
// wraps the WHOLE subtree in one guard), so distinguishing which
// permission a specific request needs is this app's own job, done by
// inspecting the request's own path and method -- never a header, a query
// parameter or a body field, for the same reason DemoPermissionFor's own
// doc comment gives.
//
// adminAuditEventsExportPath MUST be checked before adminAuditEventsPath
// in adminPermissionFor's switch: both are prefixes of
// "/api/v1/admin/audit-events/export", and the export leg is
// deliberately gated on the stronger PermissionAuditExport rather than
// falling through to the plain-read PermissionAuditRead (module.go's own
// PermissionAuditExport doc comment: exporting a tenant's complete audit
// trail is a materially stronger action than merely reading it).
const (
	adminTenantsPath                  = adminRoutePath + "/tenants"
	adminUsersPath                    = adminRoutePath + "/users"
	adminImpersonationPath            = adminRoutePath + "/impersonation"
	adminAuditEventsExportPath        = adminRoutePath + "/audit-events/export"
	adminAuditEventsPath              = adminRoutePath + "/audit-events"
	adminRolesPath                    = adminRoutePath + "/roles"
	AdminUsageSummaryPath             = adminRoutePath + "/usage-summary"
	adminNotificationsSendRecordsPath = adminRoutePath + "/notifications/send-records"
)

// adminPermissionFor chooses the admin:* permission a request against
// admin's mounted subtree must hold, from its path and method alone.
//
// This is deliberately NOT the generic DemoPermissionFor(resource)
// read/write split every other gated module route uses: admin's
// permissions are distinguished by SUB-RESOURCE (tenants, users,
// impersonation, audit-events read vs. export, roles, usage, notification
// send-records), not by one resource's read/write split, so this app's
// own router-level gate has to know the sub-path shape -- exactly the
// same reason storageResource's whole-module gate does NOT apply here.
//
// A path this function does not recognize returns "", which
// rbac.RequirePermissionFunc's own doc comment says denies the request --
// the strict, fail-closed direction: a route admin adds later that this
// table forgets to name is refused, never served ungated.
func adminPermissionFor(r *http.Request) string {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, adminTenantsPath):
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return admin.PermissionAccess
		}
		return admin.PermissionTenantsManage
	case strings.HasPrefix(path, adminUsersPath):
		return admin.PermissionSearchUsers
	case strings.HasPrefix(path, adminImpersonationPath):
		return admin.PermissionImpersonate
	case strings.HasPrefix(path, adminAuditEventsExportPath):
		// Must be checked before adminAuditEventsPath below -- see the
		// path constants' own doc comment.
		return admin.PermissionAuditExport
	case strings.HasPrefix(path, adminAuditEventsPath):
		return admin.PermissionAuditRead
	case strings.HasPrefix(path, adminRolesPath):
		return admin.PermissionRolesManage
	case strings.HasPrefix(path, AdminUsageSummaryPath):
		return admin.PermissionUsageRead
	case strings.HasPrefix(path, adminNotificationsSendRecordsPath):
		return admin.PermissionNotificationsRead
	default:
		return ""
	}
}

// adminSubjectResolver is admin's OWN subject resolver -- deliberately NOT
// DemoSubjectResolver: every admin:* permission is evaluated in
// rbac.SystemDomain, because admin's own route does NOT go through
// ordinary tenancy.Middleware tenant resolution, so TenantID here is
// HARD-CODED to rbac.SystemDomain rather than read from
// pkgcore.TenantFromContext the way DemoSubjectResolver does for every
// other module's route.
//
// Reading the ambient tenant would be wrong in both directions:
//   - An operator who ALSO belongs to an ordinary customer tenant, signed
//     into THAT tenant's session, would be evaluated as Subject{that
//     tenant, staffID} -- a false negative, since their admin:* grants
//     live only under rbac.SystemDomain.
//   - Far worse, rbac.BuiltinRoleOwner grants every permission ANY module
//     declared with no domain partitioning at all (go/rbac/builtin.go),
//     so an ordinary tenant's own Owner (a ubiquitous, unprivileged role
//     in this app's demo data) would be evaluated as Subject{their own
//     tenant, ownerID} and pass admin's gate purely because "owner"
//     happens to also carry admin:*'s permission strings in the shared
//     global catalog -- a real cross-tenant-isolation break, not a
//     hypothetical one.
//
// This also never reads DemoUserHeader (unlike DemoSubjectResolver): this
// file's own header comment already documents that admin's routes accept
// only a verified authn.Principal, never the header. And unlike
// DemoSubjectResolver, it is safe regardless of what tenancy.Middleware or
// admin.ImpersonationMiddleware may or may not have done to the request
// context, because BuildServer mounts admin's own route entirely outside
// both of them (see BuildServer's own composition comment) -- this
// resolver reads ONLY the caller's real, unsubstituted Principal.
func adminSubjectResolver(r *http.Request) (rbac.Subject, bool) {
	principal, ok := authn.PrincipalFromContext(r.Context())
	if !ok {
		return rbac.Subject{}, false
	}
	sub := rbac.Subject{TenantID: rbac.SystemDomain, UserID: principal.UserID}
	if !sub.Valid() {
		return rbac.Subject{}, false
	}
	return sub, true
}

// guardAdminRoute wraps admin's mounted subtree in rbac's permission gate,
// keyed by adminPermissionFor rather than a single resource -- the one
// mounted path this app gates differently from every other module's route,
// per adminPermissionFor's own doc comment. It uses adminSubjectResolver,
// never DemoSubjectResolver -- see that resolver's own doc comment for
// why sharing the generic one was a privilege-escalation bug.
func guardAdminRoute(az rbac.Authorizer, handler http.Handler) http.Handler {
	return rbac.RequirePermissionFunc(az, adminPermissionFor,
		rbac.WithSubjectResolver(adminSubjectResolver),
	)(handler)
}

// seedDemoPlatformStaff registers DemoPlatformStaffEmail through the
// composed handler's real register route, over the same authn/demoseed
// helper seedDemoUsers uses, grants it membership in rbac.SystemDomain ALONE
// -- never in any customer tenant, so its access token's tenant claim
// resolves unambiguously to "system" with no tenant_id request needed at
// sign-in -- ensures rbac's built-in roles exist in that tenant, and
// grants it BuiltinRoleOwner there. BuiltinRoleOwner carries every
// permission any module declared (rbac/builtin.go), admin's own
// admin:* permissions included, which is exactly the "platform
// administrator" shape: a person is a normal authn User
// holding an ordinary RoleBinding under the "system" pseudo-tenant, no
// special-cased identity model of its own.
//
// The SystemDomain membership is granted into the app's sign-in
// membership store (sign_in_memberships.go) -- the one membership that
// store holds by design, because "system" is a pseudo-tenant with no org
// tree for a row to live in -- and, unlike a customer-tenant membership,
// that grant has no database home, so this seed re-asserts it on every
// boot: an account it finds already registered is looked up by its email
// through authn.Service.SearchUsers (the same exact-email recovery
// seedDemoUsers uses, documented there) and granted afresh. Without that
// re-assertion a process restart would leave the platform-staff account
// permanently unable to sign in -- 403 authn.tenant_membership_required
// from an empty in-memory roster -- locking the operator out of admin's
// own console until the database is wiped. The registration itself goes
// through demoseed,
// exactly like seedDemoUsers' accounts: an already-registered staff
// account is discovered by the SearchUsers lookup and never POSTs the
// public register route -- the route whose per-IP budget
// repeated restart boots could exhaust under the distributed
// deployment mode's shared KVStore (demoseed's own doc comment has
// the reasoning) -- so a restart consumes no register budget at all.
//
// It returns the registered user id, or an error naming exactly what
// failed -- registration, membership or role assignment -- mirroring
// seedDemoUsers' own fail-the-boot-rather-than-half-seed discipline. The
// role half is idempotent (EnsureBuiltinRoles and AssignRole both are),
// so re-asserting on an already-seeded account changes nothing an
// operator may have revoked; the membership half cannot be revoked from
// anywhere but this seed.
//
// BuildServer calls it only when the operator set
// APP_DEMO_PLATFORM_STAFF_PASSWORD (the DemoPlatformStaffPassword bootstrap
// field) -- NEVER
// with cfg.DemoUsersPassword, the three demo accounts' own variable: the
// platform administrator must have its own credential source, per that
// field's own doc comment. It runs independently of seedDemoUsers: a
// boot seeding only the platform-staff account seeds exactly that account,
// and one seeding only the demo users seeds exactly those three.
func seedDemoPlatformStaff(ctx context.Context, handler http.Handler, memberships *signInMemberships, svc *rbac.Service, authnService *authn.Service, password string) (string, error) {
	logger := obs.FromContext(ctx)

	seeder, err := demoseed.NewSeeder(handler, authnService.SearchUsers, demoSeedDomain)
	if err != nil {
		return "", err
	}
	userID, alreadyExists, err := seeder.Register(ctx, DemoPlatformStaffEmail, password)
	if err != nil {
		return "", err
	}
	if alreadyExists {
		logger.Info("demo platform-staff account already registered; re-asserting its system-domain membership and role",
			"user_id", userID)
	}

	memberships.Grant(userID, rbac.SystemDomain)

	systemCtx := pkgcore.WithTenant(ctx, rbac.SystemDomain)
	// rbac emits an audit row for every role it defines or grants, and
	// this seed is boot-time automation with no operator session behind
	// it: the writes carry a system actor naming the seed, the same
	// attribution shape the app's own boot-time ai-gateway credential
	// write uses ("reference-app-boot" in server.go), so no role write of
	// this host is ever recorded with a blank Actor.
	seedCtx := pkgcore.WithActor(systemCtx, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: demoSeedActorID})
	if ensureErr := svc.EnsureBuiltinRoles(seedCtx); ensureErr != nil {
		return "", ensureErr
	}
	sub := rbac.Subject{TenantID: rbac.SystemDomain, UserID: userID}
	if assignErr := svc.AssignRole(seedCtx, sub, rbac.BuiltinRoleOwner, rbac.Scope{}); assignErr != nil {
		return "", assignErr
	}

	logger.Info("seeded demo platform-staff account", "user_id", userID)
	return userID, nil
}
