package admin

import "github.com/vislake/speed/go/pkgcore/apperr"

// This file is admin's error catalog. Every value here is an *apperr.Error
// whose Code follows "<module>.<reason>", and every Code has a matching
// entry in both locales/zh-CN.toml and locales/en-US.toml, the convention
// every module's error catalog follows.
//
// Holding these as package-level sentinels is safe because *apperr.Error's
// builders derive a new value instead of mutating the receiver. Match on
// Code through apperr.As, never by pointer identity.
var (
	// ErrTenantNotFound reports that no admin_tenants row exists for the
	// requested tenant id.
	ErrTenantNotFound = apperr.NotFound("admin.tenant_not_found")

	// ErrTenantAlreadyExists reports a manual-registration attempt for a
	// tenant id the ledger already carries a row for -- whether from an
	// earlier manual registration or from the event-driven lazy population
	// path.
	ErrTenantAlreadyExists = apperr.Conflict("admin.tenant_already_exists")

	// ErrTenantIDRequired is returned when a manual tenant registration
	// names no tenant id.
	ErrTenantIDRequired = apperr.Invalid("admin.tenant_id_required")

	// ErrTenantStatusInvalid is returned by the tenant-ledger PATCH handler
	// when the request names a status outside the API enum's closed
	// vocabulary ("active"/"suspended"), validated through the generated
	// api.AdminTenantStatus.Valid() before anything is persisted. tenancy's
	// own gate refuses any status other than active -- including a value
	// this version does not define -- so an out-of-vocabulary string
	// persisted verbatim would silently take the tenant offline until an
	// operator noticed; refusing the write here keeps that gate from ever
	// being fed garbage.
	ErrTenantStatusInvalid = apperr.Invalid("admin.tenant_status_invalid")

	// ErrGrantNotFound reports that no admin_impersonation_grants row
	// exists for the requested grant id.
	ErrGrantNotFound = apperr.NotFound("admin.impersonation_grant_not_found")

	// ErrImpersonationReasonRequired reports a start-impersonation request
	// with no Reason: the operator must state one, the reason itself part
	// of the audit trail.
	ErrImpersonationReasonRequired = apperr.Invalid("admin.impersonation_reason_required")

	// ErrImpersonationTargetRequired reports a start-impersonation request
	// naming no target user, no target tenant, or both.
	ErrImpersonationTargetRequired = apperr.Invalid("admin.impersonation_target_required")

	// ErrImpersonationSelfNotAllowed refuses an administrator's attempt to
	// start a grant naming themselves as the target user: impersonation
	// exists to let an operator see what ANOTHER user sees, and a
	// self-targeted grant could otherwise be used to manufacture a
	// same-identity OnBehalfOf record that means nothing.
	ErrImpersonationSelfNotAllowed = apperr.Invalid("admin.impersonation_self_not_allowed")

	// ErrImpersonationTargetForbidden refuses a start-impersonation request
	// naming rbac.SystemDomain -- the platform-operations pseudo-tenant --
	// as TargetTenantID. admin's own routes evaluate every admin:*
	// permission in exactly that domain, so a grant scoped to it would let
	// the substituted Principal ImpersonationMiddleware installs reach
	// whatever admin:* permissions the TARGET happens to hold there --
	// turning the very mechanism meant to cap an impersonating admin at
	// the target's own access into a path for picking a MORE privileged
	// target instead. This is refused unconditionally, never merely gated
	// on a stricter permission: no permission in the catalog is
	// fine-grained enough to distinguish "may impersonate an ordinary
	// business-tenant user" from "may impersonate a fellow platform
	// operator", so the only safe default is refusing the latter outright.
	// It is the subject-side half of the SystemDomain boundary: the
	// granting-side half -- writing roles or bindings INTO the system
	// domain through the role-management surface -- is closed by the twin
	// ErrRolesSystemDomainForbidden.
	ErrImpersonationTargetForbidden = apperr.Invalid("admin.impersonation_target_forbidden")

	// ErrImpersonationGrantEnded is returned by EndGrant when the grant
	// named was already ended (by an earlier DELETE, or by having expired
	// and been observed as such).
	ErrImpersonationGrantEnded = apperr.Conflict("admin.impersonation_grant_ended")

	// ErrImpersonationTargetNotFound is returned by Start when the target
	// user id does not resolve to any real authn account at all -- surfaced
	// while resolving the target's own locale for the mandatory security
	// notification, since a ghost user id can never receive one. No grant
	// is ever written when this is returned.
	ErrImpersonationTargetNotFound = apperr.NotFound("admin.impersonation_target_not_found")

	// ErrImpersonationNotificationUnavailable is returned by Start when the
	// mandatory, non-unsubscribable impersonation-started security
	// notification could not be guaranteed -- the target's locale could
	// not be resolved for a reason other than "no such user", the
	// cross-tenant system-context grant could not be entered, or the
	// notification itself could not even be enqueued. Start refuses the
	// whole call rather than writing a grant behind a notification nobody
	// will ever receive.
	ErrImpersonationNotificationUnavailable = apperr.Internal("admin.impersonation_notification_unavailable")

	// ErrPrincipalRequired is returned by every admin HTTP operation when
	// the request carries no verified authn.Principal -- every route this
	// module mounts sits downstream of authn.Middleware in a correctly
	// wired host, so this is a wiring failure, not an expected runtime
	// condition, but it must still fail closed rather than invent an actor
	// id for the audit trail.
	ErrPrincipalRequired = apperr.Unauthorized("admin.principal_required")

	// The four wiring errors below are boot-time failures Module.Register
	// returns when a mandatory host seam was never injected through the
	// matching With* option. They are never returned from an HTTP handler;
	// they fail Kernel.Bootstrap itself, naming exactly which option the
	// host forgot.

	// ErrAuthnServiceRequired is returned when no *authn.Service was
	// injected with WithAuthn -- the cross-tenant user search has nothing
	// to search without one.
	ErrAuthnServiceRequired = apperr.Internal("admin.authn_service_required")

	// ErrOrgModuleRequired is returned when no *org.Module was injected
	// with WithOrg -- the search path's membership composition and the
	// subscriber's root-node discriminator both depend on it.
	ErrOrgModuleRequired = apperr.Internal("admin.org_module_required")

	// ErrComplianceModuleRequired is returned when no *compliance.Module
	// was injected with WithCompliance -- the audit-query HTTP surface has
	// no read path without one.
	ErrComplianceModuleRequired = apperr.Internal("admin.compliance_module_required")

	// ErrNotificationModuleRequired is returned when no *notification.Module
	// was injected with WithNotification -- the mandatory
	// impersonation-started security notification has no transport
	// without one.
	ErrNotificationModuleRequired = apperr.Internal("admin.notification_module_required")

	// ErrQueueRequired is returned when no jobs.Queue was injected with
	// WithQueue -- the audit-export leg
	// (POST /api/v1/admin/audit-events/export) has no way to run
	// compliance.ExportService.Export asynchronously without one.
	ErrQueueRequired = apperr.Internal("admin.queue_required")

	// ErrRBACServiceRequired is returned by every RoleService method and
	// by ImpersonationService.Start when Module.AttachRBAC has not been
	// called yet. Unlike the wiring errors above, this is NOT a
	// Register-time (Bootstrap) failure: rbac.Service does not exist
	// until the host calls rbacModule.Attach(reg), which must run
	// strictly AFTER Bootstrap returns -- a full cycle later than admin's
	// own Register runs. See role.go's own RoleService doc comment and
	// Module.AttachRBAC for the full reasoning. A request reaching the
	// role-management HTTP surface before the host has called AttachRBAC
	// gets this refusal instead of a nil-service panic; an impersonation
	// Start gets it instead of issuing a grant that could outlive its
	// administrator's admin:impersonate permission -- the automatic
	// permission-revocation end of a live grant depends on the same
	// attached service (impersonation_service.go's own rbacSvc doc
	// comment).
	ErrRBACServiceRequired = apperr.Internal("admin.rbac_service_required")

	// ErrRolesSystemDomainForbidden is returned by every RoleService
	// tenant-naming write when the request names rbac.SystemDomain -- the
	// platform-operations pseudo-tenant -- as the tenant to write into.
	// The role-management surface is gated on admin:roles_manage, and the
	// role catalog is single and global with no domain partitioning:
	// admin's own admin:* permissions live in it, so accepting the system
	// tenant as an ordinary request-body tenant would let a roles_manage-
	// only caller define a role carrying admin:impersonate (or any other
	// admin:*) inside the system domain and bind it to themselves --
	// collapsing the admin permission boundaries into one. SystemDomain's
	// role catalog and bindings are the platform's internal domain,
	// administered by hosts out of band, directly against rbac.Service
	// under a system-tenant context (the shape the reference app's
	// seedDemoPlatformStaff takes), never through this surface. This is
	// refused unconditionally, never merely gated on a stricter
	// permission: no admin permission is fine-grained enough to distinguish
	// "may manage a customer tenant's roles" from "may delegate
	// platform-operator authority", so the only safe default is refusing
	// SystemDomain outright. It is the granting-side twin of
	// ErrImpersonationTargetForbidden's subject-side refusal: a grant may
	// neither be SCOPED to the system domain as an impersonation target
	// nor WRITTEN into it through the role surface.
	ErrRolesSystemDomainForbidden = apperr.Invalid("admin.roles_system_domain_forbidden")

	// ErrUsageModulesNotWired is returned by UsageService.Summary when
	// NEITHER go/metering nor go/billing was ever wired through
	// WithMetering/WithBilling. A dashboard with nothing at all to stitch
	// is a wiring gap, not a partial answer -- when only one of the two
	// is wired, Summary instead answers with that one dimension present
	// and the other absent from every row, never refusing outright.
	ErrUsageModulesNotWired = apperr.Internal("admin.usage_modules_not_wired")

	// ErrExportOperatorRequired is returned by ExportService.Enqueue when
	// called with no operator user id: every admin write path attributes
	// the calling operator, and an export enqueued with nothing to
	// attribute it to would leave "who exported this tenant's audit
	// trail" permanently unanswerable. The HTTP handler always supplies
	// one (callerUserID, exactly like every other admin write path); this
	// is reachable only through a direct, non-HTTP caller of Enqueue.
	ErrExportOperatorRequired = apperr.Invalid("admin.export_operator_required")

	// ErrRequestBodyInvalid is returned by every handler.go operation whose
	// request body failed to json.Decode -- malformed JSON or a field of
	// the wrong wire type, nothing to do with which fields the decoded
	// value then carries. This code is used ONLY at the decode step
	// itself; every site's own SEPARATE, correct field-validation refusal
	// (an actually-missing tenant id or target AFTER a successful decode)
	// is a different error.
	ErrRequestBodyInvalid = apperr.Invalid("admin.request_body_invalid")

	// ErrImpersonationTargetNotMember is returned by
	// ImpersonationService.Start when the target user id resolves to a
	// real authn account but that account holds no membership in the
	// target tenant at all -- a grant scoped to a tenant the target does
	// not even belong to would substitute an identity that could never
	// legitimately act there in the first place.
	ErrImpersonationTargetNotMember = apperr.Invalid("admin.impersonation_target_not_member")

	// ErrImpersonationTargetValidationUnavailable is returned by Start
	// when the target's existence or tenant membership could not be
	// determined for a reason OTHER than "does not exist" or "not a
	// member" -- the cross-tenant system-context grant needed to check
	// membership could not be entered, or the membership lookup itself
	// failed. Start refuses rather than writing a grant no one has
	// actually verified the target may even use.
	ErrImpersonationTargetValidationUnavailable = apperr.Internal("admin.impersonation_target_validation_unavailable")

	// ErrImpersonationNotWired is returned by Start when the
	// ImpersonationService was never attached by Module.Register's attach
	// call -- the state a service is in before Register runs, reachable
	// through Module.Impersonation() before Bootstrap. A service in that
	// state cannot run the mandatory validate-and-notify pass (target
	// existence and locale resolution, tenant-membership validation, and
	// the security notification), so Start refuses outright rather than
	// writing a grant no one has validated or notified; attach always
	// runs during Register with the full mandatory seam set, so a
	// correctly wired host never sees this.
	ErrImpersonationNotWired = apperr.Internal("admin.impersonation_not_wired")

	// ErrTenantConcurrentUpdate is returned by TenantRepository.Update
	// when the conditional UPDATE's guard finds the row no longer matches
	// what was read moments earlier -- a concurrent PATCH (suspend racing
	// resume, say) already landed in between. The caller's own patch is
	// refused rather than silently overwriting the concurrent write with
	// a stale read; a retry re-reads the current row and reapplies its
	// intent against it.
	ErrTenantConcurrentUpdate = apperr.Conflict("admin.tenant_concurrent_update")
)

