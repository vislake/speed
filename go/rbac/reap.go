package rbac

import (
	"context"
	"encoding/json"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is rbac's subscriber for one event ANOTHER module publishes:
// org's org.member.removed, announced when a tenant removes a member. The
// binding rbac holds for that member -- the role grants accumulated in the
// tenant while the membership was live -- must not outlive the membership:
// an authorization row for someone who can no longer enter the tenant is
// latent access, waiting for a bug elsewhere (a resurrected session, a
// restored membership) to become real.
//
// The module boundary is the reason this is a subscription at all, and the
// shape of the code below is dictated by what the boundary permits:
//
//   - rbac never imports org. org is a peer module released on its own
//     schedule, and importing it would invert the dependency direction
//     (docs/internal/01-architecture.md). rbac knows org by two facts
//     alone: the event's string name, spelled out as the unexported
//     constant below, and a JSON-shaped payload probe
//     (memberRemovedUserIDFromPayload) that accepts the snake_case field
//     names org's payload struct carries, never org's type.
//
//   - rbac only subscribes; it never publishes this event and never
//     declares it in Register. Publishing it would make rbac a co-publisher
//     of a fact only org can know (whether the removal really committed),
//     and declaring it would widen the frozen event vocabulary with a
//     foreign event -- the event registry is this module's own surface, and
//     "org.member.removed" is org's.
//
//   - Subscribe cannot fail and an absent publisher is not an error: a host
//     without org's module simply never fires this handler, exactly as a
//     host without authn never fires org's own authn.user.created handler.
//
// The reap itself is deliberately NOT a bulk delete. Each live binding is
// revoked one at a time through Service.RevokeRole, the same soft-delete
// path an administrator's manual revoke takes: the row is mark-deleted
// (so a later restore of the membership can restore the grants), the
// process-local decision cache is invalidated, and EventRoleBindingRevoked
// is published so every replica converges -- the exact convergence path
// this module's own revoke tests pin. A bulk statement would skip all
// three.
//
// See onMemberRemoved's doc comment for the handler's resilience contract,
// which mirrors the one org's own handleUserCreated documents for the
// authn.user.created event.

// eventMemberRemoved is org's org.member.removed event, the string rbac
// subscribes to. It is unexported and not part of eventDecls on purpose:
// rbac does not publish or declare it, it only listens (see the file
// comment above). The value is the whole of rbac's compile-time knowledge
// of org's event surface, exactly the way org's own knowledge of authn's
// authn.user.created is the string alone.
const eventMemberRemoved = "org.member.removed"

// memberRemovedUserIDKeys are the field spellings rbac accepts for the
// removed user's id inside an org.member.removed payload, probed in order.
//
// Several spellings are accepted because the payload reaches rbac as data,
// not as a type: a same-process publish delivers org's MemberRemoved struct
// (whose snake_case JSON tags rbac cannot see at compile time without
// importing org), while the Redis bus delivers a map built from those same
// tags. Probing a small ordered list is what lets rbac read both shapes.
// The list mirrors the one org probes against authn.user.created payloads
// (go/org/events.go) -- the same boundary, the same technique.
var memberRemovedUserIDKeys = []string{"user_id", "userId", "UserID", "userID"}

// memberRemovedUserIDFromPayload extracts the removed user's id from an
// org.member.removed payload of any shape, by round-tripping it through
// JSON into a map and probing the accepted key spellings.
//
// It never type-asserts the publisher's concrete type -- doing so would
// require importing org, the one thing this module may not do -- and it
// returns ok=false rather than an error for every unusable shape, because
// the caller's contract is to log and continue, not to fail the publisher.
// The payload itself is never logged: it belongs to another module and may
// carry a name or an address.
func memberRemovedUserIDFromPayload(payload any) (string, bool) {
	if payload == nil {
		return "", false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", false
	}
	for _, key := range memberRemovedUserIDKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		id, ok := value.(string)
		if ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// onMemberRemoved is the subscriber Attach installs for org's
// org.member.removed event: it reaps every role binding the removed member
// still holds in the tenant the removal happened in.
//
// # The resilience contract
//
// The publisher is another module, released on its own schedule, and rbac
// has no compile-time knowledge of its payload. The cases are the ones
// org's own handleUserCreated documents for the event org subscribes to,
// mirrored here:
//
//  1. Nobody publishes the event. The subscription is installed and simply
//     never fires. Registry.Events.Subscribe returns nothing and cannot
//     fail, so an absent org module is not even observable here.
//
//  2. The payload is not a shape rbac recognizes -- a different struct, a
//     bare string, a map without a user id. Logged at Warn, and nil is
//     returned. Returning an error would be actively harmful: on the
//     in-memory bus a handler's error propagates back to the publisher's
//     own Publish call, which org's Remove makes inside its own request
//     after the membership delete has already committed -- rbac's failure
//     to understand a payload would surface there as a failed removal.
//
//  3. The event carries no tenant. There is no tenant whose bindings could
//     be reaped, so there is nothing to do. Logged at Debug and skipped.
//
//  4. The event carries a tenant. The tenant context is rebuilt from the
//     event (pkgcore.WithTenant) because a handler invoked by the
//     distributed mode's bus runs on a context that carries none, and
//     every Repository call would otherwise fail closed. Then the
//     member's live bindings are revoked one by one; see reapRoleBindings
//     for why the failures inside that loop are logged and continued
//     rather than returned.
//
// The handler never returns an error. On the in-memory bus it runs
// synchronously inside org's Remove call, after org's transaction has
// committed, so an error here would make a successful removal report
// failure to the very caller that triggered it.
func (s *Service) onMemberRemoved(ctx context.Context, evt pkgcore.Event) error {
	userID, ok := memberRemovedUserIDFromPayload(evt.Payload)
	if !ok {
		observability.FromContext(ctx).Warn("rbac ignored a member-removed event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if evt.TenantID == "" {
		observability.FromContext(ctx).Debug("rbac ignored a member-removed event with no tenant",
			"event_type", evt.Type, "user_id", userID)
		return nil
	}

	ctx = pkgcore.WithTenant(ctx, evt.TenantID)
	s.reapRoleBindings(ctx, evt, userID)
	return nil
}

// reapRoleBindings revokes every live role binding the removed member still
// holds in the tenant ctx carries -- ctx was rebuilt from the event by
// onMemberRemoved, so the subject here comes entirely from the event's
// tenant and payload, never from whatever identity the delivery context
// happened to carry (a bus subscriber context has none anyway, and a
// publisher's identity must never steer whose grants get revoked).
//
// Each binding is revoked through Service.RevokeRole with the scope the
// binding was granted at, which reuses the entire tested revoke path: the
// soft-delete mark, the local cache invalidation and the
// EventRoleBindingRevoked announcement that converges the other replicas
// are exactly what an administrator's manual revoke performs, and the
// errors it can return are the ones that path already classifies.
//
// The loop never aborts on a failure, for the same reason the handler
// never returns one: an org.member.removed delivery that reaped nine of
// ten bindings has done real work, and reporting failure on it would make
// org's committed removal look failed. Each failure is logged at Warn and
// the reap moves on; the residual bindings keep the tenant's own
// subsequent revokes (or a re-delivery, which finds nothing left to do --
// the reap is idempotent because ByUser only returns live rows) as their
// backstop. The one classified case is ErrBindingNotFound from RevokeRole,
// which means a concurrent administrator revoke already withdrew the
// binding between the enumeration above and the revoke -- exactly the
// caller's goal, so it is not even worth a log line.
func (s *Service) reapRoleBindings(ctx context.Context, evt pkgcore.Event, userID string) {
	log := observability.FromContext(ctx)
	sub := Subject{TenantID: evt.TenantID, UserID: userID}

	bindings, err := s.bindings.ByUser(ctx, userID)
	if err != nil {
		log.Warn("rbac could not enumerate a removed member's role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return
	}
	for _, binding := range bindings {
		// The binding names the role by id; RevokeRole names it by key, so
		// the id is resolved once here. Roles have no delete path in this
		// module, so a binding whose role cannot be found is an anomaly
		// worth a log line rather than a crash.
		role, err := s.roles.FindByID(ctx, binding.RoleID)
		if err != nil {
			log.Warn("rbac could not resolve a removed member's binding role",
				"event_type", evt.Type, "user_id", userID, "role_id", binding.RoleID, "error", err)
			continue
		}
		if err := s.RevokeRole(ctx, sub, role.Key, Scope{NodeID: binding.NodeID}); err != nil {
			if isBindingNotFound(err) {
				continue
			}
			log.Warn("rbac could not revoke a removed member's role binding",
				"event_type", evt.Type, "user_id", userID, "role", role.Key, "error", err)
		}
	}
}
