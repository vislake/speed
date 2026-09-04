package admin

import "github.com/vislake/speed/go/pkgcore/apperr"

// This file is admin's error catalog. Every value here is an *apperr.Error
// whose Code follows "<module>.<reason>", and every Code has a matching
// entry in BOTH locales/zh-CN.toml and locales/en-US.toml, mirroring every
// other module's identical convention (see go/authn/errors.go's own header
// for the full rationale, which applies here unchanged).
//
// Holding these as package-level sentinels is safe because *apperr.Error's
// builders derive a new value instead of mutating the receiver. Match on
// Code through apperr.As, never by pointer identity.
var (
	// ErrTenantNotFound reports that no admin_tenants row exists for the
	// requested tenant id.
	ErrTenantNotFound = apperr.NotFound("admin.tenant_not_found")

	// ErrTenantAlreadyExists reports a manual-registration attempt
	// (D3's second population path) for a tenant id the ledger already
	// carries a row for -- whether from an earlier manual registration or
	// from the event-driven lazy population path.
	ErrTenantAlreadyExists = apperr.Conflict("admin.tenant_already_exists")

	// ErrTenantIDRequired is returned when a manual tenant registration
	// names no tenant id.
	ErrTenantIDRequired = apperr.Invalid("admin.tenant_id_required")

	// ErrGrantNotFound reports that no admin_impersonation_grants row
	// exists for the requested grant id.
	ErrGrantNotFound = apperr.NotFound("admin.impersonation_grant_not_found")

	// ErrImpersonationReasonRequired reports a start-impersonation request
	// with no Reason: docs/internal/23-admin.md section 5 requires the
	// operator to state one, itself part of the audit trail.
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
	// permission in exactly that domain (D1), so a grant scoped to it would
	// let the substituted Principal ImpersonationMiddleware installs reach
	// whatever admin:* permissions the TARGET happens to hold there --
	// turning the very mechanism meant to cap an impersonating admin at
	// the target's own access (D5 property (b)) into a path for picking a
	// MORE privileged target instead. This is refused unconditionally,
	// never merely gated on a stricter permission: no round-1 permission
	// is fine-grained enough to distinguish "may impersonate an ordinary
	// business-tenant user" from "may impersonate a fellow platform
	// operator", so the only safe default is refusing the latter outright.
	ErrImpersonationTargetForbidden = apperr.Invalid("admin.impersonation_target_forbidden")

	// ErrImpersonationGrantEnded is returned by EndGrant when the grant
	// named was already ended (by an earlier DELETE, or by having expired
	// and been observed as such).
	ErrImpersonationGrantEnded = apperr.Conflict("admin.impersonation_grant_ended")

	// ErrPrincipalRequired is returned by every admin HTTP operation when
	// the request carries no verified authn.Principal -- every route this
	// module mounts sits downstream of authn.Middleware in a correctly
	// wired host (see AGENTS.md's wiring section), so this is a wiring
	// failure, not an expected runtime condition, but it must still fail
	// closed rather than invent an actor id for the audit trail.
	ErrPrincipalRequired = apperr.Unauthorized("admin.principal_required")

	// The four wiring errors below are boot-time failures Module.Register
	// returns when a mandatory host seam was never injected through the
	// matching With* option -- the same pattern org's ErrEmailIndexerRequired
	// and config's ErrCipherRequired follow. They are never returned from
	// an HTTP handler; they fail Kernel.Bootstrap itself, naming exactly
	// which option the host forgot.

	// ErrAuthnServiceRequired is returned when no *authn.Service was
	// injected with WithAuthn -- D6's cross-tenant user search has nothing
	// to search without one.
	ErrAuthnServiceRequired = apperr.Internal("admin.authn_service_required")

	// ErrOrgModuleRequired is returned when no *org.Module was injected
	// with WithOrg -- D6's membership composition and D3's root-node
	// discriminator both depend on it.
	ErrOrgModuleRequired = apperr.Internal("admin.org_module_required")

	// ErrComplianceModuleRequired is returned when no *compliance.Module
	// was injected with WithCompliance -- D7's audit query HTTP shell has
	// no read path without one.
	ErrComplianceModuleRequired = apperr.Internal("admin.compliance_module_required")

	// ErrNotificationModuleRequired is returned when no *notification.Module
	// was injected with WithNotification -- D5's mandatory
	// impersonation-started security notification has no transport
	// without one.
	ErrNotificationModuleRequired = apperr.Internal("admin.notification_module_required")
)

// errorCodes lists every code this module can return, in catalog order. It
// exists so the locale files and the catalog cannot drift apart unnoticed
// -- errors_test.go walks it against both embedded .toml files, mirroring
// go/authn/errors.go's identical errorCodes convention.
var errorCodes = []string{
	ErrTenantNotFound.Code,
	ErrTenantAlreadyExists.Code,
	ErrTenantIDRequired.Code,
	ErrGrantNotFound.Code,
	ErrImpersonationReasonRequired.Code,
	ErrImpersonationTargetRequired.Code,
	ErrImpersonationSelfNotAllowed.Code,
	ErrImpersonationTargetForbidden.Code,
	ErrImpersonationGrantEnded.Code,
	ErrPrincipalRequired.Code,
	ErrAuthnServiceRequired.Code,
	ErrOrgModuleRequired.Code,
	ErrComplianceModuleRequired.Code,
	ErrNotificationModuleRequired.Code,
	errInternal.Code,
}
