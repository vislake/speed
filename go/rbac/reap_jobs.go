package rbac

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is rbac's queue-backed reaping (P1-rbac-reap): the revoke-side
// org-event subscribers -- onMemberRemoved for org.member.removed,
// onNodeDeleted for org.node.deleted -- enqueue a reap task on the host's
// jobs.Queue (Module.WithQueue) instead of running the reaping
// synchronously inside the event delivery, and these tasks are what this
// module's own workers execute. The jobs queue is the retry home the
// depended-on side of the old design never had: go/pkgcore's bus contract
// (eventbus/redis/eventbus.go and the in-memory bus alike) states that the
// bus never redelivers and handler errors never come back to the
// publisher, so a reap that hit a transient database error mid-pass was
// simply over -- every binding after the failure stayed live forever, and
// a member later re-joined through a FRESH membership (org fires no
// restore event for that path) silently kept a role the removal should
// have ended. A task the queue retries until it succeeds is the honest
// convergence for that failure shape.
//
// Idempotency discipline: each task's payload is the event instance's own
// identity -- the removal's (tenant, user, membership id) or the
// deletion's (tenant, node id set) -- and that identity is also the
// task's deterministic idempotency key, so two deliveries of one event
// (which neither bus produces today, but which an at-least-once bus would)
// collapse into one job. The reap itself is idempotent under re-runs for
// the same reason the synchronous one was: its enumerations return live
// rows only, so a retried task finds the bindings its first attempt
// already revoked gone from the enumeration. A membership that is removed,
// restored and removed again is a NEW event instance each time (a new
// membership id per removal), so its reap task is never collapsed into an
// earlier one.
//
// Ordering note, recorded rather than papered over: the queue cannot
// promise that a removal reap runs before a later org.member.restored
// reinstate, the way the synchronous subscriber's in-event-order
// execution did. The residual window is a restore processed while its
// removal's reap task is still queued or running -- sub-worker-latency
// automation, never an operator's restore -- and the wrong end state it
// produces is a revoked binding for a member org made visible again
// again: fail-closed (missing access, never unauthorized access), and
// healed by the member's next removal-and-restore cycle, whose reinstate
// pass re-lifts every member-removal row. That is the direction this
// module accepts residual risk in; the leak direction the reaps exist to
// close is the one the queue's retries now converge. The restore-side
// subscribers (onMemberRestored, onNodeRestored) deliberately stay
// synchronous for the same reason: their re-instatements must never race
// their matching reaps.

// Task types rbac registers on reg.Jobs (Attach) and enqueues (reap.go's
// subscribers). They follow the "<module>.<entity>.<action>" naming the
// module's events use.
const (
	// taskTypeReapMember is the task revoking a removed member's role
	// bindings (the member-removal reap).
	taskTypeReapMember = "rbac.member.reap"

	// taskTypeReapNode is the task revoking every binding scoped to a
	// deleted org node (the node-deletion reap, cascade included).
	taskTypeReapNode = "rbac.node.reap"
)

// reapMemberPayload is the payload of taskTypeReapMember tasks: the
// removal instance's own identity, captured from the org.member.removed
// event at enqueue time. The tenant travels in the task's TenantID, never
// here (jobs.Task's own field), and the reaping itself re-enumerates live
// bindings at run time -- the payload is an address, not a row set, so a
// task retried after an interruption converges on whatever the removal
// still leaves live.
type reapMemberPayload struct {
	// UserID is the removed member's user id.
	UserID string `json:"user_id"`

	// MembershipID is the removed membership's id (org's own
	// MemberRemoved.MembershipID), the per-removal-instance identity that
	// keeps one user's successive removals from collapsing into one task.
	// Empty only for a payload org never sent that way.
	MembershipID string `json:"membership_id"`
}

// reapNodePayload is the payload of taskTypeReapNode tasks: the deletion
// instance's own identity, the node id set captured from the
// org.node.deleted event at enqueue time (cascade included -- a cascade's
// whole set rides in one task).
type reapNodePayload struct {
	// NodeIDs is the deleted node id set, sorted at enqueue time so the
	// task's idempotency key is order-independent.
	NodeIDs []string `json:"deleted_node_ids"`
}

// memberReapKey is the deterministic idempotency key of a member-removal
// reap task: the removal instance's (tenant, membership) identity. The
// membership id is what distinguishes one removal from the next removal of
// the same user; the user id is the fallback for an event whose payload
// carried no membership id (an org version before MemberRemoved carried
// one), where the instance identity degrades to the user -- the same
// collapse the old redelivery assumption would have had.
func memberReapKey(tenant pkgcore.TenantID, userID, membershipID string) string {
	instance := membershipID
	if instance == "" {
		instance = userID
	}
	return "rbac-member-reap:" + string(tenant) + ":" + instance
}

// nodeReapKey is the deterministic idempotency key of a node-deletion reap
// task: the deletion instance's (tenant, node id set) identity. Node ids
// are application-generated and never reused, so the set names the
// deletion.
func nodeReapKey(tenant pkgcore.TenantID, nodeIDs []string) string {
	return "rbac-node-reap:" + string(tenant) + ":" + strings.Join(nodeIDs, ",")
}

