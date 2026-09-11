package rbac

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// This file pins the queue-backed reaping: with a jobs
// queue wired through Module.WithQueue, an org.member.removed or
// org.node.deleted event is ENQUEUED as a reap task instead of being run
// synchronously inside the event delivery, and the task -- retried by the
// queue when it fails -- is what actually withdraws the bindings. The
// queue is the retry home the reaps need: neither published bus
// redelivers, so a removal whose
// reap hits a transient failure would leave the removed member's bindings
// live forever, and a member who re-joins through a fresh membership
// would silently keep a role the removal should have ended.
//
// The worker-based tests below start a real jobs.StandaloneQueue over the
// same SQLite file the Service's own tables live in -- jobs.Wire creates
// the queue's own schema there, and each test's own startQueue launches
// the workers when it wants them -- and drive the real composed flow:
// publish the org event on the registry's bus, the subscriber enqueues,
// the queue's worker executes the task. Waiting is bounded by a deadline
// loop over the observable end state (the binding rows), never by a fixed
// sleep.
//
// Determinism, in three layers, is what keeps these tests from flaking
// under CI scheduling load. Three CI recurrences failed the same three
// tests, and the two timing dependencies share one enabling condition --
// several goroutines (the queue's dispatcher, heartbeat and worker,
// ticking or executing around the test's own reads) on one SQLite file
// through a multi-connection pool. CI's recorded signature was SQLITE_BUSY
// "database is locked" errors at the test's own database calls (grants)
// and in the queue's dispatcher polls; a local reproduction under load
// caught the family's other member, the worker reaping one binding
// between the publish and the enqueued-state observation ("bindings
// changed before the queue ran: 1 live rows, want 2").
//
//   - The test database is pinned to ONE pool connection
//     (newQueueTestService). SQLite's lock protocol can refuse a
//     read-then-write upgrade with an immediate SQLITE_BUSY that no
//     busy_timeout cures (dbkit's own dialect doc says so), and the
//     queue's dispatcher and heartbeat tick every millisecond, so a
//     multi-connection pool hands the test goroutine and the queue's
//     goroutines conflicting locks with real probability under load. One
//     connection serializes every statement inside database/sql's pool,
//     where waiting is an ordinary queue: no two connections can ever
//     hold conflicting locks, and the pass/fail decision no longer
//     depends on lock timing. The mechanism under test -- enqueue,
//     worker execution, retry convergence -- is connection-count
//     agnostic.
//
//   - The "the subscriber only enqueued" observation in the two worker
//     tests happens while NO worker exists: the queue's Start is deferred
//     until after the task row and the untouched binding rows are
//     asserted (the schema is materialized first by a throwaway
//     Start/Close, since Enqueue needs the jobs table to exist). An
//     observation of a transient state races whatever could change it; a
//     worker that cannot exist yet makes the observation unconditional.
//
//   - Post-convergence announcements (the revoked-binding events, the
//     cache flip that makes Can answer false) are observed by waiting
//     for the durable end state, never read once at an instant: each
//     revoke's event fires after its row's mark-delete commits, so the
//     test waits for the event count before asserting anything the
//     announcement ordering guarantees (the event recorder carries a
//     mutex for the same reason -- the worker goroutine appends to it
//     while the test polls it).
//
// One more property is pinned here: the enqueue itself has no retry home
// -- a reap task that never lands is a task no retry can converge (the
// bus never redelivers and the subscribers must still return nil), so an
// Enqueue failure falls back to the synchronous reaping. The two
// EnqueueFailure tests below wire a queue whose Enqueue always fails and
// assert the bindings are still reaped.

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

