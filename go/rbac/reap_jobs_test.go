package rbac

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the queue-backed reaping (P1-rbac-reap): with a jobs
// queue wired through Module.WithQueue, an org.member.removed or
// org.node.deleted event is ENQUEUED as a reap task instead of being run
// synchronously inside the event delivery, and the task -- retried by the
// queue when it fails -- is what actually withdraws the bindings. Before
// this round the reaping lived entirely inside the event handler with no
// retry home at all (neither published bus redelivers), so a removal whose
// reap hit a transient failure left the removed member's bindings live
// forever, and a member who re-joined through a fresh membership silently
// kept a role the removal should have ended.
//
// The worker-based tests below start a real jobs.StandaloneQueue over the
// same SQLite file the Service's own tables live in -- the queue's Start
// creates its own schema there -- and drive the real composed flow:
// publish the org event on the registry's bus, the subscriber enqueues,
// the queue's worker executes the task. Waiting is bounded by a deadline
// loop over the observable end state (the binding rows), never by a fixed
// sleep.

// waitFor polls cond until it reports true or deadline passes. A bounded
// deadline loop is the deterministic core of every worker test here: the
// queue's poll interval is a millisecond, so a condition the mechanism
// delivers is reached in milliseconds, and one the mechanism does not
// deliver fails at the deadline with the last observed state.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startQueue starts q and arranges its shutdown at test end.
func startQueue(t *testing.T, q *jobs.StandaloneQueue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("starting the queue: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := q.Close(stopCtx); err != nil {
			t.Errorf("closing the queue: %v", err)
		}
	})
}

// liveBindings returns the user's still-live bindings in tenant.
func liveBindings(t *testing.T, svc *Service, tenant pkgcore.TenantID, userID string) []RoleBinding {
	t.Helper()
	rows, err := svc.bindings.ByUser(tenantCtx(tenant), userID)
	if err != nil {
		t.Fatalf("listing %s's bindings: %v", userID, err)
	}
	return rows
}

// jobRows returns the rows of the given task type from the queue's own
// table, the queue being over the same database the Service uses. It is
// how a test counts enqueues without racing the worker.
func jobRows(t *testing.T, db *gorm.DB, jobType string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := db.Table("jobs").Where("type = ?", jobType).Find(&rows).Error; err != nil {
		t.Fatalf("reading the queue's %s rows: %v", jobType, err)
	}
	return rows
}

// newQueueTestService attaches a Service with a queue wired over db, in
// the host's real order: build the queue first, construct the module with
// WithQueue, let Attach install the reap-task handlers on the registry,
// then drain them onto the queue.
func newQueueTestService(t *testing.T, db *gorm.DB, opts ...Option) (*Service, *pkgcore.Registry, *jobs.StandaloneQueue) {
	t.Helper()
	q := jobs.NewStandaloneQueue(db,
		jobs.WithWorkerCount(1),
		jobs.WithPollInterval(time.Millisecond))
	svc, reg := attachTestService(t, db, append(opts, WithQueue(q))...)
	for jobType, handler := range reg.Jobs.Handlers() {
		jobsHandler, ok := handler.(jobs.Handler)
		if !ok {
			t.Fatalf("registry job handler %q is not a jobs.Handler", jobType)
		}
		if err := q.RegisterHandler(jobsHandler); err != nil {
			t.Fatalf("registering %s on the queue: %v", jobType, err)
		}
	}
	return svc, reg, q
}

func TestService_MemberRemovalReap_EnqueuesAReapTaskThatTheWorkerExecutes(t *testing.T) {
	db := newRBACTestDB(t)
	svc, reg, q := newQueueTestService(t, db)
	startQueue(t, q)

	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, removed, "reader", Scope{}, "notes:read")
	grant(t, svc, removed, "writer", Scope{NodeID: "node-1"}, "notes:write")

	if ok, _ := svc.Can(context.Background(), removed, "read", "notes"); !ok {
		t.Fatal("the grant was not live before the removal")
	}

	rec := recordEvents(reg)
	publishMemberRemoved(t, reg, removed.TenantID, removedMember{
		MembershipID: "membership-1",
		UserID:       removed.UserID,
	})

	// The subscriber must only have enqueued: at the moment Publish
	// returns, the synchronous half of the delivery is over and the
	// binding rows are untouched -- the reaping happens when the worker
	// picks the task up.
	if rows := liveBindings(t, svc, removed.TenantID, removed.UserID); len(rows) != 2 {
		t.Fatalf("bindings changed before the queue ran: %d live rows, want 2", len(rows))
	}
	enqueued := jobRows(t, db, taskTypeReapMember)
	if len(enqueued) != 1 {
		t.Fatalf("got %d enqueued %s tasks, want 1", len(enqueued), taskTypeReapMember)
	}

	// The worker converges the reap: both bindings withdrawn, each
	// announcing EventRoleBindingRevoked, the cache flipped.
	waitFor(t, "the reap task to revoke the removed member's bindings", func() bool {
		return len(liveBindings(t, svc, removed.TenantID, removed.UserID)) == 0
	})
	if ok, err := svc.Can(context.Background(), removed, "read", "notes"); err != nil || ok {
		t.Fatalf("Can after the queued reap = %v, %v; want false", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (one per reaped binding)", got, EventRoleBindingRevoked)
	}

	// Remove-then-rejoin: the same user re-joining through a FRESH
	// membership (org fires no restore event for that path) must not
	// silently regain the role the removal ended -- the row is revoked,
	// and nothing in the module resurrects it. The rejoin is modelled as
	// org would fire it: no event rbac listens to, and the pre-removal
	// grant stays gone until an administrator assigns it anew.
	if rows := liveBindings(t, svc, removed.TenantID, removed.UserID); len(rows) != 0 {
		t.Fatalf("the re-joined member holds %d live bindings, want 0 -- the removal's reap did not stick", len(rows))
	}
}

