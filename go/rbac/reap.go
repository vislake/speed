package rbac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

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
// onNodeRestored undo exactly those revokes -- the two handlers this
// file's lower half documents. Without them the reaps' mark-deletes would
// be a one-way door: a restored member or node would come back with every
// grant the reap withdrew still revoked, silently, forever.
//
// The re-instatement is precise to the row, never to the scope, because a
// revoked row now carries the revoke-origin marker this file's
// D14-resolution adds (migrations/{postgres,sqlite}/0003_add_revoke_origin
// .sql, model.go's RevokeOrigin field): every mark-delete records which
// writer performed it -- the deliberate Service.RevokeRole path, the
// member-removal reap, or the node-deletion reap -- and the restore side
// scopes by that marker. onMemberRestored restores only rows carrying the
// member-removal origin, so a deliberate revocation that predates the
// removal is never silently undone; onNodeRestored restores only rows
// carrying the node-deletion origin, so a row the node-deletion reap wrote
// for a user who has SINCE left the tenant is not resurrected by the
// node's return (the member-removal reap's claim step, claimByUser,
// re-attributes such rows to the member-removal the moment the member
// leaves, leaving the user's own member-restore event -- which fires only
// when the membership is genuinely live again -- as their resurrection
// path). The member-restore side re-verifies the row's other structural
// precondition in turn: a row whose node is STILL gone when the member
// returns is not un-marked -- it is re-attributed to the node-deletion
// origin (RoleBindingRepository.reattributeToNodeDeletion) and waits for
// the node's own restore, never returning to Can while the node is hidden
// (see bindingNodeLivesAtMemberRestore). The restore side un-marks each
// matching row BY ID through RoleBindingRepository.Restore, announcing
// each restored grant with EventRoleBindingRestored exactly as the
// deletion side announces each revoked one.
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
// manual revoke performs -- the row is mark-deleted with the reap's origin
// recorded in the same UPDATE (so a later restore of the membership or the
// node -- or, for onNodeDeleted, a later grant reassigned onto a reused
// node id -- can restore the grant; the origin is what tells the restore
// side which rows belong to which reap, see the file header), the
// process-local decision cache is invalidated, and EventRoleBindingRevoked
// is published so every replica converges. The reason the loop stays
// per-binding rather than becoming one bulk statement is precisely that
// triple: a bulk write would skip the per-row mark and its origin (the
// restore side needs every revoked row intact with its full grant tuple,
// which is what onMemberRestored and onNodeRestored hand back to the
// shared reinstateReapedBindings step), the per-subject cache invalidation
// (the cache is keyed by subject; only the loop knows which subjects to
// drop), and the per-binding convergence events. What the loop does NOT do
// is re-read the row it is withdrawing: revokeReapedBindings revokes each
// enumerated binding BY ID and resolves its role once per distinct role
// per pass, where the public RevokeRole -- which takes a subject, a role
// key and a scope and re-resolves both -- would cost one full
// read-modify-delete cycle per binding. Where the loop RUNS depends on the
// host's wiring: on a host with a jobs queue (Module.WithQueue) each reap
// is a queue task the queue retries until it converges (reap_jobs.go); on
// a host without one the reaps run synchronously inside org's own request
// (the in-memory bus delivers in-process) -- where that per-binding
// overhead is what a many-binding cascade would otherwise drag into org's
// single HTTP DELETE -- with failures logged and never retried. The
// restore-side handlers (onMemberRestored, onNodeRestored) always run
// synchronously inside org's restore request, whichever side the reaps run
// on: their re-instating must stay ordered behind the events' own delivery
// (see reap_jobs.go's ordering note), and a restore request is not where a
// per-binding overhead belongs anyway.
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

// membershipIDKeys are the field spellings rbac accepts for the membership
// id inside an org.member.removed payload (MemberRemoved.MembershipID),
// probed in order like memberUserIDKeys. It is read only so the
// queue-backed reap task can key itself on the removal INSTANCE (reap_
// jobs.go's memberReapKey): one user's successive removals are distinct
// membership instances, and distinct keys keep their reaps distinct.
var membershipIDKeys = []string{"membership_id", "membershipId", "MembershipID"}

