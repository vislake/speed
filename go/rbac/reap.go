package rbac

import (
	"context"
	"encoding/json"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is rbac's subscriber for FOUR events ANOTHER module publishes,
// in two matched pairs. The first pair is org's org.member.removed,
// announced when a tenant removes a member, and org's org.node.deleted,
// announced when org removes a node (or, for a cascade, a whole subtree)
// from a tenant's organization tree. Both reap the same hazard from a
// different angle: a role binding that survives the fact it was scoped to
// is latent access, waiting for a bug elsewhere (a resurrected session, a
// restored membership, a node id that gets reused) to become real.
// org.member.removed closes that hazard for a binding scoped to a user who
// left the tenant; org.node.deleted closes it for a binding scoped to a
// node the tree no longer has -- org-rbac.md's P1-2 finding: rbac never
// consults org's tree at decision time (only a host-injected
// SubtreeResolver does, and only when one is wired), and before that
// round nothing reaped a binding left dangling on a deleted node at all.
// org's own TreeService.Delete already refuses to delete a subtree that
// still has members bound to it (memberGuardFor, go/org's tree.go) -- that
// refusal is real and stays exactly as it is -- but a node can carry an
// rbac binding with no org member ever having existed there, or a member
// removed independently of any binding cleanup, and deleting THAT node
// sails straight through org's own guard. This file's onNodeDeleted is
// the separate, additional safety net for exactly that case, never a
// replacement for org's own member-refusal.
//
// The second pair is the restore side of the same two facts: org's
// org.member.restored, announced when MemberService.Restore makes a
// mark-deleted membership visible again, and org's org.node.restored,
// announced when TreeService.Restore makes a mark-deleted node visible
// again. Where the first pair revokes, the second re-instates: a removal
// or a delete that was undone is an org statement that the bindings the
// corresponding reap withdrew are wanted back, and onMemberRestored /
// onNodeRestored undo exactly those revokes through Service.RestoreRole --
// the two handlers this file's lower half documents. Without them the
// reaps' mark-deletes would be a one-way door: a restored member or node
// would come back with every grant the reap withdrew still revoked,
// silently, forever.
//
// The module boundary is the reason this is a subscription at all, and the
// shape of the code below is dictated by what the boundary permits:
//
//   - rbac never imports org. org is a peer module released on its own
//     schedule, and importing it would invert the dependency direction
//     (docs/internal/01-architecture.md). rbac knows org by two facts per
//     event alone: the event's string name, spelled out as the unexported
//     constants below, and a JSON-shaped payload probe
//     (memberUserIDFromPayload, nodeDeletedIDsFromPayload,
//     nodeRestoredNodeIDFromPayload) that accepts the snake_case field
//     names org's payload structs carry, never org's types.
//
//   - rbac only subscribes; it never publishes any of the four events and
//     never declares any of them in Register. Publishing one would make
//     rbac a co-publisher of a fact only org can know (whether the
//     removal, the delete or the restore really committed), and declaring
//     one would widen the frozen event vocabulary with a foreign event --
//     the event registry is this module's own surface, and the four
//     "org.*" names are org's.
//
//   - Subscribe cannot fail and an absent publisher is not an error: a host
//     without org's module simply never fires either handler, exactly as a
//     host without authn never fires org's own authn.user.created handler.
//
// Both reaps are deliberately NOT a bulk delete. Each live binding is
// revoked one at a time through the same three effects an administrator's
// manual revoke performs -- the row is mark-deleted (so a later restore of
// the membership -- or, for onNodeDeleted, a later grant reassigned onto a
// reused node id -- can restore the grant), the process-local decision
// cache is invalidated, and EventRoleBindingRevoked is published so every
// replica converges. The reason the loop stays per-binding rather than
// becoming one bulk statement is precisely that triple: a bulk write would
// skip the per-row mark (the restore side needs every revoked row intact
// with its full grant tuple, which is what onMemberRestored and
// onNodeRestored hand back to Service.RestoreRole), the per-subject cache
// invalidation (the cache is keyed by subject; only the loop knows which
// subjects to drop), and the per-binding convergence events. What the loop
// does NOT do is re-read the row it is withdrawing: revokeReapedBindings
// revokes each enumerated binding BY ID and resolves its role once per
// distinct role per pass, where the public RevokeRole -- which takes a
// subject, a role key and a scope and re-resolves both -- would cost one
// full read-modify-delete cycle per binding. The two reaps run
// synchronously inside org's own request (the in-memory bus delivers
// in-process), so that per-binding overhead is what a many-binding
// cascade would otherwise drag into org's single HTTP DELETE.
//
// See onMemberRemoved's, onNodeDeleted's, onMemberRestored's and
// onNodeRestored's own doc comments for each handler's resilience
// contract, all mirroring the one org's own handleUserCreated documents
// for the authn.user.created event.

// eventMemberRemoved is org's org.member.removed event, the string rbac
// subscribes to. It is unexported and not part of eventDecls on purpose:
// rbac does not publish or declare it, it only listens (see the file
// comment above). The value is the whole of rbac's compile-time knowledge
// of org's event surface, exactly the way org's own knowledge of authn's
// authn.user.created is the string alone.
const eventMemberRemoved = "org.member.removed"

// eventMemberRestored is org's org.member.restored event, the restore
// counterpart of eventMemberRemoved: it fires when org's MemberService
// makes a mark-deleted membership visible again, and it is the signal that
// the bindings the member-removed reap revoked for that member are wanted
// back (see onMemberRestored). It is unexported and not part of eventDecls
// for the same reason eventMemberRemoved is not.
const eventMemberRestored = "org.member.restored"

// memberUserIDKeys are the field spellings rbac accepts for the member's
// user id inside either org member-roster event's payload -- org.member.removed's
// MemberRemoved.UserID and org.member.restored's MemberRestored.UserID
// carry the identical field -- probed in order.
//
// Several spellings are accepted because the payload reaches rbac as data,
// not as a type: a same-process publish delivers org's struct (whose
// snake_case JSON tags rbac cannot see at compile time without importing
// org), while the Redis bus delivers a map built from those same tags.
// Probing a small ordered list is what lets rbac read both shapes.
// The list mirrors the one org probes against authn.user.created payloads
// (go/org/events.go) -- the same boundary, the same technique.
var memberUserIDKeys = []string{"user_id", "userId", "UserID", "userID"}

// memberUserIDFromPayload extracts the member's user id from an
// org.member.removed or org.member.restored payload of any shape, by
// round-tripping it through JSON into a map and probing the accepted key
// spellings.
//
// It never type-asserts the publisher's concrete type -- doing so would
// require importing org, the one thing this module may not do -- and it
// returns ok=false rather than an error for every unusable shape, because
// the caller's contract is to log and continue, not to fail the publisher.
// The payload itself is never logged: it belongs to another module and may
// carry a name or an address.
func memberUserIDFromPayload(payload any) (string, bool) {
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
	for _, key := range memberUserIDKeys {
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
//     member's live bindings are revoked one by one; see
//     revokeReapedBindings for why the failures inside that pass are
//     logged and continued rather than returned.
//
// The handler never returns an error. On the in-memory bus it runs
// synchronously inside org's Remove call, after org's transaction has
// committed, so an error here would make a successful removal report
// failure to the very caller that triggered it.
func (s *Service) onMemberRemoved(ctx context.Context, evt pkgcore.Event) error {
	userID, ok := memberUserIDFromPayload(evt.Payload)
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
// The revoking itself is delegated to revokeReapedBindings, which both
// reaps share; see that method's doc comment for why each binding is
// withdrawn by id rather than through the public RevokeRole and what that
// keeps and what it skips.
func (s *Service) reapRoleBindings(ctx context.Context, evt pkgcore.Event, userID string) {
	log := observability.FromContext(ctx)

	bindings, err := s.bindings.ByUser(ctx, userID)
	if err != nil {
		log.Warn("rbac could not enumerate a removed member's role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return
	}
	s.revokeReapedBindings(ctx, evt, bindings)
}

// eventNodeDeleted is org's org.node.deleted event, the string rbac
// subscribes to for the dangling-binding reap. It is unexported and not
// part of eventDecls for the identical reason eventMemberRemoved is not:
// rbac does not publish or declare it, it only listens.
const eventNodeDeleted = "org.node.deleted"

// nodeDeletedIDsKeys are the field spellings rbac accepts for the deleted
// node id set inside an org.node.deleted payload, probed in order.
//
// The list mirrors memberUserIDKeys's own shape one level up: a
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
//     deleted ids is revoked; see revokeReapedBindings for why failures
//     inside that pass are logged and continued rather than returned.
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
// them is what this reap targets regardless of who holds it. The revoking
// is delegated to the shared revokeReapedBindings (see its doc comment for
// the per-binding cost shape), in one pass over every id the event named
// -- a single event for a cascade can carry many ids, and this runs one
// enumeration and one revoke loop over all of them, never one handler
// invocation per node.
func (s *Service) reapRoleBindingsForNodes(ctx context.Context, evt pkgcore.Event, nodeIDs []string) {
	log := observability.FromContext(ctx)

	bindings, err := s.bindings.ByNodes(ctx, nodeIDs)
	if err != nil {
		log.Warn("rbac could not enumerate bindings scoped to deleted nodes",
			"event_type", evt.Type, "error", err)
		return
	}
	s.revokeReapedBindings(ctx, evt, bindings)
}

// revokeReapedBindings withdraws every live binding in bindings -- the rows
// a removal or a deletion reap enumerated -- and is the per-binding revoke
// step the two reaps share.
//
// For each binding it performs exactly the three effects an administrator's
// manual revoke performs: the soft-delete mark (bindings.Delete, the
// identical dbkit mark-delete the public path uses), the process-local
// cache invalidation and the EventRoleBindingRevoked announcement that
// converges the other replicas (both inside publishBindingChanged). What it
// skips is the RE-READING RevokeRole performs on arguments it was handed:
// that method takes a subject, a role KEY and a scope and re-resolves the
// binding's row and the role's id from them, while a reap already holds
// the very row it enumerated. Each binding is therefore deleted BY ID, and
// the role it names is resolved once per DISTINCT role per pass rather
// than once per binding. That is the cost shape this module's performance
// contract needs: the two reaps run synchronously inside org's own request
// (the in-memory bus delivers in-process), and a many-binding cascade must
// not drag org's single HTTP DELETE through one full read-modify-delete
// cycle per binding -- measured, not timed, by the reap statement-counting
// regression in reap_test.go.
//
// A binding whose role cannot be resolved is left in place. Roles have no
// delete path in this module, so an unresolvable role is an anomaly worth
// a Warn rather than a crash -- reported once per binding that names it,
// exactly as the per-binding resolution the reaps performed before
// reported it.
//
// The pass never aborts on a failure, for the reason the reaps'
// enumeration-side comments give: a delivery that reaped some bindings and
// not others has done real work, and surfacing an error would make org's
// committed removal or delete look failed. Each failure is logged at Warn
// and the pass moves on; the residual bindings keep the tenant's own
// subsequent revokes (or a re-delivery, which finds nothing left to do --
// the reap is idempotent because both enumerations only return live rows)
// as their backstop. The one classified case is the delete's
// ErrRecordNotFound -- a concurrent administrator revoke already withdrew
// the binding between the enumeration and this delete, exactly the
// caller's goal, so it is not even worth a log line (the identical
// classification RevokeRole gives its own delete's zero-rows outcome).
func (s *Service) revokeReapedBindings(ctx context.Context, evt pkgcore.Event, bindings []RoleBinding) {
	log := observability.FromContext(ctx)

	resolvedRoles := make(map[string]*Role, len(bindings))
	for _, binding := range bindings {
		role, ok := resolvedRoles[binding.RoleID]
		if !ok {
			var err error
			role, err = s.roles.FindByID(ctx, binding.RoleID)
			if err != nil {
				log.Warn("rbac could not resolve a reaped binding's role",
					"event_type", evt.Type, "user_id", binding.UserID, "node_id", binding.NodeID,
					"role_id", binding.RoleID, "error", err)
				continue
			}
			resolvedRoles[binding.RoleID] = role
		}

		if err := s.bindings.Delete(ctx, binding.ID); err != nil {
			if hasCode(err, dbkit.ErrRecordNotFound.Code) {
				continue
			}
			log.Warn("rbac could not revoke a reaped role binding",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.publishBindingChanged(ctx, EventRoleBindingRevoked, sub, role, Scope{NodeID: binding.NodeID}); err != nil {
			log.Warn("rbac could not announce a reaped binding's revoke",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
		}
	}
}

// onMemberRestored is the subscriber Attach installs for org's
// org.member.restored event: it re-instates the role bindings the removal
// of this member reaped, now that org has made the membership visible
// again. Without it, org's restore would bring the person back with every
// grant the removal reap withdrew still revoked -- silently, forever.
//
// # The resilience contract
//
// The publisher is another module, released on its own schedule, and rbac
// has no compile-time knowledge of its payload. The cases are the ones
// onMemberRemoved documents for the removal event, mirrored exactly:
//
//  1. Nobody publishes the event. The subscription is installed and simply
//     never fires.
//
//  2. The payload is not a shape rbac recognizes. Logged at Warn, and nil
//     is returned -- returning an error would be actively harmful: on the
//     in-memory bus a handler's error propagates back to the publisher's
//     own Publish call, which org's Restore makes inside its own request
//     after the membership restore has already committed -- rbac's failure
//     to understand a payload would surface there as a failed restore.
//
//  3. The event carries no tenant. There is no tenant whose bindings could
//     be re-instated, so there is nothing to do. Logged at Debug and
//     skipped.
//
//  4. The event carries a tenant. The tenant context is rebuilt from the
//     event (pkgcore.WithTenant) because a handler invoked by the
//     distributed mode's bus runs on a context that carries none, and
//     every Repository call would otherwise fail closed. Then every
//     revoked binding of the restored member is re-instated one tuple at a
//     time; see reinstateRoleBindings for why the failures inside that
//     loop are logged and continued rather than returned.
//
// The handler never returns an error. On the in-memory bus it runs
// synchronously inside org's Restore call, after org's transaction has
// committed, so an error here would make a successful restore report
// failure to the very caller that triggered it.
func (s *Service) onMemberRestored(ctx context.Context, evt pkgcore.Event) error {
	userID, ok := memberUserIDFromPayload(evt.Payload)
	if !ok {
		observability.FromContext(ctx).Warn("rbac ignored a member-restored event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if evt.TenantID == "" {
		observability.FromContext(ctx).Debug("rbac ignored a member-restored event with no tenant",
			"event_type", evt.Type, "user_id", userID)
		return nil
	}

	ctx = pkgcore.WithTenant(ctx, evt.TenantID)
	s.reinstateRoleBindings(ctx, evt, userID)
	return nil
}

// reinstateRoleBindings re-instates every revoked role binding the
// restored member holds in the tenant ctx carries -- ctx was rebuilt from
// the event by onMemberRestored, so the subject here comes entirely from
// the event's tenant and payload, never from whatever identity the
// delivery context happened to carry.
//
// # What "every revoked binding" re-instates
//
// The removal reap revoked every binding the member held that was live at
// removal time (reapRoleBindings), so this side reads the mirror question:
// every binding of this (tenant, user) that is currently revoked,
// enumerated from the soft-deleted rows themselves
// (RoleBindingRepository.RevokedByUser) and re-instated through
// Service.RestoreRole -- the same tested path an administrator's manual
// restore takes: the most recently revoked row at the (user, role, node)
// tuple is un-marked, the process-local decision cache is invalidated, and
// EventRoleBindingRestored is published so every replica converges.
// RestoreRole is invoked once per tuple rather than once per row because a
// tuple can carry several soft-deleted rows (a
// revoke-then-reassign-then-revoke-again sequence leaves one per revoke);
// it restores the most recent, which for a tuple the removal reaped IS the
// reaped row, and one call per tuple keeps the announcement volume at one
// event per grant, exactly the deletion side's one revoke event per
// binding. A binding already live at the tuple -- regranted since the
// removal -- makes RestoreRole a no-op, per its own idempotent contract.
//
// # The attribution limitation, and why this is the closest sound shape
//
// A soft-deleted row carries no marker saying WHICH revoke wrote it: an
// administrator's manual revocation and a reap's write leave the identical
// two columns, both travelling through the same mark-delete path
// (Service.RevokeRole for the manual one, revokeReapedBindings for the
// reaps' -- the two share bindings.Delete and publishBindingChanged).
// Re-instating every revoked tuple of the restored member therefore also
// undoes revocations this member's removal did NOT reap -- a grant an
// administrator deliberately withdrew while the person was still a member,
// or one the NODE-deletion reap revoked because its node died, would come
// back with the membership. That over-reach is unavoidable from the data
// the reaps retain: the reaped set at removal time is exactly "the tuples
// that were live then", which nothing stored records, and telling the
// rows apart would need a marker this module's frozen deletion path does
// not write. The over-reach is biased to the availability side because a
// member restore is an affirmative, visible operator act (org's
// MemberService.Restore is called by membership id, never fired
// automatically), because it is the same un-attributed posture org's own
// Restore methods take toward rows with multiple delete origins, and
// because while the reaps are the only production writers of revoked rows
// -- no administrator revoke surface exists yet (deferrals D1/D12) -- it
// cannot occur at all. It is recorded, with the marker idea, as this
// module's deferral D14.
//
// The loop never aborts on a failure, for the identical reason
// reapRoleBindings' own doc comment gives: a member-restored delivery that
// re-instated nine of ten bindings has done real work, and every failure
// is logged at Warn rather than surfaced. ErrBindingNotFound from
// RestoreRole -- the tuple gained a live row or lost its revoked row to a
// concurrent actor between the enumeration above and the restore (the
// classification assign.go's RestoreRole comment documents) -- means the
// grant already holds or was withdrawn by a newer decision, so it is not
// even worth a log line, mirroring the deletion side's identical
// classification of the same case.
func (s *Service) reinstateRoleBindings(ctx context.Context, evt pkgcore.Event, userID string) {
	log := observability.FromContext(ctx)

	revoked, err := s.bindings.RevokedByUser(ctx, userID)
	if err != nil {
		log.Warn("rbac could not enumerate a restored member's revoked role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return
	}
	seen := make(map[string]struct{}, len(revoked))
	for _, binding := range revoked {
		// One restore decision per (user, role, node) tuple; see the method
		// doc comment for why RestoreRole is tuple-addressed at all.
		tuple := binding.UserID + "\x00" + binding.RoleID + "\x00" + binding.NodeID
		if _, dup := seen[tuple]; dup {
			continue
		}
		seen[tuple] = struct{}{}

		// The revoked row names the role by id; RestoreRole names it by key,
		// so the id is resolved once here -- the identical pattern
		// reapRoleBindings uses for the revoke direction. Roles have no
		// delete path in this module, so a revoked binding whose role cannot
		// be found is an anomaly worth a log line rather than a crash.
		role, err := s.roles.FindByID(ctx, binding.RoleID)
		if err != nil {
			log.Warn("rbac could not resolve a restored member's revoked binding role",
				"event_type", evt.Type, "user_id", userID, "role_id", binding.RoleID, "error", err)
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.RestoreRole(ctx, sub, role.Key, Scope{NodeID: binding.NodeID}); err != nil {
			if isBindingNotFound(err) {
				continue
			}
			log.Warn("rbac could not restore a restored member's revoked role binding",
				"event_type", evt.Type, "user_id", userID, "role", role.Key, "node_id", binding.NodeID, "error", err)
		}
	}
}

// eventNodeRestored is org's org.node.restored event, the string rbac
// subscribes to for the re-instatement side of the dangling-binding reap.
// It is unexported and not part of eventDecls for the identical reason
// eventNodeDeleted is not: rbac does not publish or declare it, it only
// listens.
const eventNodeRestored = "org.node.restored"

// nodeRestoredNodeIDKeys are the field spellings rbac accepts for the
// restored node's id inside an org.node.restored payload, probed in order.
//
// The list mirrors nodeDeletedIDsKeys's own shape one level down: a
// same-process publish delivers org's NodeRestored struct (whose
// snake_case JSON tag, node_id, rbac cannot see at compile time without
// importing org), while the Redis bus delivers a map built from that same
// tag. org's own field is spelled NodeID (see go/org/events.go's own doc
// comment on NodeRestored), so the untagged-struct spelling probed here
// matches that exactly.
var nodeRestoredNodeIDKeys = []string{"node_id", "nodeId", "NodeID"}

// nodeRestoredNodeIDFromPayload extracts the restored node's id from an
// org.node.restored payload of any shape, by round-tripping it through
// JSON into a map and probing the accepted key spellings.
//
// It never type-asserts the publisher's concrete type -- doing so would
// require importing org, the one thing this module may not do -- and it
// returns ok=false rather than an error for every unusable shape or an
// empty id, because the caller's contract is to log and continue, not to
// fail the publisher. The payload itself is never logged: it belongs to
// another module.
func nodeRestoredNodeIDFromPayload(payload any) (string, bool) {
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
	for _, key := range nodeRestoredNodeIDKeys {
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

// onNodeRestored is the subscriber Attach installs for org's
// org.node.restored event: it re-instates every role binding scoped to the
// one node org just made visible again. org's Restore is deliberately
// per-node, never cascading, so this re-instates exactly the one restored
// node's bindings -- never a descendant's, which stay revoked until that
// descendant is itself restored and fires its own event.
//
// # The resilience contract
//
// The publisher is another module, released on its own schedule, and rbac
// has no compile-time knowledge of its payload. The cases mirror
// onMemberRestored's own, one level down (a node id in place of a user
// id):
//
//  1. Nobody publishes the event. The subscription is installed and simply
//     never fires.
//
//  2. The payload is not a shape rbac recognizes, or carries an empty id.
//     Logged at Warn, and nil is returned -- returning an error would
//     surface here as a failed restore on org's side, exactly as
//     onMemberRestored's own doc comment explains for its event.
//
//  3. The event carries no tenant. There is no tenant whose bindings could
//     be re-instated, so there is nothing to do. Logged at Debug and
//     skipped.
//
//  4. The event carries a tenant and a node id. The tenant context is
//     rebuilt from the event for the same reason onMemberRestored's does,
//     and every revoked binding scoped to the restored node is
//     re-instated; see reinstateRoleBindingsForNode for why failures
//     inside that loop are logged and continued rather than returned.
//
// The handler never returns an error, for the same reason onNodeDeleted's
// does not: on the in-memory bus it runs synchronously inside org's
// TreeService.Restore call, after org's own transaction has committed, so
// an error here would make a successful restore report failure to the very
// caller that triggered it.
func (s *Service) onNodeRestored(ctx context.Context, evt pkgcore.Event) error {
	nodeID, ok := nodeRestoredNodeIDFromPayload(evt.Payload)
	if !ok {
		observability.FromContext(ctx).Warn("rbac ignored a node-restored event with an unrecognized payload",
			"event_type", evt.Type)
		return nil
	}
	if evt.TenantID == "" {
		observability.FromContext(ctx).Debug("rbac ignored a node-restored event with no tenant",
			"event_type", evt.Type)
		return nil
	}

	ctx = pkgcore.WithTenant(ctx, evt.TenantID)
	s.reinstateRoleBindingsForNode(ctx, evt, nodeID)
	return nil
}

// reinstateRoleBindingsForNode re-instates every revoked role binding
// scoped to the restored node, in the tenant ctx carries -- ctx was rebuilt
// from the event by onNodeRestored, so the tenant comes entirely from the
// event, never from whatever identity the delivery context happened to
// carry.
//
// Unlike reinstateRoleBindings, which enumerates by user, this enumerates
// by node: a node-restored event carries no user at all, only the one node
// id org just made visible again, and every revoked binding scoped to it is
// what this re-instates regardless of who holds it -- the mirror of
// reapRoleBindingsForNodes' own per-node enumeration, one pass over the
// revoked rows at the single restored node. Each distinct (user, role)
// tuple at that node is re-instated through the identical RestoreRole path
// reinstateRoleBindings already reuses -- see that method's doc comment
// for the loop's failure handling and its attribution-limitation
// paragraph: a revocation at this node that predates the node's deletion
// and was never followed by a re-grant is indistinguishable from a row the
// node-deletion reap wrote, so it is re-instated too, the same
// un-attributed posture org's own per-node TreeService.Restore takes
// toward its own rows (recorded as deferral D14).
func (s *Service) reinstateRoleBindingsForNode(ctx context.Context, evt pkgcore.Event, nodeID string) {
	log := observability.FromContext(ctx)

	revoked, err := s.bindings.RevokedByNodes(ctx, []string{nodeID})
	if err != nil {
		log.Warn("rbac could not enumerate revoked bindings scoped to the restored node",
			"event_type", evt.Type, "node_id", nodeID, "error", err)
		return
	}
	seen := make(map[string]struct{}, len(revoked))
	for _, binding := range revoked {
		// One restore decision per (user, role) tuple at this one node; the
		// node is constant, so the tuple key omits it. See
		// reinstateRoleBindings' own comment for why RestoreRole is
		// tuple-addressed at all.
		tuple := binding.UserID + "\x00" + binding.RoleID
		if _, dup := seen[tuple]; dup {
			continue
		}
		seen[tuple] = struct{}{}

		// The revoked row names the role by id; RestoreRole names it by key,
		// so the id is resolved once here -- the identical pattern
		// reinstateRoleBindings uses for the same reason.
		role, err := s.roles.FindByID(ctx, binding.RoleID)
		if err != nil {
			log.Warn("rbac could not resolve a restored node's revoked binding role",
				"event_type", evt.Type, "node_id", nodeID, "role_id", binding.RoleID, "error", err)
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.RestoreRole(ctx, sub, role.Key, Scope{NodeID: binding.NodeID}); err != nil {
			if isBindingNotFound(err) {
				continue
			}
			log.Warn("rbac could not restore a restored node's revoked role binding",
				"event_type", evt.Type, "node_id", nodeID, "user_id", binding.UserID, "role", role.Key, "error", err)
		}
	}
}
