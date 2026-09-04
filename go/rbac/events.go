package rbac

import (
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

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

	// EventRoleBindingRestored is published when a previously withdrawn
	// grant is restored through Service.RestoreRole, undoing exactly the
	// mark-delete EventRoleBindingRevoked announced. It exists so that a
	// restore on one replica converges the others through the bus exactly
	// like a revoke already does: without it, a replica other than the one
	// that called RestoreRole would keep answering from the cache entry
	// EventRoleBindingRevoked invalidated, silently serving a stale
	// "revoked" decision until its TTL expired.
	EventRoleBindingRestored = "rbac.role_binding.restored"

	// EventRoleChanged is published when a role's own definition changes --
	// it is created, or the permissions it grants are edited. Every user
	// bound to that role is affected, so subscribers invalidate by role
	// rather than by user.
	EventRoleChanged = "rbac.role.changed"
)

const (
	// eventRoleBindingPayloadType names the concrete payload all three
	// binding events carry, for EventDecl.PayloadType, so a subscriber (and
	// the future event catalog) knows what to expect in Event.Payload
	// without importing this package just to read a string. The three
	// events share one payload shape because they describe the same fact
	// (this user, this role, this scope) in different directions --
	// granted, withdrawn, or a withdrawal undone.
	eventRoleBindingPayloadType = "rbac.RoleBindingChangedEvent"

	// eventRoleChangedPayloadType names EventRoleChanged's payload.
	eventRoleChangedPayloadType = "rbac.RoleChangedEvent"
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
		Type:        EventRoleBindingRestored,
		PayloadType: eventRoleBindingPayloadType,
		Description: "Published when a previously withdrawn role grant is restored.",
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

// RoleBindingChangedEvent is the payload of both
// EventRoleBindingAssigned and EventRoleBindingRevoked -- the shape
// eventRoleBindingPayloadType names. One payload type serves both because
// the fact carried is identical (this user, this role, this scope); only
// the direction differs, and that is what the event Type says.
//
// Its first job is cache invalidation: TenantID and UserID together
// address exactly the entry every replica must drop. Its second is the
// audit trail a future compliance consumer builds by subscribing, which is
// why the role KEY travels alongside the role id -- an audit record naming
// only a UUID is unreadable once the role is gone.
type RoleBindingChangedEvent struct {
	// TenantID owns the binding. It is the tenant the grant applies in,
	// never the acting administrator's.
	TenantID string

	// UserID is the subject whose grants changed.
	UserID string

	// RoleID is the role's primary key.
	RoleID string

	// RoleKey is the role's tenant-unique key ("owner", "admin", ...),
	// carried so a subscriber need not resolve RoleID against a row that
	// may since have been deleted.
	RoleKey string

	// NodeID is the organization node the grant is scoped to, empty for a
	// tenant-wide grant.
	NodeID string

	// ActorUserID is the user id of the subject that performed the change,
	// taken from ctx through SubjectFromContext when the host installed
	// one, and empty otherwise.
	//
	// It is BEST-EFFORT on purpose. rbac takes no actor parameter (see
	// Authorizer.AssignRole) and cannot invent one, and no audit-record
	// persistence layer exists yet to demand it. When impersonation lands,
	// this single field is not enough -- an impersonated action must record
	// both the impersonated user and the real administrator -- which is
	// tracked as this module's deferral D10 rather than papered over with a
	// field that would be silently wrong.
	ActorUserID string

	// ChangedAt is when the change was written.
	ChangedAt time.Time
}

// RoleChangedEvent is the payload of EventRoleChanged -- the shape
// eventRoleChangedPayloadType names -- published when a role is defined or
// the permission set it carries is reconciled.
//
// It carries no user id because a role change has no single subject: every
// binding to that role is affected, so the invalidation it triggers is
// tenant-wide (see grantCache.invalidateTenant).
type RoleChangedEvent struct {
	// TenantID owns the role.
	TenantID string

	// RoleID is the role's primary key.
	RoleID string

	// RoleKey is the role's tenant-unique key.
	RoleKey string

	// Permissions is the permission set the role now carries, sorted. It
	// makes the event self-contained for an audit consumer; the evaluator
	// itself re-reads from the database rather than trusting it, because a
	// payload that crossed a replica boundary is not the source of truth.
	Permissions []string

	// ActorUserID is the acting subject's user id when ctx carried one.
	// Best-effort for the same reason RoleBindingChangedEvent.ActorUserID
	// is.
	ActorUserID string

	// ChangedAt is when the change was written.
	ChangedAt time.Time
}

// roleBindingChangedFromWire recovers a RoleBindingChangedEvent from
// whatever the EventBus delivered.
//
// Two shapes must both work, and this is not a nicety. The standalone
// mode's in-memory bus passes the concrete struct through untouched, but
// pkgcore's distributed bus serializes the payload as JSON and hands the
// subscriber back a plain map[string]any keyed by the struct's Go field
// names. Cache invalidation across replicas is the whole point of
// publishing these events, so an event that crossed a replica boundary
// must decode here or the distributed deployment mode would silently fall
// back to TTL expiry alone -- a revoke taking a full TTL to take effect on
// every replica but the one that wrote it.
//
// A payload of neither shape is not this event; the caller drops it rather
// than failing the handler chain.
func roleBindingChangedFromWire(payload any) (RoleBindingChangedEvent, bool) {
	switch p := payload.(type) {
	case RoleBindingChangedEvent:
		return p, true
	case map[string]any:
		return roleBindingChangedFromJSONMap(p)
	default:
		return RoleBindingChangedEvent{}, false
	}
}

// roleBindingChangedFromJSONMap reads the wire-shaped map a remote
// delivery decodes to. TenantID and UserID are mandatory -- without both,
// the event cannot address a cache entry, which is the only thing the
// subscriber does with it. Every other field degrades to its zero value:
// invalidation does not depend on any of them, and dropping the event over
// an unparseable timestamp would trade a cosmetic problem for a security
// one.
func roleBindingChangedFromJSONMap(m map[string]any) (RoleBindingChangedEvent, bool) {
	tenantID, ok := m["TenantID"].(string)
	if !ok || tenantID == "" {
		return RoleBindingChangedEvent{}, false
	}
	userID, ok := m["UserID"].(string)
	if !ok || userID == "" {
		return RoleBindingChangedEvent{}, false
	}
	evt := RoleBindingChangedEvent{
		TenantID:    tenantID,
		UserID:      userID,
		RoleID:      stringOf(m["RoleID"]),
		RoleKey:     stringOf(m["RoleKey"]),
		NodeID:      stringOf(m["NodeID"]),
		ActorUserID: stringOf(m["ActorUserID"]),
	}
	if raw, ok := m["ChangedAt"].(string); ok {
		evt.ChangedAt, _ = time.Parse(time.RFC3339Nano, raw)
	}
	return evt, true
}

// roleChangedFromWire is roleBindingChangedFromWire's counterpart for
// EventRoleChanged; see that function for why both shapes are accepted.
func roleChangedFromWire(payload any) (RoleChangedEvent, bool) {
	switch p := payload.(type) {
	case RoleChangedEvent:
		return p, true
	case map[string]any:
		return roleChangedFromJSONMap(p)
	default:
		return RoleChangedEvent{}, false
	}
}

// roleChangedFromJSONMap reads the wire-shaped map. Only TenantID is
// mandatory: a role change invalidates the whole tenant, so the tenant is
// the only field the subscriber needs. Permissions arrives as a JSON array
// of strings, so it is rebuilt element by element rather than type-asserted
// to []string, which would never succeed against []any.
func roleChangedFromJSONMap(m map[string]any) (RoleChangedEvent, bool) {
	tenantID, ok := m["TenantID"].(string)
	if !ok || tenantID == "" {
		return RoleChangedEvent{}, false
	}
	evt := RoleChangedEvent{
		TenantID:    tenantID,
		RoleID:      stringOf(m["RoleID"]),
		RoleKey:     stringOf(m["RoleKey"]),
		ActorUserID: stringOf(m["ActorUserID"]),
	}
	if raw, ok := m["Permissions"].([]any); ok {
		perms := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				perms = append(perms, s)
			}
		}
		evt.Permissions = perms
	}
	if raw, ok := m["ChangedAt"].(string); ok {
		evt.ChangedAt, _ = time.Parse(time.RFC3339Nano, raw)
	}
	return evt, true
}

// stringOf returns the string a wire map held under a key, or "" when the
// key was absent or held something else. It keeps the optional-field reads
// above one line each.
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
