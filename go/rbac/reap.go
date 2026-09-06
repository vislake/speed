package rbac

import (
	"context"
	"encoding/json"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is rbac's subscriber for TWO events ANOTHER module publishes:
// org's org.member.removed, announced when a tenant removes a member, and
// org's org.node.deleted, announced when org removes a node (or, for a
// cascade, a whole subtree) from a tenant's organization tree. Both reap
// the same hazard from a different angle: a role binding that survives the
// fact it was scoped to is latent access, waiting for a bug elsewhere (a
// resurrected session, a restored membership, a node id that gets reused)
// to become real. org.member.removed closes that hazard for a binding
// scoped to a user who left the tenant; org.node.deleted closes it for a
// binding scoped to a node the tree no longer has -- org-rbac.md's P1-2
// finding: rbac never consults org's tree at decision time (only a
// host-injected SubtreeResolver does, and only when one is wired), and
// before this round nothing reaped a binding left dangling on a deleted
// node at all. org's own TreeService.Delete already refuses to delete a
// subtree that still has members bound to it (memberGuardFor, go/org's
// tree.go) -- that refusal is real and stays exactly as it is -- but a
// node can carry an rbac binding with no org member ever having existed
// there, or a member removed independently of any binding cleanup, and
// deleting THAT node sails straight through org's own guard. This file's
// onNodeDeleted is the separate, additional safety net for exactly that
// case, never a replacement for org's own member-refusal.
//
// The module boundary is the reason this is a subscription at all, and the
// shape of the code below is dictated by what the boundary permits:
//
//   - rbac never imports org. org is a peer module released on its own
//     schedule, and importing it would invert the dependency direction
//     (docs/internal/01-architecture.md). rbac knows org by two facts per
//     event alone: the event's string name, spelled out as the unexported
//     constants below, and a JSON-shaped payload probe
//     (memberRemovedUserIDFromPayload, nodeDeletedIDsFromPayload) that
//     accepts the snake_case field names org's payload structs carry,
//     never org's types.
//
//   - rbac only subscribes; it never publishes either event and never
//     declares either in Register. Publishing one would make rbac a
//     co-publisher of a fact only org can know (whether the removal or the
//     delete really committed), and declaring one would widen the frozen
//     event vocabulary with a foreign event -- the event registry is this
//     module's own surface, and "org.member.removed"/"org.node.deleted"
//     are org's.
//
//   - Subscribe cannot fail and an absent publisher is not an error: a host
//     without org's module simply never fires either handler, exactly as a
//     host without authn never fires org's own authn.user.created handler.
//
// Both reaps are deliberately NOT a bulk delete. Each live binding is
// revoked one at a time through Service.RevokeRole, the same soft-delete
// path an administrator's manual revoke takes: the row is mark-deleted
// (so a later restore of the membership -- or, for onNodeDeleted, a later
// grant reassigned onto a reused node id -- can restore the grant), the
// process-local decision cache is invalidated, and EventRoleBindingRevoked
// is published so every replica converges -- the exact convergence path
// this module's own revoke tests pin. A bulk statement would skip all
// three.
//
// See onMemberRemoved's and onNodeDeleted's own doc comments for each
// handler's resilience contract, both mirroring the one org's own
// handleUserCreated documents for the authn.user.created event.

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

// eventNodeDeleted is org's org.node.deleted event, the string rbac
// subscribes to for the dangling-binding reap. It is unexported and not
// part of eventDecls for the identical reason eventMemberRemoved is not:
// rbac does not publish or declare it, it only listens.
const eventNodeDeleted = "org.node.deleted"

// nodeDeletedIDsKeys are the field spellings rbac accepts for the deleted
// node id set inside an org.node.deleted payload, probed in order.
//
// The list mirrors memberRemovedUserIDKeys's own shape one level up: a
// same-process publish delivers org's NodeDeleted struct (whose
// snake_case JSON tag, deleted_node_ids, rbac cannot see at compile time
// without importing org), while the Redis bus delivers a map built from
// that same tag. org's own field is spelled DeletedNodeIds (not the more
// idiomatic DeletedNodeIDs -- see go/org/events.go's own doc comment on
// NodeDeleted for why), so the untagged-struct spelling probed here
// matches that exactly.
var nodeDeletedIDsKeys = []string{"deleted_node_ids", "deletedNodeIds", "DeletedNodeIds"}

// nodeDeletedIDsFromPayload extracts the deleted node id set from an
// org.node.deleted payload of any shape, by round-tripping it through
// JSON into a map and probing the accepted key spellings.
//
// It never type-asserts the publisher's concrete type -- doing so would
// require importing org, the one thing this module may not do -- and it
// returns ok=false rather than an error for every unusable shape or an
// empty id list, because the caller's contract is to log and continue,
// not to fail the publisher. The payload itself is never logged: it
// belongs to another module.
func nodeDeletedIDsFromPayload(payload any) ([]string, bool) {
	if payload == nil {
		return nil, false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, false
	}
	for _, key := range nodeDeletedIDsKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		rawList, ok := value.([]any)
		if !ok {
			continue
		}
		ids := make([]string, 0, len(rawList))
		for _, raw := range rawList {
			id, ok := raw.(string)
			if ok && id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			return ids, true
		}
	}
	return nil, false
}