// errorCodes lists every code this module can return, in catalog order. It
// exists so the locale files and the catalog cannot drift apart unnoticed
// -- errors_test.go walks it against both embedded .toml files.
var errorCodes = []string{
	ErrTenantNotFound.Code,
	ErrTenantAlreadyExists.Code,
	ErrTenantIDRequired.Code,
	ErrTenantStatusInvalid.Code,
	ErrGrantNotFound.Code,
	ErrImpersonationReasonRequired.Code,
	ErrImpersonationTargetRequired.Code,
	ErrImpersonationSelfNotAllowed.Code,
	ErrImpersonationTargetForbidden.Code,
	ErrImpersonationGrantEnded.Code,
	ErrImpersonationTargetNotFound.Code,
	ErrImpersonationNotificationUnavailable.Code,
	ErrPrincipalRequired.Code,
	ErrAuthnServiceRequired.Code,
	ErrOrgModuleRequired.Code,
	ErrComplianceModuleRequired.Code,
	ErrNotificationModuleRequired.Code,
	ErrQueueRequired.Code,
	ErrRBACServiceRequired.Code,
	ErrRolesSystemDomainForbidden.Code,
	ErrUsageModulesNotWired.Code,
	ErrExportOperatorRequired.Code,
	ErrRequestBodyInvalid.Code,
	ErrImpersonationTargetNotMember.Code,
	ErrImpersonationTargetValidationUnavailable.Code,
	ErrImpersonationNotWired.Code,
	ErrTenantConcurrentUpdate.Code,
	errInternal.Code,
}