// enqueueMemberReap enqueues the member-removal reap task for the removal
// instance (tenant, userID, membershipID) the event named. It is the
// queue-backed replacement for running reapRoleBindings synchronously
// inside the event delivery; see this file's header comment for the full
// shape. Returns the queue's error unwrapped, for the caller (the
// subscriber) to log -- a subscriber must never return it, since on the
// in-memory bus an error here would surface inside org's committed Remove
// call.
func (s *Service) enqueueMemberReap(ctx context.Context, tenant pkgcore.TenantID, userID, membershipID string) error {
	payload, err := json.Marshal(reapMemberPayload{UserID: userID, MembershipID: membershipID})
	if err != nil {
		return err
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeReapMember,
		TenantID:       tenant,
		Payload:        payload,
		IdempotencyKey: memberReapKey(tenant, userID, membershipID),
	})
	return err
}

// enqueueNodeReap enqueues the node-deletion reap task for the deletion
// instance (tenant, nodeIDs) the event named -- enqueueNodeReap's
// counterpart for the other revoke-side reap; see that method's doc
// comment for the caller contract.
func (s *Service) enqueueNodeReap(ctx context.Context, tenant pkgcore.TenantID, nodeIDs []string) error {
	sorted := append([]string(nil), nodeIDs...)
	sort.Strings(sorted)
	payload, err := json.Marshal(reapNodePayload{NodeIDs: sorted})
	if err != nil {
		return err
	}
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeReapNode,
		TenantID:       tenant,
		Payload:        payload,
		IdempotencyKey: nodeReapKey(tenant, sorted),
	})
	return err
}

// memberReapTask is the jobs.Handler for taskTypeReapMember: it runs the
// member-removal reap -- the claim plus the live-binding revocation
// reapRoleBindings performs -- inside the queue, where a transient failure
// is retried instead of abandoned. ctx carries the job's tenant, rebuilt
// by the worker from the task (jobs.Handler's own contract), so every
// repository call runs under the tenant whose member was removed, exactly
// as the synchronous subscriber's rebuilt context did.
type memberReapTask struct {
	svc *Service
}

// Type implements jobs.Handler.
func (t memberReapTask) Type() string { return taskTypeReapMember }

// Handle implements jobs.Handler.
func (t memberReapTask) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var payload reapMemberPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("rbac: %s task carries an unreadable payload: %w", taskTypeReapMember, err)
	}
	if payload.UserID == "" {
		return jobs.Result{}, fmt.Errorf("rbac: %s task carries no user id", taskTypeReapMember)
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return jobs.Result{}, fmt.Errorf("rbac: %s task ran without a tenant context", taskTypeReapMember)
	}
	// A synthetic event stands in for the org event the synchronous path
	// received: the reaping code reads only the tenant and the type (for
	// logs and for the Subject of the revoked-binding announcements), and
	// both are the task's own.
	evt := pkgcore.Event{Type: taskTypeReapMember, TenantID: tenant}
	if err := t.svc.reapRoleBindings(ctx, evt, payload.UserID); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// OnFailure implements jobs.FailureHook: the queue exhausted this task's
// retries, so the removal's reaping has a permanent residue. The retries
// already logged each failure through the queue's own machinery; this is
// the one final alert naming what will stay live -- a binding the removal
// should have ended -- until an operator intervenes.
func (t memberReapTask) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
	obs.FromContext(ctx).Error("rbac member-removal reap exhausted its retries; bindings of the removed member may remain live",
		"job_id", string(job.ID), "error", cause)
}

// nodeReapTask is the jobs.Handler for taskTypeReapNode: it runs the
// node-deletion reap (reapRoleBindingsForNodes) inside the queue, with
// memberReapTask's retry contract.
type nodeReapTask struct {
	svc *Service
}

// Type implements jobs.Handler.
func (t nodeReapTask) Type() string { return taskTypeReapNode }

// Handle implements jobs.Handler.
func (t nodeReapTask) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var payload reapNodePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("rbac: %s task carries an unreadable payload: %w", taskTypeReapNode, err)
	}
	if len(payload.NodeIDs) == 0 {
		return jobs.Result{}, fmt.Errorf("rbac: %s task carries no node ids", taskTypeReapNode)
	}
	tenant, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return jobs.Result{}, fmt.Errorf("rbac: %s task ran without a tenant context", taskTypeReapNode)
	}
	evt := pkgcore.Event{Type: taskTypeReapNode, TenantID: tenant}
	if err := t.svc.reapRoleBindingsForNodes(ctx, evt, payload.NodeIDs); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// OnFailure implements jobs.FailureHook; see memberReapTask.OnFailure.
func (t nodeReapTask) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
	obs.FromContext(ctx).Error("rbac node-deletion reap exhausted its retries; bindings scoped to the deleted nodes may remain live",
		"job_id", string(job.ID), "error", cause)
}

// compile-time checks that both tasks satisfy jobs.Handler and
// jobs.FailureHook.
var (
	_ jobs.Handler     = memberReapTask{}
	_ jobs.FailureHook = memberReapTask{}
	_ jobs.Handler     = nodeReapTask{}
	_ jobs.FailureHook = nodeReapTask{}
)