// onNodeDeleted is the subscriber Attach installs for org's org.node.deleted
// event: it reaps every role binding scoped to a node org just deleted (or,
// for a cascade, to any node the deleted subtree contained).
//
// # The resilience contract
//
// The publisher is another module, released on its own schedule, and rbac
// has no compile-time knowledge of its payload. The cases mirror
// onMemberRemoved's own, one level up (node ids in place of a user id):
//
//  1. Nobody publishes the event. The subscription is installed and simply
//     never fires.
//
//  2. The payload is not a shape rbac recognizes, or carries an empty id
//     list. Logged at Warn, and nil is returned -- returning an error would
//     surface here as a failed delete on org's side, exactly as
//     onMemberRemoved's own doc comment explains for its event.
//
//  3. The event carries no tenant. There is no tenant whose bindings could
//     be reaped, so there is nothing to do. Logged at Debug and skipped.
//
//  4. The event carries a tenant and a non-empty id set. The tenant
//     context is rebuilt from the event for the same reason
//     onMemberRemoved's does, and every binding scoped to any of the
//     deleted ids is revoked; see reapRoleBindingsForNodes for why
//     failures inside that loop are logged and continued rather than
//     returned.
//
// The handler never returns an error, for the same reason onMemberRemoved's
// does not: on the in-memory bus it runs synchronously inside org's Delete
// call, after org's own transaction has committed, so an error here would
// make a successful delete report failure to the very caller that
// triggered it.
func (s *Service) onNodeDeleted(ctx context.Context, evt pkgcore.Event) error {
	nodeIDs, ok := nodeDeletedIDsFromPayload(evt.Payload)
	if !ok {
		observability.FromContext(ctx).Warn("rbac ignored a node-deleted event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if evt.TenantID == "" {
		observability.FromContext(ctx).Debug("rbac ignored a node-deleted event with no tenant",
			"event_type", evt.Type)
		return nil
	}

	ctx = pkgcore.WithTenant(ctx, evt.TenantID)
	s.reapRoleBindingsForNodes(ctx, evt, nodeIDs)
	return nil
}

// reapRoleBindingsForNodes revokes every live role binding scoped to one of
// nodeIDs, in the tenant ctx carries -- ctx was rebuilt from the event by
// onNodeDeleted, so the tenant comes entirely from the event, never from
// whatever identity the delivery context happened to carry.
//
// Unlike reapRoleBindings, which enumerates by user, this enumerates by
// node: a node-deleted event carries no user at all, only the set of node
// ids org just removed from its tree, and every binding scoped to any of
// them is what this reap targets regardless of who holds it. Every one is
// revoked through the identical RevokeRole path reapRoleBindings already
// reuses (see this file's own header comment), in one pass over every id
// the event named -- a single event for a cascade can carry many ids, and
// this runs one enumeration and one revoke loop over all of them, never
// one handler invocation per node.
//
// The loop never aborts on a failure, for the identical reason
// reapRoleBindings' own doc comment gives: a node-deleted delivery that
// reaped some bindings and not others has done real work, and every
// failure is logged at Warn rather than surfaced. ErrBindingNotFound (a
// concurrent administrator revoke, or a re-delivery that finds nothing
// left -- the reap is idempotent because ByNodes only returns live rows)
// is not even worth a log line, mirroring reapRoleBindings' own
// classification of the identical case.
func (s *Service) reapRoleBindingsForNodes(ctx context.Context, evt pkgcore.Event, nodeIDs []string) {
	log := observability.FromContext(ctx)

	bindings, err := s.bindings.ByNodes(ctx, nodeIDs)
	if err != nil {
		log.Warn("rbac could not enumerate bindings scoped to deleted nodes",
			"event_type", evt.Type, "error", err)
		return
	}
	for _, binding := range bindings {
		// The binding names the role by id; RevokeRole names it by key, so
		// the id is resolved once here -- the identical pattern
		// reapRoleBindings uses for the same reason.
		role, err := s.roles.FindByID(ctx, binding.RoleID)
		if err != nil {
			log.Warn("rbac could not resolve a deleted node's binding role",
				"event_type", evt.Type, "node_id", binding.NodeID, "role_id", binding.RoleID, "error", err)
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.RevokeRole(ctx, sub, role.Key, Scope{NodeID: binding.NodeID}); err != nil {
			if isBindingNotFound(err) {
				continue
			}
			log.Warn("rbac could not revoke a deleted node's role binding",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID, "role", role.Key, "error", err)
		}
	}
}
