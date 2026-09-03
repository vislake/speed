package rbac

import "github.com/vislake/speed/go/pkgcore/apperr"

// The error index of the rbac module. Every exported error is an
// *apperr.Error builder: match a decorated error with apperr.As(err) and
// compare its Code, the convention dbkit, tenancy and config all document.
// A call that returns an error decorated with WithParam or WithCause
// derives a NEW *apperr.Error, so never compare a once-returned error
// against a var here with == or errors.Is.
//
// Every code is <module>.<reason> per backend coding standard §6.2, and
// every one has a matching message id in locales/{zh-CN,en-US}.toml. The
// API never returns the localized prose: it returns the code plus
// parameters and the client resolves the text, which is why the locale
// files exist beside these declarations rather than as strings in here.
var (
	// ErrUnknownPermission reports an attempt to grant a resource:action
	// string that no module declared through pkgcore's
	// PermissionRegistrar. It is deliberately a GRANT-time error only: a
	// permission CHECK for an unknown permission denies quietly instead,
	// because a check that errors would turn a typo into a failed request
	// rather than a refused one -- and an error is far easier to
	// accidentally treat as "allow" than a plain false.
	ErrUnknownPermission = apperr.Invalid("rbac.unknown_permission")

	// ErrRoleNotFound reports a role key or id that does not exist in the
	// caller's tenant. Like dbkit.ErrRecordNotFound it never distinguishes
	// "no such role" from "that role belongs to another tenant": telling
	// the two apart would let a caller enumerate another tenant's roles.
	ErrRoleNotFound = apperr.NotFound("rbac.role_not_found")

	// ErrDuplicateRole reports defining a role whose key already exists in
	// the tenant. The storage layer enforces the same invariant through
	// uq_rbac_roles_tenant_key, so this error is the readable form of a
	// constraint that holds regardless.
	ErrDuplicateRole = apperr.Conflict("rbac.duplicate_role")

	// ErrBindingNotFound reports a role binding that does not exist in the
	// caller's tenant, with the same deliberate ambiguity between "absent"
	// and "another tenant's" that ErrRoleNotFound carries.
	ErrBindingNotFound = apperr.NotFound("rbac.binding_not_found")

	// ErrSubjectRequired reports a call that needs a Subject and did not
	// get a usable one -- an absent subject, or one missing its tenant or
	// its user (Subject.Valid). Authorization has no anonymous mode: a
	// request that reaches this module without an identity is refused, not
	// evaluated with an empty one.
	ErrSubjectRequired = apperr.Invalid("rbac.subject_required")

	// ErrPermissionDenied reports that a subject does not hold the
	// permission being required. It is the code the HTTP middleware puts
	// in its 403 body; the boolean-returning evaluation API reports the
	// same fact as a plain false, since deny is the normal answer to a
	// question, not an exceptional one.
	ErrPermissionDenied = apperr.Forbidden("rbac.permission_denied")

	// ErrSubtreeUnresolved reports that a node-scoped binding could not be
	// placed in the organization tree: no SubtreeResolver was wired, or
	// the wired one does not know the node. The binding is DENIED in that
	// case, never widened to the whole tenant -- an unresolvable narrowing
	// must never fail open into the broader grant it was meant to
	// restrict.
	ErrSubtreeUnresolved = apperr.Forbidden("rbac.subtree_unresolved")

	// ErrServiceNotAttached reports a call through a Service whose Module
	// has not been Attached to a Registry yet (or whose Attach failed).
	// It can only surface in the window between Register and Attach, which
	// is a host wiring bug rather than a runtime condition.
	ErrServiceNotAttached = apperr.Internal("rbac.service_not_attached")

	// ErrAlreadyAttached reports a second Attach call on one Module. The
	// permission catalog freezes at the first Attach; a second call would
	// re-read a registry whose registrars may have grown, silently serving
	// a different catalog than the first snapshot, so it fails instead.
	ErrAlreadyAttached = apperr.Internal("rbac.already_attached")

	// ErrStorage reports a failure to read or write one of this module's
	// three tables. It wraps the underlying error as its cause; the cause
	// chain never reaches an HTTP response, so no SQL fragment or internal
	// identifier leaks outward (backend coding standard §6.2).
	ErrStorage = apperr.Internal("rbac.storage_error")
)

// hasCode reports whether err is an *apperr.Error carrying exactly code.
//
// Classification goes through the CODE rather than errors.Is against the
// sentinel value, because every WithParam/WithCause call returns a new
// *apperr.Error: the sentinels above are templates, not singletons, so
// errors.Is(err, ErrRoleNotFound) is false for the decorated error a
// repository actually returns. Consumers outside this package classify the
// same way, with apperr.As and a Code comparison.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
