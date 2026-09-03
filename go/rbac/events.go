package rbac

import "github.com/vislake/speed/go/pkgcore"

// The domain events rbac publishes, named <module>.<entity>.<action> per
// backend coding standard §8.
//
// They exist for two distinct consumers, which is why they are declared
// even before anything subscribes to them: an audit trail (who was granted
// what, and when), and this module's own replicas -- in the distributed
// deployment mode several processes each hold a process-local
// authorization cache, and a grant changed on one of them has to invalidate
// the others. A stale authorization cache is a security failure, not a
// performance one, which is why the invalidation path is an event on the
// shared bus rather than a per-process timer alone.
const (
	// EventRoleBindingAssigned is published when a user is granted a role,
	// tenant-wide or over one subtree.
	EventRoleBindingAssigned = "rbac.role_binding.assigned"

	// EventRoleBindingRevoked is published when such a grant is withdrawn.
	EventRoleBindingRevoked = "rbac.role_binding.revoked"

	// EventRoleChanged is published when a role's own definition changes --
	// it is created, or the permissions it grants are edited. Every user
	// bound to that role is affected, so subscribers invalidate by role
	// rather than by user.
	EventRoleChanged = "rbac.role.changed"
)

const (
	// eventRoleBindingPayloadType names the concrete payload both binding
	// events carry, for EventDecl.PayloadType, so a subscriber (and the
	// future event catalog) knows what to expect in Event.Payload without
	// importing this package just to read a string. The two events share
	// one payload shape because they describe the same fact in opposite
	// directions.
	eventRoleBindingPayloadType = "rbac.RoleBindingChangedPayload"

	// eventRoleChangedPayloadType names EventRoleChanged's payload.
	eventRoleChangedPayloadType = "rbac.RoleChangedPayload"
)

// The audit actions rbac contributes to the platform-wide enumeration.
// They use the present-tense verb form docs/internal/10-compliance-and-
// audit.md's own examples use ("org.member.remove"), deliberately distinct
// from the past-tense fact the events above record: the audit trail says
// what operation someone performed, the event says what became true as a
// result.
const (
	// AuditActionRoleDefine records defining or editing a role.
	AuditActionRoleDefine = "rbac.role.define"

	// AuditActionRoleAssign records granting a role to a user.
	AuditActionRoleAssign = "rbac.role.assign"

	// AuditActionRoleRevoke records withdrawing such a grant.
	AuditActionRoleRevoke = "rbac.role.revoke"
)

// eventDecls is what Register declares to pkgcore's EventRegistrar. It is
// a package-level var rather than a literal inside Register so the module's
// tests can assert the declared set without re-typing it.
var eventDecls = []pkgcore.EventDecl{
	{
		Type:        EventRoleBindingAssigned,
		PayloadType: eventRoleBindingPayloadType,
		Description: "Published when a user is granted a role, tenant-wide or over one organization subtree.",
	},
	{
		Type:        EventRoleBindingRevoked,
		PayloadType: eventRoleBindingPayloadType,
		Description: "Published when a user's role grant is withdrawn.",
	},
	{
		Type:        EventRoleChanged,
		PayloadType: eventRoleChangedPayloadType,
		Description: "Published when a role is defined or the permissions it grants change.",
	},
}

// auditActions is what Register declares to pkgcore's
// AuditActionRegistrar, kept beside eventDecls for the same reason.
var auditActions = []string{
	AuditActionRoleDefine,
	AuditActionRoleAssign,
	AuditActionRoleRevoke,
}