// newQueueTestService attaches a Service with a queue wired over db, in
// the host's real order: build the queue first, construct the module with
// WithQueue, let Attach install the reap-task handlers on the registry,
// then jobs.Wire them onto the queue -- which also creates the queue's
// schema, so a test may Enqueue before any worker exists and observe the
// enqueued-but-not-yet-executed state before its own startQueue call.
//
// The queue's Start is deliberately NOT called here -- each test decides
// when a worker may exist. The worker tests that observe the transient
// enqueued state start it after the observation (see their comments);
// the tests that need the worker live from publish time on call
// startQueue themselves.
//
// Two construction details serve determinism rather than speed:
//
//   - db's pool is pinned to a single connection. The queue's dispatcher
//     and heartbeat tick every millisecond and the worker runs the reap
//     concurrently with the test's own assertions, all through the one
//     pool dbkit.Open handed out -- and on one SQLite file, a
//     multi-connection pool makes SQLITE_BUSY reachable under scheduling
//     load: SQLite answers a read-then-write upgrade with an immediate
//     "database is locked" that no busy_timeout cures (dbkit's own
//     dialect doc names the class), and the busy timeouts themselves
//     expire when the machine is saturated. Three CI recurrences of these
//     tests failed exactly that way, at the test's own grants and at the
//     dispatcher's polls. One connection serializes every statement
//     inside the pool -- waiting moves into database/sql, where it is an
//     ordinary queue and can never surface as a lock error -- and the
//     mechanism under test does not care how many connections carried it.
//
//   - The retry backoff is pinned to jobs' own defaults. The
//     TransientFailureNearTheEnqueue test's convergence budget is a
//     function of the pacing of the queue's retries (a failed attempt is
//     retried after the backoff base), so the pacing these tests rely on
//     is stated here rather than inherited from a default that could
//     drift.
func newQueueTestService(t *testing.T, db *gorm.DB, opts ...Option) (*Service, *pkgcore.ComponentRegistry, *jobs.StandaloneQueue) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("reaching the database's pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	q := jobs.NewStandaloneQueue(db,
		jobs.WithWorkerCount(1),
		jobs.WithPollInterval(time.Millisecond),
		jobs.WithBackoff(jobs.DefaultBackoffBase, jobs.DefaultBackoffMax))
	svc, reg := attachTestService(t, db, append(opts, WithQueue(q))...)
	if err := jobs.Wire(context.Background(), q, reg.Jobs); err != nil {
		t.Fatalf("wiring the registry's handlers onto the queue: %v", err)
	}
	return svc, reg, q
}