// membershipIDFromPayload extracts the membership id from an
// org.member.removed payload of any shape, by the identical JSON
// round-trip probe memberUserIDFromPayload performs. It reports ok=false
// for every unusable shape; the caller (onMemberRemoved) treats an absent
// id as a key fallback to the user id, never as an error.
func membershipIDFromPayload(payload any) (string, bool) {
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
	for _, key := range membershipIDKeys {
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
//     reaping happens one of two ways, depending on whether the host
//     wired a jobs queue (Module.WithQueue -- see reap_jobs.go's header
//     comment for the full shape): with a queue, the reap is ENQUEUED as
//     a task and its failures are the queue's to retry; without one, it
//     runs synchronously here, its failures logged and never retried.
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
	if s.queue != nil {
		membershipID, _ := membershipIDFromPayload(evt.Payload)
		if err := s.enqueueMemberReap(ctx, evt.TenantID, userID, membershipID); err != nil {
			observability.FromContext(ctx).Warn("rbac could not enqueue a removed member's reaping; the removal's bindings may stay live",
				"event_type", evt.Type, "user_id", userID, "error", err)
		}
		return nil
	}
	if err := s.reapRoleBindings(ctx, evt, userID); err != nil {
		// The synchronous fallback for a host that wired no queue: one
		// aggregate log line for whatever the reap could not complete,
		// instead of the per-binding lines the pass used to emit. Nothing
		// retries it -- that is exactly what wiring a queue buys (see
		// reap_jobs.go's header comment).
		observability.FromContext(ctx).Warn("rbac could not fully reap a removed member's role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
	}
	return nil
}

// reapRoleBindings performs the member-removal reap's two effects on the
// removed member's bindings in the tenant ctx carries -- ctx was rebuilt
// from the event by onMemberRemoved, so the subject here comes entirely
// from the event's tenant and payload, never from whatever identity the
// delivery context happened to carry (a bus subscriber context has none
// anyway, and a publisher's identity must never steer whose grants get
// revoked).
//
// The first effect is the claim (claimNodeReapedBindings): the member's
// still-revoked rows that the NODE-deletion reap wrote are re-attributed
// to this removal, so a later node restore -- which might well arrive while
// the member is still gone -- cannot resurrect authorization for someone
// who is no longer a member. Without the claim, the row set the node
// deletion reaped and the row set this removal revokes would disagree
// exactly when the two reaps' events interleave, and the node restore
// would rebuild a grant the removal had in fact ended (this file's
// D14-resolution, P0-rbac-8).
//
// The second effect is the revoke itself, delegated to revokeReapedBindings
// -- which both reaps share, and which this reap drives with the
// member-removal origin so the rows it writes carry the marker that tells
// onMemberRestored they are the removal's own (see that method's doc
// comment for why each binding is withdrawn by id rather than through the
// public RevokeRole and what that keeps and what it skips).
//
// Both effects fail independently and neither aborts the other: the claim
// runs FIRST and unconditionally because it does not depend on the
// live-row enumeration at all -- it targets rows that are already revoked,
// and a failure to enumerate the live ones must not also skip the claim --
// and every failure anywhere in the pass is returned as one joined error
// for the CALLER to handle, never silently dropped. The caller is either
// onMemberRemoved's no-queue fallback (logs the joined error at Warn and
// moves on, the pre-queue behavior) or the queue-backed reap task
// (memberReapTask, which returns it so the queue retries the reap -- the
// retry home the old design's redelivery backstop never was; see
// reap_jobs.go's header comment). The claim's own UPDATE is idempotent, so
// a retry that reaches it again rewrites nothing.
func (s *Service) reapRoleBindings(ctx context.Context, evt pkgcore.Event, userID string) error {
	var errs []error

	if err := s.claimNodeReapedBindings(ctx, evt, userID); err != nil {
		errs = append(errs, err)
	}

	bindings, err := s.bindings.ByUser(ctx, userID)
	if err != nil {
		errs = append(errs, fmt.Errorf("enumerating the removed member's role bindings: %w", err))
		return errors.Join(errs...)
	}
	errs = append(errs, s.revokeReapedBindings(ctx, evt, bindings, revokeOriginMemberRemoval)...)
	return errors.Join(errs...)
}

// claimNodeReapedBindings runs the member-removal reap's claim step:
// every currently soft-deleted binding of userID that the node-deletion
// reap wrote (revoke_origin = 'node-deletion') is re-attributed to the
// member-removal origin (RoleBindingRepository.claimByUser). It exists
// because the two reaps enumerate DIFFERENT row sets, and the difference
// is exactly where a removed member's authorization could otherwise come
// back: the node-deletion reap withdraws a binding scoped to a deleted
// node, and if the user is removed from the tenant AFTER that -- with no
// live binding left for the member-removal reap to withdraw -- a later
// node restore would see a node-deletion row and re-instate it for someone
// who is no longer a member. The claim is what tells that node restore
// "no": the row now belongs to the member-removal, and only the user's own
// org.member.restored event (which fires only when the membership is live
// again) can resurrect it.
//
// Deliberate RevokeRole rows are deliberately left untouched: no org event
// may ever resurrect them, so there is nothing for the claim to do. The
// step writes no row state beyond the marker -- no decision changes, no
// event is published, and the durable re-attribution alone is what the
// later node-restored handler reads -- which is also why a re-run of this
// same removal's reap finds nothing left to claim and rewrites nothing
// (the reap stays idempotent under the queue's retries).
//
// The error is returned, after a Warn log, for reapRoleBindings to join
// into its aggregate: a claim that fails is a re-attribution that did not
// happen, and the queue must retry it exactly like a failed revoke.
func (s *Service) claimNodeReapedBindings(ctx context.Context, evt pkgcore.Event, userID string) error {
	if err := s.bindings.claimByUser(ctx, userID); err != nil {
		observability.FromContext(ctx).Warn("rbac could not claim a removed member's node-reaped role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return fmt.Errorf("claiming the removed member's node-reaped role bindings: %w", err)
	}
	return nil
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
//     onMemberRemoved's does, and the reaping happens the same two ways
//     that subscriber's does: enqueued as a task when the host wired a
//     jobs queue, run synchronously (failures logged, never retried)
//     otherwise.
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
	if s.queue != nil {
		if err := s.enqueueNodeReap(ctx, evt.TenantID, nodeIDs); err != nil {
			observability.FromContext(ctx).Warn("rbac could not enqueue the reaping of bindings scoped to deleted nodes; they may stay live",
				"event_type", evt.Type, "error", err)
		}
		return nil
	}
	if err := s.reapRoleBindingsForNodes(ctx, evt, nodeIDs); err != nil {
		// The synchronous fallback for a host that wired no queue; see
		// onMemberRemoved's identical call for the shape.
		observability.FromContext(ctx).Warn("rbac could not fully reap the bindings scoped to deleted nodes",
			"event_type", evt.Type, "error", err)
	}
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
// the per-binding cost shape), driven with the node-deletion origin so the
// rows this reap writes carry the marker that tells onNodeRestored they
// are this deletion's own, in one pass over every id the event named
// -- a single event for a cascade can carry many ids, and this runs one
// enumeration and one revoke loop over all of them, never one handler
// invocation per node.
//
// It returns the joined error of everything the pass could not complete
// (see reapRoleBindings for the caller contract); the enumeration failure
// is the one whole-pass case and returns immediately, with the failures of
// the revoke pass joined on top of it.
func (s *Service) reapRoleBindingsForNodes(ctx context.Context, evt pkgcore.Event, nodeIDs []string) error {
	bindings, err := s.bindings.ByNodes(ctx, nodeIDs)
	if err != nil {
		return fmt.Errorf("enumerating bindings scoped to the deleted nodes: %w", err)
	}
	return errors.Join(s.revokeReapedBindings(ctx, evt, bindings, revokeOriginNodeDeletion)...)
}

// revokeReapedBindings withdraws every live binding in bindings -- the rows
// a removal or a deletion reap enumerated -- recording origin as the
// revoke's writer -- and is the per-binding revoke step the two reaps
// share: reapRoleBindings drives it with revokeOriginMemberRemoval,
// reapRoleBindingsForNodes with revokeOriginNodeDeletion. origin is the
// marker the restore side scopes by (see the file header's D14-resolution
// and model.go's RevokeOrigin field comment): it is written into the same
// mark-delete UPDATE that soft-deletes the row, atomically, so no revoked
// row can exist whose writer this reap does not name.
//
// For each binding it performs exactly the three effects an administrator's
// manual revoke performs: the soft-delete mark (bindings.Delete, now the
// origin-aware mark-delete RoleBindingRepository.Delete shadows -- the
// public RevokeRole path drives the same method with the deliberate
// origin), the process-local cache invalidation and the
// EventRoleBindingRevoked announcement that converges the other replicas
// (both inside publishBindingChanged). What it skips is the RE-READING
// RevokeRole performs on arguments it was handed: that method takes a
// subject, a role KEY and a scope and re-resolves the binding's row and
// the role's id from them, while a reap already holds the very row it
// enumerated. Each binding is therefore deleted BY ID, and the role it
// names is resolved once per DISTINCT role per pass rather than once per
// binding. That is the cost shape this module's performance contract
// needs -- measured, not timed, by the reap statement-counting regression
// in reap_test.go.
//
// # Failure semantics: retryable failures travel, the pass never aborts
//
// The pass never aborts on a failure: a reap that withdrew nine of ten
// bindings has done real work, and the remaining one is exactly what a
// retry exists to converge. Every failure that a retry could fix -- a
// binding whose role could not be resolved, a delete that failed -- is
// therefore RETURNED as one entry of the caller's joined error, per
// binding and naming the row, so the caller can retry the whole reap: a
// re-run re-enumerates live rows only (both enumerations return live rows
// alone, which is what keeps the reap idempotent under the queue's
// retries), so the bindings this pass already withdrew are gone from the
// next attempt's enumeration and the failed ones come around again. The
// queue-backed caller (memberReapTask / nodeReapTask) returns the joined
// error to the queue, which retries the task; the synchronous fallback
// logs it and moves on. A binding whose role cannot be resolved is left in
// place and its failure is returned too: roles have no delete path in this
// module, so an unresolvable role is an anomaly -- a row that grants
// nothing -- and a reap that dead-letters over it is the standing alert
// that anomaly deserves.
//
// Two outcomes are classified silent, never failures: the delete's
// ErrRecordNotFound -- a concurrent administrator revoke already withdrew
// the binding between the enumeration and this delete, exactly the
// caller's goal, so there is nothing left for this pass to do (the
// identical classification RevokeRole gives its own delete's zero-rows
// outcome) -- and a failed EventRoleBindingRevoked ANNOUNCEMENT, which is
// logged at Warn and not returned: the row is already revoked, so a retry
// would re-enumerate nothing and could never re-announce it, and the
// replicas' stale decisions are covered by the anti-loss TTL exactly as a
// manual revoke's own publish failure is.
func (s *Service) revokeReapedBindings(ctx context.Context, evt pkgcore.Event, bindings []RoleBinding, origin string) []error {
	log := observability.FromContext(ctx)

	var errs []error
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
				errs = append(errs, fmt.Errorf("binding %s (role %s) has no resolvable role: %w", binding.ID, binding.RoleID, err))
				continue
			}
			resolvedRoles[binding.RoleID] = role
		}

		if err := s.bindings.Delete(ctx, binding.ID, origin); err != nil {
			if hasCode(err, dbkit.ErrRecordNotFound.Code) {
				continue
			}
			log.Warn("rbac could not revoke a reaped role binding",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
			errs = append(errs, fmt.Errorf("revoking binding %s (%s, %s at node %s): %w",
				binding.ID, binding.UserID, role.Key, binding.NodeID, err))
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.publishBindingChanged(ctx, EventRoleBindingRevoked, sub, role, Scope{NodeID: binding.NodeID}); err != nil {
			log.Warn("rbac could not announce a reaped binding's revoke",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
		}
	}
	return errs
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
//     every Repository call would otherwise fail closed. Then the
//     restored member's revoked bindings that carry the member-removal
//     origin are re-instated one row at a time; see
//     reinstateRoleBindings for why the failures inside that loop are
//     logged and continued rather than returned.
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

// reinstateRoleBindings re-instates the role bindings this member's own
// removal reaped, now that org has made the membership visible again. ctx
// was rebuilt from the event by onMemberRestored, so the subject here
// comes entirely from the event's tenant and payload, never from whatever
// identity the delivery context happened to carry.
//
// # What re-instates what
//
// The removal reap wrote the member-removal origin onto every row it
// withdrew AND claimed the member's still-revoked node-deletion rows into
// the same origin (reapRoleBindings), so the row set this side must bring
// back is exactly: the (tenant, user)'s currently revoked bindings whose
// revoke_origin says member-removal. It is enumerated from the soft-deleted
// rows themselves (RoleBindingRepository.RevokedByUser) and filtered in Go
// by the marker; every row that does NOT carry it is left revoked,
// deliberately: a deliberate RevokeRole revocation (origin empty) is an
// operator decision no org event may undo, and a row still carrying the
// node-deletion origin belongs to a node restore, not a member restore.
// That precision is what deferral D14 promised and what this round's
// marker migration (0003_add_revoke_origin.sql) delivers; the pre-marker
// rows the backfill left on the empty origin are treated exactly like
// deliberate revocations -- never resurrected by an org event, restored
// the explicit way if an operator wants them back.
//
// Each matching row is un-marked BY ID through
// RoleBindingRepository.Restore -- the same conditional write an
// administrator's manual Service.RestoreRole performs on the row its
// findMostRecentlyRevoked recovers, but addressed to the very row the
// marker names, never to "whatever is most recently revoked at the tuple":
// a tuple whose newest revoked row is a later deliberate revocation (or a
// later regrant's node-deletion reap) must keep THAT row revoked while the
// older, genuinely-reaped row comes back, and only an id-addressed restore
// can tell the two apart. Row-level precision also makes the tuple
// de-duplication the pre-marker loop needed unnecessary: if two
// member-removal rows ever share one tuple (a removal cycle per row), the
// first restore makes the tuple live and the second collides with the
// partial unique index -- a classified skip, never an error or a second
// live row. Each successful un-mark invalidates the subject's cached
// decisions and publishes EventRoleBindingRestored (both inside
// publishBindingChanged), so every replica converges exactly as it does on
// a manual restore.
//
// # The structural-precondition gate (P1-rbac-reinstate-node)
//
// The org.member.restored event asserts ONE of the two facts a revoked
// member-removal row's grant depends on -- the membership is visible
// again. The other, the node a node-scoped row is scoped to, is not
// asserted by anything in the event, and it must be re-verified before any
// row is un-marked: a restored member whose row the removal claimed while
// her node was deleted (or whose node was deleted while she was gone)
// must not regain a grant at a node org still hides. Can -- the coarse
// gate this module's HTTP surface answers -- never consults node liveness
// at decision time (scope.go), so the reap discipline is the entire
// protection of that gate, and a re-instatement that skipped the check
// re-opened it: the three-step sequence (delete the node -> remove the
// member -> restore the member) made the binding on the deleted node live
// again and Can answered allowed. Every member-removal row scoped to a
// node therefore passes through bindingNodeLivesAtMemberRestore before any
// role resolution; a row whose node cannot be verified stays revoked (see
// that method for the two dispositions). The node-restored path has no
// equivalent gate by design: its event asserts the node's own visibility,
// which is the fact its rows wait on.
//
// A row whose role can no longer be resolved is left revoked with a Warn,
// the identical treatment the revoke side gives an unresolvable row -- a
// role cannot die through this module, so the anomaly is worth one log
// line, never an abort.
//
// The loop never aborts on a failure, for the identical reason
// reapRoleBindings' own doc comment gives: a member-restored delivery that
// re-instated nine of ten bindings has done real work, and every failure
// is logged at Warn rather than surfaced. Two outcomes are classified
// silent, mirroring assign.go's RestoreRole treatment of its own restore
// races: dbkit.ErrRecordNotFound -- the row is no longer revoked when the
// un-mark lands, meaning a concurrent restore of the same row won and the
// desired end state already holds -- and the gorm.ErrDuplicatedKey a
// restore collides with when a live row now occupies the tuple, which
// means the grant already exists. In both cases the caller's goal is
// achieved by someone else's write, so neither is worth a log line.
func (s *Service) reinstateRoleBindings(ctx context.Context, evt pkgcore.Event, userID string) {
	log := observability.FromContext(ctx)

	revoked, err := s.bindings.RevokedByUser(ctx, userID)
	if err != nil {
		log.Warn("rbac could not enumerate a restored member's revoked role bindings",
			"event_type", evt.Type, "user_id", userID, "error", err)
		return
	}
	s.reinstateReapedBindings(ctx, evt, revoked, revokeOriginMemberRemoval)
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
// org.node.restored event: it re-instates the role bindings scoped to the
// one node org just made visible again that THIS node's deletion reaped --
// never a deliberate revocation at the node, and never a row the
// member-removal claimed for a member who has since left (see
// reinstateRoleBindingsForNode). org's Restore is deliberately per-node,
// never cascading, so this re-instates exactly the one restored node's
// bindings -- never a descendant's, which stay revoked until that
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
//     and the revoked bindings scoped to the restored node that carry the
//     node-deletion origin are re-instated; see
//     reinstateRoleBindingsForNode for why failures inside that loop are
//     logged and continued rather than returned.
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

// reinstateRoleBindingsForNode re-instates the role bindings THIS node's
// deletion reaped, now that org has made the node visible again. ctx was
// rebuilt from the event by onNodeRestored, so the tenant comes entirely
// from the event, never from whatever identity the delivery context
// happened to carry.
//
// Unlike reinstateRoleBindings, which enumerates by user, this enumerates
// by node: a node-restored event carries no user at all, only the one node
// id org just made visible again, and the revoked rows scoped to it are
// what this re-instates -- the mirror of reapRoleBindingsForNodes' own
// per-node enumeration, one pass over the revoked rows at the single
// restored node, filtered in Go by the node-deletion origin the reaping
// pass wrote onto every row it withdrew.
//
// The filter is the whole of the hazard closure this side must provide: a
// node restore must bring back exactly the grants the node deletion took,
// for holders who are still members -- and it must NOT rebuild
// authorization for a holder who left the tenant while the node was gone.
// The marker delivers both halves. A row that does not carry the
// node-deletion origin stays revoked: a deliberate RevokeRole at this node
// is an operator decision no node restore may undo, and a row the
// member-removal reap wrote (or claimed -- reapRoleBindings re-attributes
// a removed member's node-deletion rows to the member-removal the moment
// she leaves) belongs to that member's own restore, not to the node's: if
// it came back here, a user removed from the tenant would regain live
// authorization on the node's mere return, with no membership behind it --
// the P0-rbac-8 escalation this round closes. A node-reaped row whose
// holder was never removed carries the origin and comes back, which is the
// mechanism's intended case.
//
// Each matching row is un-marked BY ID through the same
// reinstateReapedBindings step reinstateRoleBindings uses -- the row-level
// restore, cache invalidation and EventRoleBindingRestored announcement
// that method's doc comment describes in full -- so the two restore-side
// subscribers share one un-marking loop exactly as the two reaps share one
// revoking loop.
func (s *Service) reinstateRoleBindingsForNode(ctx context.Context, evt pkgcore.Event, nodeID string) {
	log := observability.FromContext(ctx)

	revoked, err := s.bindings.RevokedByNodes(ctx, []string{nodeID})
	if err != nil {
		log.Warn("rbac could not enumerate revoked bindings scoped to the restored node",
			"event_type", evt.Type, "node_id", nodeID, "error", err)
		return
	}
	s.reinstateReapedBindings(ctx, evt, revoked, revokeOriginNodeDeletion)
}

// reinstateReapedBindings un-marks every binding in revoked that carries
// origin -- the rows one restore-side subscriber enumerated and the
// matching reap attributed to it -- and is the per-binding re-instatement
// step the two restore-side subscribers share, revokeReapedBindings'
// mirror image: the same resolved-once-per-distinct-role role resolution,
// the same per-row write, the same publishBindingChanged effects (cache
// invalidation and the EventRoleBindingRestored announcement that
// converges the replicas), and the same log-and-continue failure handling.
//
// Each matching row is restored BY ID through RoleBindingRepository.Restore
// -- the same conditional un-mark an administrator's manual RestoreRole
// performs, but addressed to the very row the marker names, never to
// "whatever is most recently revoked at the tuple", so a tuple whose newest
// revoked row was written by a DIFFERENT writer (a later deliberate
// revocation, a later regrant reaped by the other event) keeps that row
// revoked while the genuinely-reaped one comes back. That row-level
// precision is the point of the whole marker: it is what makes
// re-instatement unable to resurrect a revocation the matching removal or
// deletion did not make.
//
// A binding whose role cannot be resolved is left revoked with a Warn,
// exactly as revokeReapedBindings leaves an unresolvable row in place.
// Rows not carrying origin are skipped before any role resolution -- the
// marker decides, and the row sets a subscriber may hold are otherwise
// full of rows that must stay revoked. For a member-restored pass (origin
// is revokeOriginMemberRemoval) a node-scoped row must additionally clear
// the structural-precondition gate bindingNodeLivesAtMemberRestore -- a
// member's own return cannot re-open a grant whose node org still hides --
// and a row the gate declines is skipped here, re-attributed or left in
// place by that method (see reinstateRoleBindings' doc comment for the
// full rationale).
//
// Two restore outcomes are classified silent, mirroring assign.go's
// RestoreRole treatment of its own restore races: dbkit.ErrRecordNotFound
// -- the row is no longer revoked by the time the un-mark lands (a
// concurrent restore of the same row won, so the grant already holds) --
// and the gorm.ErrDuplicatedKey a restore collides with when a live row
// now occupies the tuple (a fresh AssignRole since the enumeration, so the
// grant already exists). In both cases the caller's goal was achieved by
// someone else's write, so neither is worth a log line. Anything else is
// logged at Warn and the loop moves on, for the same reason the revoke
// side never aborts: a restore delivery that re-instated nine of ten
// grants has done real work, and surfacing an error would make org's
// committed restore look failed.
func (s *Service) reinstateReapedBindings(ctx context.Context, evt pkgcore.Event, revoked []RoleBinding, origin string) {
	log := observability.FromContext(ctx)

	resolvedRoles := make(map[string]*Role, len(revoked))
	for _, binding := range revoked {
		if binding.RevokeOrigin != origin {
			continue
		}
		if origin == revokeOriginMemberRemoval && binding.NodeID != "" &&
			!s.bindingNodeLivesAtMemberRestore(ctx, log, evt, binding) {
			// The node the row is scoped to does not resolve (or cannot be
			// verified): the member's own return cannot re-open it. The
			// helper decided the row's disposition -- re-attributed to the
			// node-deletion origin when the node is genuinely gone, left
			// under the member-removal origin when the answer was unknown.
			continue
		}
		role, ok := resolvedRoles[binding.RoleID]
		if !ok {
			var err error
			role, err = s.roles.FindByID(ctx, binding.RoleID)
			if err != nil {
				log.Warn("rbac could not resolve a reinstated binding's role",
					"event_type", evt.Type, "user_id", binding.UserID, "node_id", binding.NodeID,
					"role_id", binding.RoleID, "error", err)
				continue
			}
			resolvedRoles[binding.RoleID] = role
		}

		if err := s.bindings.Restore(ctx, binding.ID); err != nil {
			if hasCode(err, dbkit.ErrRecordNotFound.Code) || errors.Is(err, gorm.ErrDuplicatedKey) {
				continue
			}
			log.Warn("rbac could not restore a reinstated role binding",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
			continue
		}
		sub := Subject{TenantID: evt.TenantID, UserID: binding.UserID}
		if err := s.publishBindingChanged(ctx, EventRoleBindingRestored, sub, role, Scope{NodeID: binding.NodeID}); err != nil {
			log.Warn("rbac could not announce a reinstated binding's restore",
				"event_type", evt.Type, "node_id", binding.NodeID, "user_id", binding.UserID,
				"role", role.Key, "error", err)
		}
	}
}

// bindingNodeLivesAtMemberRestore is the member-restored re-instatement's
// structural-precondition gate, the fix for the P1-rbac-reinstate-node
// finding: a revoked row that the member-removal reap wrote or claimed is
// un-marked by the member's own org.member.restored event only when the
// OTHER fact the row's grant depends on -- the node it is scoped to --
// still exists in the org tree, re-verified here through the host's
// SubtreeResolver (the module's one permitted window onto the tree; it
// never imports org). The event asserts the membership; it says nothing
// about the node, and Can, the coarse gate, never consults node liveness
// at decision time (only DataScope's row-level narrowing does, and a
// reinstated row must not rely on that: the deny-then-narrow layering
// only tolerates a dangling LIVE binding; a re-instatement that re-opened
// a revoked one at a dead node would hand Can an allowed answer for the
// full node-deleted window). Every member-removal row scoped to a node
// passes through here before any role resolution in
// reinstateReapedBindings; the node-restored path needs no equivalent
// gate, because its own event asserts the node's visibility.
//
// Three outcomes:
//
//  1. The node resolves (ok=true and a non-empty path): the row's two
//     preconditions hold -- true.
//
//  2. The node does not resolve (ok=false, or an empty path): the row is
//     re-attributed to the node-deletion origin
//     (RoleBindingRepository.reattributeToNodeDeletion), the mirror
//     inverse of the removal claim, and left revoked -- so a later
//     org.node.restored re-instates it exactly like every other grant the
//     deletion reaped, and a node that never returns keeps it revoked.
//     Logged at Warn: a restored member whose grant stays down while her
//     node is gone is exactly the surprise an operator will search logs
//     for, and the re-attribution is the state change that explains it.
//
//  3. No SubtreeResolver is wired, or the resolver errors: the answer is
//     unknown, and the re-instatement fails closed -- the row stays
//     revoked under the member-removal origin, never re-attributed
//     (without an answer the code must not presume the node dead and hand
//     the row to an event that may never fire). Nothing re-checks it on
//     its own: neither bus redelivers the restore event (see reap_jobs.go's
//     header comment), so the re-check happens on the member's NEXT
//     removal-and-restore cycle, whose re-instatement pass runs this same
//     gate again -- or on the node's own deletion-and-restore cycle if the
//     row is re-claimed meanwhile. Logged at Warn with the reason.
//
// ctx is the event-tenant context onMemberRestored rebuilt, so the
// resolver is asked under the tenant whose bindings are being
// re-instated, exactly as the repository calls around it run.
func (s *Service) bindingNodeLivesAtMemberRestore(ctx context.Context, log *slog.Logger, evt pkgcore.Event, binding RoleBinding) bool {
	nodeID := binding.NodeID
	if s.subtree == nil {
		log.Warn("rbac could not verify a restored member's binding node: no SubtreeResolver is wired; leaving the binding revoked",
			"event_type", evt.Type, "user_id", binding.UserID, "node_id", nodeID, "role_id", binding.RoleID)
		return false
	}

	nodePath, ok, err := s.subtree.NodePath(ctx, nodeID)
	if err != nil {
		log.Warn("rbac could not verify a restored member's binding node through the SubtreeResolver; leaving the binding revoked",
			"event_type", evt.Type, "user_id", binding.UserID, "node_id", nodeID, "role_id", binding.RoleID, "error", err)
		return false
	}
	if ok && nodePath != "" {
		return true
	}

	if changed, err := s.bindings.reattributeToNodeDeletion(ctx, binding.ID); err != nil {
		log.Warn("rbac could not re-attribute a restored member's binding to the node-deletion origin",
			"event_type", evt.Type, "user_id", binding.UserID, "node_id", nodeID, "role_id", binding.RoleID, "error", err)
		return false
	} else if !changed {
		// A concurrent writer moved the row -- restored it, re-claimed it,
		// or re-attributed it -- between the enumeration and this write.
		// The desired end state is that writer's to describe; the
		// classified-silent treatment mirrors Restore's own race
		// classification.
		return false
	}
	log.Warn("rbac left a restored member's binding revoked: its node does not resolve; re-attributed to the node-deletion origin, awaiting the node's own restore",
		"event_type", evt.Type, "user_id", binding.UserID, "node_id", nodeID, "role_id", binding.RoleID)
	return false
}