func TestService_NodeDeletionReap_EnqueuesAReapTaskThatTheWorkerExecutes(t *testing.T) {
	// The node-deletion reap's leg of the same queue delivery: an
	// org.node.deleted event (a cascade, carrying two node ids) enqueues
	// one node-reap task, and the worker revokes every binding scoped to
	// either deleted node.
	db := newRBACTestDB(t)
	svc, reg, q := newQueueTestService(t, db)
	startQueue(t, q)

	holder := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, holder, "reader", Scope{NodeID: "node-1"}, "notes:read")
	grant(t, svc, holder, "editor", Scope{NodeID: "node-2"}, "notes:write")
	grant(t, svc, Subject{TenantID: "tenant-a", UserID: "user-2"}, "reader", Scope{NodeID: "node-9"}, "notes:read")

	ctx := context.Background()
	rec := recordEvents(reg)
	bus := reg.Events.Bus()
	if err := bus.Publish(pkgcore.WithTenant(ctx, "tenant-a"), pkgcore.Event{
		Type:     eventNodeDeleted,
		TenantID: "tenant-a",
		Payload:  nodeDeleted{DeletedNodeIds: []string{"node-1", "node-2"}},
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventNodeDeleted, err)
	}

	enqueued := jobRows(t, db, taskTypeReapNode)
	if len(enqueued) != 1 {
		t.Fatalf("got %d enqueued %s tasks, want 1 (one task for the whole cascade)", len(enqueued), taskTypeReapNode)
	}

	waitFor(t, "the node reap to revoke the bindings at the deleted nodes", func() bool {
		return len(liveBindings(t, svc, "tenant-a", "user-1")) == 0
	})
	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (one per reaped binding)", got, EventRoleBindingRevoked)
	}
	// The binding at the surviving node is untouched.
	if rows := liveBindings(t, svc, "tenant-a", "user-2"); len(rows) != 1 {
		t.Fatalf("the surviving node's binding was reaped: %d live rows, want 1", len(rows))
	}
}

func TestMemberReapTask_TransientFailureFailsTheAttempt_AndARetryConverges(t *testing.T) {
	// The queue-retry property, proven at the task's own boundary: a task
	// attempt that hits a transient database failure returns an error --
	// which is what makes the queue schedule the next attempt -- and a
	// retried attempt converges the revocation. The transient failure is
	// the binding table being momentarily unavailable (renamed and
	// renamed back around the first attempt), which fails the task's
	// enumeration deterministically and heals before the second attempt.
	db := newRBACTestDB(t)
	svc, _ := attachTestService(t, db)
	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, removed, "reader", Scope{}, "notes:read")

	payload, err := json.Marshal(reapMemberPayload{UserID: removed.UserID, MembershipID: "membership-1"})
	if err != nil {
		t.Fatalf("marshaling the reap payload: %v", err)
	}
	job := &jobs.Job{
		ID:       jobs.JobID("member-reap-job"),
		Type:     taskTypeReapMember,
		TenantID: removed.TenantID,
		Payload:  payload,
	}
	task := memberReapTask{svc: svc}
	ctx := tenantCtx(removed.TenantID)

	if err := db.Exec("ALTER TABLE rbac_role_bindings RENAME TO rbac_role_bindings_hidden").Error; err != nil {
		t.Fatalf("hiding the bindings table: %v", err)
	}
	if _, err := task.Handle(ctx, job, nil); err == nil {
		t.Fatal("the task succeeded against an unavailable bindings table -- the queue would never retry a failed reap")
	}
	if err := db.Exec("ALTER TABLE rbac_role_bindings_hidden RENAME TO rbac_role_bindings").Error; err != nil {
		t.Fatalf("restoring the bindings table: %v", err)
	}

	if _, err := task.Handle(ctx, job, nil); err != nil {
		t.Fatalf("the retried task failed: %v", err)
	}
	if rows := liveBindings(t, svc, removed.TenantID, removed.UserID); len(rows) != 0 {
		t.Fatalf("the retried reap left %d live bindings, want 0", len(rows))
	}
	if ok, _ := svc.Can(context.Background(), removed, "read", "notes"); ok {
		t.Fatal("Can stayed true after the retried reap")
	}
}