// liveBindings returns the user's still-live bindings in tenant.
func liveBindings(t *testing.T, svc *Service, tenant pkgcore.TenantID, userID string) []RoleBinding {
	t.Helper()
	rows, err := svc.bindings.ByUser(testkit.TenantCtx(tenant), userID)
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

func TestService_MemberRemovalReap_EnqueuesAReapTaskThatTheWorkerExecutes(t *testing.T) {
	db := newRBACTestDB(t)
	svc, reg, q := newQueueTestService(t, db)

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
	// returns, the binding rows are untouched and the task row sits
	// Pending -- the reaping happens when the worker picks the task up.
	// The queue's Start is deferred until after this observation, so no
	// worker exists to race it: a worker polling the same table could
	// otherwise reap between these two reads and make the transient
	// state unobservable, a scheduling race rather than an assertion.
	if rows := liveBindings(t, svc, removed.TenantID, removed.UserID); len(rows) != 2 {
		t.Fatalf("bindings changed before the queue ran: %d live rows, want 2", len(rows))
	}
	enqueued := jobRows(t, db, taskTypeReapMember)
	if len(enqueued) != 1 {
		t.Fatalf("got %d enqueued %s tasks, want 1", len(enqueued), taskTypeReapMember)
	}
	if status := enqueued[0]["status"]; status != string(jobs.StatusPending) {
		t.Fatalf("the enqueued %s task's status = %v, want %s -- the subscriber must leave the reaping to the worker, not run it",
			taskTypeReapMember, status, jobs.StatusPending)
	}

	startQueue(t, q)

	// The worker converges the reap: both bindings withdrawn, each
	// announcing EventRoleBindingRevoked, the cache flipped.
	testkit.Eventually(t, "the reap task to revoke the removed member's bindings", func() bool {
		return len(liveBindings(t, svc, removed.TenantID, removed.UserID)) == 0
	})
	// Each revoked-binding event fires strictly after its row's
	// mark-delete commit (and after the cache flip the same announce
	// performs), so waiting for both announcements -- rather than reading
	// the recorder once, an instant that could race the worker's last
	// announce -- is what makes the Can and row assertions below
	// unconditional.
	testkit.Eventually(t, "both reaped bindings to announce their revocation", func() bool {
		return len(rec.OfType(EventRoleBindingRevoked)) == 2
	})
	if ok, err := svc.Can(context.Background(), removed, "read", "notes"); err != nil || ok {
		t.Fatalf("Can after the queued reap = %v, %v; want false", ok, err)
	}
	if got := len(rec.OfType(EventRoleBindingRevoked)); got != 2 {
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

	// The subscriber must only have enqueued -- the queue's Start is
	// deferred until after this observation (test 1's own comment gives
	// the determinism reasoning), so the single task row sits Pending
	// with the bindings it will reap still live.
	enqueued := jobRows(t, db, taskTypeReapNode)
	if len(enqueued) != 1 {
		t.Fatalf("got %d enqueued %s tasks, want 1 (one task for the whole cascade)", len(enqueued), taskTypeReapNode)
	}
	if status := enqueued[0]["status"]; status != string(jobs.StatusPending) {
		t.Fatalf("the enqueued %s task's status = %v, want %s -- the subscriber must leave the reaping to the worker, not run it",
			taskTypeReapNode, status, jobs.StatusPending)
	}

	startQueue(t, q)

	testkit.Eventually(t, "the node reap to revoke the bindings at the deleted nodes", func() bool {
		return len(liveBindings(t, svc, "tenant-a", "user-1")) == 0
	})
	// Same announcement-ordering wait as test 1: each revoked-binding
	// event fires after its row's mark-delete commit.
	testkit.Eventually(t, "both reaped bindings to announce their revocation", func() bool {
		return len(rec.OfType(EventRoleBindingRevoked)) == 2
	})
	if got := len(rec.OfType(EventRoleBindingRevoked)); got != 2 {
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
	ctx := testkit.TenantCtx(removed.TenantID)

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
	// The transient-failure harm shape end to end: a transient database failure
	// at the moment of the removal -- the binding table momentarily
	// unavailable -- and the reaping still completes. The removal event
	// arrives while the bindings table is hidden; the table is restored
	// only once the task row reports a FAILED attempt (status retrying),
	// so the failure the queue must converge is genuinely exercised on
	// every run rather than whenever a worker poll happened to land in
	// the hidden window; the queue retries the task until one attempt
	// lands after the table is back, and the revoke the removal demands
	// converges.
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

	// The table is healed only after an attempt has genuinely failed
	// against it: the row's transition to retrying is completeRetrying's
	// persisted record that the worker ran the reap while the bindings
	// table was hidden and the reap failed there -- the exact shape the
	// queue's retries exist to converge. Waiting for that record (instead
	// of restoring after a fixed pause, or restoring immediately and
	// hoping a poll landed in the window) makes the retried-convergence
	// property this test exists to prove unconditional.
	testkit.Eventually(t, "a reap attempt to fail against the hidden bindings table", func() bool {
		rows := jobRows(t, db, taskTypeReapMember)
		return len(rows) == 1 && rows[0]["status"] == string(jobs.StatusRetrying)
	})
	if err := db.Exec("ALTER TABLE rbac_role_bindings_hidden RENAME TO rbac_role_bindings").Error; err != nil {
		t.Fatalf("restoring the bindings table: %v", err)
	}

	testkit.Eventually(t, "the queued reap to revoke the binding once the table is back", func() bool {
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
	ctx := testkit.TenantCtx("tenant-a")

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

// failingQueue is a jobs.Queue whose Enqueue always fails with err -- the
// "the jobs store is momentarily unavailable" case, which is exactly what
// a transient database failure looks like from the subscriber's side. Get
// and Cancel share the same failure so no method of the interface ever
// reports success. calls counts Enqueue invocations, so a test can prove
// the enqueue path was taken (and failed) rather than the no-queue path.
type failingQueue struct {
	err   error
	calls int
}

func (q *failingQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.calls++
	return "", q.err
}

func (q *failingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) {
	return nil, q.err
}

func (q *failingQueue) Cancel(context.Context, jobs.JobID) error {
	return q.err
}

func TestService_OnMemberRemoved_EnqueueFailure_FallsBackToTheSynchronousReap(t *testing.T) {
	// The enqueue path's own failure shape: the queue's Enqueue fails --
	// the jobs store is a database like any other, and this is the same
	// transient-failure class the queue's retries exist to converge, only
	// with no retry to converge it, since a task that never landed cannot
	// be retried. Without a fallback the failed enqueue would end the
	// reaping: the handler must return nil, the bus never redelivers, and
	// the removed member's bindings would stay live forever. The
	// subscriber falls back to the synchronous reaping instead.
	db := newRBACTestDB(t)
	q := &failingQueue{err: errors.New("jobs store unavailable")}
	svc, reg := attachTestService(t, db, WithQueue(q))

	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, removed, "reader", Scope{}, "notes:read")
	grant(t, svc, removed, "writer", Scope{NodeID: "node-1"}, "notes:write")

	rec := recordEvents(reg)
	publishMemberRemoved(t, reg, removed.TenantID, removedMember{
		MembershipID: "membership-1",
		UserID:       removed.UserID,
	})

	if q.calls != 1 {
		t.Fatalf("the subscriber called Enqueue %d times, want 1 -- the enqueue path must have been taken and failed", q.calls)
	}
	// The in-memory bus runs the subscriber synchronously inside Publish,
	// so when the helper returns the fallback has already run.
	if rows := liveBindings(t, svc, removed.TenantID, removed.UserID); len(rows) != 0 {
		t.Fatalf("the removal's reap did not fall back: %d live bindings remain after the enqueue failure, want 0", len(rows))
	}
	if got := len(rec.OfType(EventRoleBindingRevoked)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (the synchronous fallback revokes one binding at a time)", got, EventRoleBindingRevoked)
	}
	if ok, _ := svc.Can(context.Background(), removed, "read", "notes"); ok {
		t.Fatal("Can stayed true after the enqueue failure -- the fallback did not reap")
	}
}

func TestService_OnNodeDeleted_EnqueueFailure_FallsBackToTheSynchronousReap(t *testing.T) {
	// The node-deletion reap's own leg of the same fallback: a queue whose
	// Enqueue always fails must not leave the bindings scoped to a deleted
	// node live -- the subscriber reaps synchronously instead, exactly as
	// the member-removal side does.
	db := newRBACTestDB(t)
	q := &failingQueue{err: errors.New("jobs store unavailable")}
	svc, reg := attachTestService(t, db, WithQueue(q))

	holder := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, holder, "reader", Scope{NodeID: "node-1"}, "notes:read")
	grant(t, svc, holder, "editor", Scope{NodeID: "node-2"}, "notes:write")

	rec := recordEvents(reg)
	bus := reg.Events.Bus()
	if err := bus.Publish(pkgcore.WithTenant(context.Background(), "tenant-a"), pkgcore.Event{
		Type:     eventNodeDeleted,
		TenantID: "tenant-a",
		Payload:  nodeDeleted{DeletedNodeIds: []string{"node-1", "node-2"}},
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventNodeDeleted, err)
	}

	if q.calls != 1 {
		t.Fatalf("the subscriber called Enqueue %d times, want 1 -- the enqueue path must have been taken and failed", q.calls)
	}
	if rows := liveBindings(t, svc, "tenant-a", "user-1"); len(rows) != 0 {
		t.Fatalf("the deletion's reap did not fall back: %d live bindings remain after the enqueue failure, want 0", len(rows))
	}
	if got := len(rec.OfType(EventRoleBindingRevoked)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (the synchronous fallback revokes one binding at a time)", got, EventRoleBindingRevoked)
	}
}