func TestService_MemberRemovalReap_TransientFailureNearTheEnqueue_Converges(t *testing.T) {
	// The P1-rbac-reap harm shape end to end: a transient database failure
	// at the moment of the removal -- the binding table momentarily
	// unavailable -- and the reaping still completes. The removal event
	// arrives while the bindings table is hidden, so every attempt the
	// reaping makes while the failure holds fails; the queue retries the
	// task until one attempt lands after the table is back, and the revoke
	// the removal demands converges. Pre-this-round, the same sequence --
	// synchronous best-effort reaping inside the delivery, no queue to
	// retry -- left the binding live forever, and a member who re-joined
	// through a fresh membership silently kept the role (the recorded
	// pre-fix run of this scenario is in the round's notes).
	db := newRBACTestDB(t)
	svc, reg, q := newQueueTestService(t, db)
	startQueue(t, q)

	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, removed, "reader", Scope{}, "notes:read")

	if err := db.Exec("ALTER TABLE rbac_role_bindings RENAME TO rbac_role_bindings_hidden").Error; err != nil {
		t.Fatalf("hiding the bindings table: %v", err)
	}
	publishMemberRemoved(t, reg, removed.TenantID, removedMember{
		MembershipID: "membership-1",
		UserID:       removed.UserID,
	})
	if err := db.Exec("ALTER TABLE rbac_role_bindings_hidden RENAME TO rbac_role_bindings").Error; err != nil {
		t.Fatalf("restoring the bindings table: %v", err)
	}

	waitFor(t, "the queued reap to revoke the binding once the table is back", func() bool {
		return len(liveBindings(t, svc, removed.TenantID, removed.UserID)) == 0
	})
	if ok, _ := svc.Can(context.Background(), removed, "read", "notes"); ok {
		t.Fatal("Can stayed true -- the reaping did not converge")
	}
}

func TestService_MemberRemovalReap_SameRemovalTwice_EnqueuesOneTask(t *testing.T) {
	// The idempotency discipline: two deliveries of one removal instance
	// (the same membership id -- an at-least-once bus's duplicate, which
	// neither published bus produces today but the key exists for) enqueue
	// ONE task, whose own re-run is a no-op. A second removal instance
	// (a new membership id for the same user) is a different task: the
	// removal-to-reap mapping is per instance, never collapsed onto the
	// user.
	db := newRBACTestDB(t)
	svc, reg, q := newQueueTestService(t, db)
	startQueue(t, q) // the queue's own table only exists once Start has run

	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, removed, "reader", Scope{}, "notes:read")

	publishMemberRemoved(t, reg, removed.TenantID, removedMember{MembershipID: "membership-1", UserID: removed.UserID})
	publishMemberRemoved(t, reg, removed.TenantID, removedMember{MembershipID: "membership-1", UserID: removed.UserID})
	if got := len(jobRows(t, db, taskTypeReapMember)); got != 1 {
		t.Fatalf("two deliveries of one removal enqueued %d tasks, want 1 (idempotency key = the removal instance)", got)
	}

	// The user's SECOND removal (a fresh membership) must not collapse
	// into the first task -- its reap would never run.
	publishMemberRemoved(t, reg, removed.TenantID, removedMember{MembershipID: "membership-2", UserID: removed.UserID})
	if got := len(jobRows(t, db, taskTypeReapMember)); got != 2 {
		t.Fatalf("two removal instances enqueued %d tasks, want 2 (the idempotency key names the instance, not the user)", got)
	}
}

func TestMemberReapTask_UnreadablePayload_FailsTheAttempt(t *testing.T) {
	// A task whose payload cannot be read must fail the attempt (the queue
	// retries, then dead-letters with the alert) rather than silently
	// succeeding with the removal's bindings untouched.
	svc, _ := attachTestService(t, newRBACTestDB(t))
	ctx := tenantCtx("tenant-a")

	if _, err := (memberReapTask{svc: svc}).Handle(ctx, &jobs.Job{
		ID: jobs.JobID("j"), Type: taskTypeReapMember, TenantID: "tenant-a", Payload: []byte("not json"),
	}, nil); err == nil {
		t.Fatal("an unreadable payload succeeded")
	}
	if _, err := (nodeReapTask{svc: svc}).Handle(ctx, &jobs.Job{
		ID: jobs.JobID("j"), Type: taskTypeReapNode, TenantID: "tenant-a", Payload: []byte(`{"deleted_node_ids":[]}`),
	}, nil); err == nil {
		t.Fatal("an empty node id set succeeded")
	}
}

// nodeDeleted mirrors org's NodeDeleted payload shape on the wire, exactly
// like removedMember does for MemberRemoved (reap_test.go).
type nodeDeleted struct {
	DeletedNodeIds []string `json:"deleted_node_ids"`
}
