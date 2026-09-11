//go:build integration

package org_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the deterministic PostgreSQL proofs behind
// Repository.deleteSubtree's doc comment ("Why the id set is captured via
// lockSubtree, not a plain Find", repository.go): a capture of
// DeletedNodeIds -- the per-row id set published on org.node.deleted, the
// set rbac's onNodeDeleted reaper reaps role bindings by -- taken with one
// plain, unlocked "path LIKE prefix%" Find sitting between nodeID's own
// lock and the cascade's mark-delete UPDATE would race, under PostgreSQL's
// READ COMMITTED isolation, against a concurrent writer of an INTERIOR
// descendant (CreateChild/Move/Restore lock only the row they act on,
// never nodeID), in both directions:
//
//   - OVER-count: a Move that carries an interior descendant OUT of the
//     subtree and commits inside the gap leaves the descendant's id captured
//     by the Find yet no longer matched by the UPDATE -- PostgreSQL's EvalPlanQual
//     re-check skips the row the moment its committed path stops matching the
//     WHERE clause -- so org.node.deleted falsely reports a node the cascade
//     never removed, and rbac would revoke the bindings of a still-live node.
//   - UNDER-count: a Move (or CreateChild) that lands a node INTO the subtree
//     and commits inside the gap leaves that node invisible to the Find's
//     snapshot, yet if the UPDATE's own statement snapshot runs before the
//     move commits, the node also never enters the UPDATE's candidate set: it
//     survives as a LIVE row under a parent the cascade just mark-deleted --
//     an orphan whose bindings rbac never reaps because no event names it.
//
// The postgres_concurrency_test.go races in this directory never covered
// either interleaving: they pair whole operations with no interior
// descendant landing exactly between two statements of one transaction, and
// they only assert the tree invariant, never the event's id set.
//
// A wall-clock race cannot reproduce these windows deterministically: the
// gap between the unlocked Find and the UPDATE is a few instructions wide.
// The proofs here instead orchestrate the interleaving through ROW LOCKS, so
// each statement parks at a precisely known place and the release order is
// forced, never raced:
//
//   - A third transaction T (the test itself) holds a row lock on a node the
//     Move must pass through, so M (the Move) is guaranteed to be parked
//     mid-transaction -- its node and destination locked, nothing rewritten
//     yet -- before X (the cascading Delete) is even started. The queue is
//     observed through pg_locks: a blocked UPDATE appears there as a
//     non-granted transactionid lock waiting on the row holder's xid.
//   - X is started only once M is confirmed parked, so X's own scan/UPDATE
//     statements deterministically run against M's not-yet-committed state,
//     and X's UPDATE parks on M's row lock (count==2, the barrier for "X is
//     blocked mid-delete, its statement snapshot already taken").
//   - T commits. Only M is woken (the sequential-wake property -- a Move is
//     chosen over a Restore deliberately, because two transactions queued on
//     the SAME holder's row would wake together and race each other for it,
//     a coin flip no test may depend on). M completes its move and commits,
//     and only then is X woken: X's mark-delete statement resumes against
//     M's committed result. The outcome is a forced, reproducible failure on
//     the plain-Find shape and a consistent one on the lockSubtree shape,
//     in three rounds
//     each on fresh tenants.
//
// Against the plain-Find version of deleteSubtree, Test
// ...MoveOutDuringCascade fails the event-set assertion of
// assertCascadeDeleteConsistency (the event names the moved-out node the
// UPDATE skipped) and ...MoveInDuringCascade fails its no-live-rows
// assertion (the moved-in nodes survive under the mark-deleted parent).
// Against the lockSubtree shape both tests pass: lockSubtree holds every
// subtree row's lock to a fixed point before the UPDATE, so the row set it
// returns is exactly the row set the UPDATE matches -- a writer that
// committed into the subtree is re-scanned and locked, a writer that moved
// a row out loses it from the re-scan before the UPDATE ever runs.

// holdNodeRowLockTx opens a transaction on db and write-locks the live row
// nodeID with the identical no-op UPDATE touchLockByID (repository.go) uses,
// so any later operation that must pass through that row -- Move's
// lockLiveNode, a cascade's own touch -- parks on this transaction's xid
// until the test commits or rolls it back. The caller commits the returned
// transaction once the orchestration has reached the barrier it exists for;
// a test failure before that still releases the lock, because t.Cleanup
// rolls the transaction back.
func holdNodeRowLockTx(t *testing.T, db *gorm.DB, ctx context.Context, nodeID string) *gorm.DB {
	t.Helper()
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("Begin: %v", tx.Error)
	}
	res := tx.
		Where("id = ?", nodeID).
		Where("deleted_at IS NULL").
		Select("DeletedBy").
		Updates(&org.OrgNode{DeletedBy: ""})
	if res.Error != nil {
		_ = tx.Rollback().Error
		t.Fatalf("locking row %s: %v", nodeID, res.Error)
	}
	if res.RowsAffected != 1 {
		_ = tx.Rollback().Error
		t.Fatalf("locking row %s: rows affected %d, want 1 (row missing or already soft-deleted?)",
			nodeID, res.RowsAffected)
	}
	t.Cleanup(func() { _ = tx.Rollback().Error })
	return tx
}

// pgLockWaiters counts the transactions currently blocked on a row lock held
// by someone else: PostgreSQL exposes each such waiter as a non-granted
// locktype='transactionid' row in pg_locks. The count is the barrier the
// deterministic orchestration below waits on. The query is raw SQL on a
// system view, which dbkit's tenant-scope plugin deliberately does not
// intercept (go/dbkit/tenant_scope.go), and every test in this package runs
// on its own disposable server, so no other test's transactions can leak
// into the count.
func pgLockWaiters(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	if err := db.Raw("SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND NOT granted").Scan(&n).Error; err != nil {
		t.Fatalf("counting pg_locks waiters: %v", err)
	}
	return n
}

// waitForPgLockWaiters polls until exactly want transactions are queued on a
// held row lock, then returns; it fails the test if the count never reaches
// the barrier (the operation it expected to park failed or finished instead,
// which means the orchestration is no longer meaningful).
func waitForPgLockWaiters(t *testing.T, db *gorm.DB, want int, label string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := pgLockWaiters(t, db)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out waiting for %d queued row-lock waiter(s); last count %d", label, want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// deleteEventCapture records every org.node.deleted event the memory bus
// delivers, keeping the publisher's own payload struct -- the in-process bus
// passes payloads through un-marshalled, unlike the distributed mode's bus.
// Handlers run synchronously on the publisher's goroutine, so after a test's
// WaitGroup drains, everything the Delete goroutine published is visible
// here; the mutex exists for the record's own safety rather than for any
// current reader.
type deleteEventCapture struct {
	mu     sync.Mutex
	events []struct {
		tenant  pkgcore.TenantID
		payload org.NodeDeleted
	}
}

func (c *deleteEventCapture) handle(_ context.Context, evt pkgcore.Event) error {
	if evt.Type != org.EventNodeDeleted {
		return nil
	}
	payload, ok := evt.Payload.(org.NodeDeleted)
	if !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, struct {
		tenant  pkgcore.TenantID
		payload org.NodeDeleted
	}{evt.TenantID, payload})
	return nil
}

// nodeDeleted returns the recorded org.node.deleted payload for one tenant's
// deleted node id, so a test can assert on exactly the event its own delete
// produced and ignore anything an earlier round of another tenant published.
func (c *deleteEventCapture) nodeDeleted(tenant pkgcore.TenantID, nodeID string) (org.NodeDeleted, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.tenant == tenant && e.payload.NodeID == nodeID {
			return e.payload, true
		}
	}
	return org.NodeDeleted{}, false
}

// wiredOrgTree bootstraps org's module against db on a memory bus -- the
// wiring a real host uses, Register included, so TreeService.Delete publishes
// org.node.deleted onto the bus the returned capture listens to -- and
// returns the module's tree plus the capture. Every test in this file needs
// the tree to go through this module rather than org.NewTreeService(db)
// directly: an unwired tree has no host, and publishEvent is a silent no-op
// without one (events.go), which would make the event assertions vacuous.
func wiredOrgTree(t *testing.T, db *gorm.DB) (*org.TreeService, *org.MemberService, *deleteEventCapture) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := componenttest.NewRegistry()
	reg.Put(bus)
	m := org.NewModule(db, org.WithEmailIndexer(newIndexer(t)), org.WithInvitationEmailDisabled())
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("module Register: %v", err)
	}
	capture := &deleteEventCapture{}
	bus.Subscribe(org.EventNodeDeleted, capture.handle)
	return m.Tree(), m.Members(), capture
}

// subtreeRowsUnscoped returns every org_nodes row whose path lies under
// prefix -- live and mark-deleted alike -- through the same tenant-scoped
// scan shape deleteSubtree's own statements use (the tenant filter comes
// from ctx; Unscoped lifts only the soft-delete auto-scope, because a
// consistency assertion must see the rows the cascade hid).
func subtreeRowsUnscoped(t *testing.T, db *gorm.DB, ctx context.Context, prefix string) []org.OrgNode {
	t.Helper()
	var rows []org.OrgNode
	if err := db.WithContext(ctx).Unscoped().
		Where("path LIKE ?", prefix+"%").
		Order("id").
		Find(&rows).Error; err != nil {
		t.Fatalf("subtree scan under %q: %v", prefix, err)
	}
	return rows
}

// assertCascadeDeleteConsistency checks the two directions of the race the
// plain-Find deleteSubtree would lose, and nothing else: after the delete
// has returned, no live row may remain under prefix (a cascade delete that
// orphans a just-moved-in node fails here), and the org.node.deleted event's
// DeletedNodeIds must equal the set of rows actually mark-deleted under
// prefix, with RemovedCount its length (a cascade that skipped a row it
// claimed to delete fails here). Both assertions are what a real subscriber
// -- rbac's onNodeDeleted reaper, which revokes role bindings exactly for
// the ids DeletedNodeIds names -- depends on: an event that omits a removed
// id leaks a dangling binding, one that names a surviving node revokes a
// live grant.
func assertCascadeDeleteConsistency(t *testing.T, db *gorm.DB, ctx context.Context, prefix string, evt org.NodeDeleted, label string) {
	t.Helper()
	rows := subtreeRowsUnscoped(t, db, ctx, prefix)
	var live, deleted []string
	for _, n := range rows {
		if n.DeletedAt == nil {
			live = append(live, n.ID)
		} else {
			deleted = append(deleted, n.ID)
		}
	}
	if len(live) != 0 {
		t.Fatalf("%s: %d live row(s) remain under prefix %q after the cascading delete returned: %v",
			label, len(live), prefix, live)
	}
	sort.Strings(deleted)
	named := slices.Clone(evt.DeletedNodeIds)
	sort.Strings(named)
	if !slices.Equal(named, deleted) {
		t.Fatalf("%s: org.node.deleted names ids %v, but the rows actually mark-deleted under %q are %v -- the event set and the real set disagree",
			label, named, prefix, deleted)
	}
	if evt.RemovedCount != int64(len(deleted)) {
		t.Fatalf("%s: org.node.deleted RemovedCount = %d, but %d rows are actually mark-deleted under %q",
			label, evt.RemovedCount, len(deleted), prefix)
	}
}

// TestDeleteSubtreeEventIDs_MoveOutDuringCascade_NoOvercount_Postgres is the
// deterministic over-count proof: a Move that carries an interior descendant
// OUT of the deleted subtree and commits between the deletedIds capture and
// the mark-delete UPDATE. See the file header for the orchestration; the
// roles are T = this test, holding the lock on E that the Move's destination
// parent lock must wait on; M = Move(D, E), parked with D locked while T is
// held; X = the cascading Delete(A), parked on D's lock with its capture
// (plain Find) or lockSubtree scan already done. When T
// commits, M relocates D under E and commits before X resumes; the plain
// Find had already captured D's id, but the UPDATE's EvalPlanQual re-check
// skips D the moment its committed path no longer matches the prefix -- the
// event over-counts, and only assertCascadeDeleteConsistency's
// event-set-vs-reality comparison catches it. The current code re-scans
// after D's lock is taken, sees D leave the subtree, and reports exactly
// the rows it deletes.
func TestDeleteSubtreeEventIDs_MoveOutDuringCascade_NoOvercount_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree, _, events := wiredOrgTree(t, db)

	const rounds = 3
	for round := 0; round < rounds; round++ {
		label := fmt.Sprintf("round %d", round)
		tenant := pkgcore.TenantID(fmt.Sprintf("tenant-move-out-%d", round))
		ctx := tenantCtx(tenant)

		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("%s: CreateRoot: %v", label, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(a): %v", label, err)
		}
		d, err := tree.CreateChild(ctx, a.ID, "d", "store")
		if err != nil {
			t.Fatalf("%s: CreateChild(d): %v", label, err)
		}
		e, err := tree.CreateChild(ctx, root.ID, "e", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(e): %v", label, err)
		}

		// T holds E, so M's lockLiveNode on its destination parent parks it
		// after it has already locked D -- the exact mid-Move state whose
		// eventual commit a plain-Find capture could not survive.
		held := holdNodeRowLockTx(t, db, ctx, e.ID)

		var (
			wg        sync.WaitGroup
			moveErr   error
			deleteErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, moveErr = tree.Move(ctx, d.ID, e.ID)
		}()
		waitForPgLockWaiters(t, db, 1, label+": Move parked on the held destination parent")
		go func() {
			defer wg.Done()
			deleteErr = tree.Delete(ctx, a.ID, true)
		}()
		waitForPgLockWaiters(t, db, 2, label+": Delete parked on the in-flight Move's subtree lock")
		if err := held.Commit().Error; err != nil {
			t.Fatalf("%s: committing the held-parent transaction: %v", label, err)
		}
		wg.Wait()

		if moveErr != nil {
			t.Fatalf("%s: Move(D, E) = %v, want success", label, moveErr)
		}
		if deleteErr != nil {
			t.Fatalf("%s: Delete(A, cascade) = %v, want success", label, deleteErr)
		}
		// D moved out in time: the cascade must not have touched it, and the
		// tree must still serve it live under its new parent.
		if _, err := tree.Get(ctx, d.ID); err != nil {
			t.Fatalf("%s: moved-out node D is not live after the cascade: %v", label, err)
		}

		evt, ok := events.nodeDeleted(tenant, a.ID)
		if !ok {
			t.Fatalf("%s: no org.node.deleted event recorded for deleted node %s", label, a.ID)
		}
		if !evt.Cascade {
			t.Fatalf("%s: org.node.deleted Cascade = false, want true", label)
		}
		assertCascadeDeleteConsistency(t, db, ctx, a.Path, evt, label)
	}
}

// TestDeleteSubtreeEventIDs_MoveInDuringCascade_NoUnderCount_Postgres is the
// deterministic under-count proof: a Move that lands a node -- with its own
// subtree -- INTO the deleted subtree and commits while the cascade is
// parked on its locks. Roles: T = this test, holding the lock on D2 (B's
// child) that M's lockSubtree must wait on, which also proves M has already
// locked B and its destination C by the time X starts; M = Move(B, C); X =
// the cascading Delete(A). When T commits, M rewrites B and D2 under C and
// commits before X resumes; a plain Find never saw B or D2, and the
// mark-delete UPDATE's statement snapshot -- taken while M was still parked
// -- predates M's commit, so B and D2 are not even candidates for it: they
// survive as LIVE rows under the mark-deleted C, a deletion the event set
// (which matches reality) cannot expose to any subscriber. Only
// assertCascadeDeleteConsistency's no-live-rows assertion catches it.
// lockSubtree re-scans once C's lock is taken, discovers the
// newly arrived B and D2, locks them, and includes them in both the deleted
// set and the UPDATE.
func TestDeleteSubtreeEventIDs_MoveInDuringCascade_NoUnderCount_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree, _, events := wiredOrgTree(t, db)

	const rounds = 3
	for round := 0; round < rounds; round++ {
		label := fmt.Sprintf("round %d", round)
		tenant := pkgcore.TenantID(fmt.Sprintf("tenant-move-in-%d", round))
		ctx := tenantCtx(tenant)

		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("%s: CreateRoot: %v", label, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(a): %v", label, err)
		}
		c, err := tree.CreateChild(ctx, a.ID, "c", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(c): %v", label, err)
		}
		b, err := tree.CreateChild(ctx, root.ID, "b", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(b): %v", label, err)
		}
		d2, err := tree.CreateChild(ctx, b.ID, "d2", "store")
		if err != nil {
			t.Fatalf("%s: CreateChild(d2): %v", label, err)
		}

		// T holds D2. M's lockSubtree over B's prefix must pass through D2,
		// so the barrier "one waiter" means M is parked with B AND C locked --
		// exactly the state X must find C in.
		held := holdNodeRowLockTx(t, db, ctx, d2.ID)

		var (
			wg        sync.WaitGroup
			moveErr   error
			deleteErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, moveErr = tree.Move(ctx, b.ID, c.ID)
		}()
		waitForPgLockWaiters(t, db, 1, label+": Move parked on the held descendant")
		go func() {
			defer wg.Done()
			deleteErr = tree.Delete(ctx, a.ID, true)
		}()
		waitForPgLockWaiters(t, db, 2, label+": Delete parked on the in-flight Move's subtree lock")
		if err := held.Commit().Error; err != nil {
			t.Fatalf("%s: committing the held-descendant transaction: %v", label, err)
		}
		wg.Wait()

		if moveErr != nil {
			t.Fatalf("%s: Move(B, C) = %v, want success", label, moveErr)
		}
		if deleteErr != nil {
			t.Fatalf("%s: Delete(A, cascade) = %v, want success", label, deleteErr)
		}

		evt, ok := events.nodeDeleted(tenant, a.ID)
		if !ok {
			t.Fatalf("%s: no org.node.deleted event recorded for deleted node %s", label, a.ID)
		}
		if !evt.Cascade {
			t.Fatalf("%s: org.node.deleted Cascade = false, want true", label)
		}
		// The whole relocated subtree lands in the deleted set with A and C:
		// the event names B and D2 and no live row survives under A's prefix.
		// With a plain Find, B and D2 survive live under the mark-deleted C
		// and fail the assertion below.
		assertCascadeDeleteConsistency(t, db, ctx, a.Path, evt, label)
	}
}

// TestDeleteSubtree_MemberAddToInteriorDescendant_DuringCascade_NoDanglingMembership_Postgres
// is the deterministic member-guard proof: deleteSubtree's member guard
// runs right after nodeID's own lock but BEFORE lockSubtree ever locks the
// interior descendants of the subtree. A concurrent MemberService.Add
// targeting an INTERIOR descendant -- never the deleted node itself -- takes
// only lockLiveNode on THAT row (membership.go's ensure), a row the cascade
// had not touched yet at guard time, so Add can commit a membership into
// the window between the guard's read and the cascade's eventual sweep of
// the descendant: the membership survives bound to a row the cascade then
// mark-deleted. This is the same TOCTOU family the member-guard round closed
// for the deleted node itself, still open for its descendants -- and exactly
// the window SQLite's whole-file locking papered over (a second writer parks
// at the file lock before its guard-relevant statements run), which is why
// this proof lives against a real PostgreSQL server, where the two writers
// share no lock at all until the cascade reaches the descendant's row.
//
// # Orchestration
//
// Roles: T = this test, holding the row lock on the smaller-id interior
// descendant (the first one the cascade's lockSubtree scan, ordered by
// (depth, id), will try to touch); X = the cascading Delete(A), parked on
// T's hold with the OTHER interior descendant still completely unlocked;
// A2 = MemberService.Add to that other descendant, started only once X is
// confirmed parked (waitForPgLockWaiters(1)), so its lockLiveNode succeeds,
// its membership insert commits, and its transaction ends while X is still
// mid-lockSubtree.
//
// If X's guard ran (before lockSubtree) it would see no members, so once T
// commits and X sweeps the whole subtree, A2's just-committed membership is
// left bound to a mark-deleted row -- the dangling membership the
// assertions below catch. With the guard running only after lockSubtree has
// locked every row of the subtree to a fixed point, A2's committed
// membership is visible to that later guard read and X refuses with
// org.node_has_members, rolling back; the membership and its node both stay
// live.
func TestDeleteSubtree_MemberAddToInteriorDescendant_DuringCascade_NoDanglingMembership_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree, members, _ := wiredOrgTree(t, db)

	const rounds = 3
	for round := 0; round < rounds; round++ {
		label := fmt.Sprintf("round %d", round)
		tenant := pkgcore.TenantID(fmt.Sprintf("tenant-add-race-%d", round))
		ctx := tenantCtx(tenant)

		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("%s: CreateRoot: %v", label, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(a): %v", label, err)
		}
		inner1, err := tree.CreateChild(ctx, a.ID, "inner-1", "store")
		if err != nil {
			t.Fatalf("%s: CreateChild(inner-1): %v", label, err)
		}
		inner2, err := tree.CreateChild(ctx, a.ID, "inner-2", "store")
		if err != nil {
			t.Fatalf("%s: CreateChild(inner-2): %v", label, err)
		}
		// lockSubtree touches the subtree's rows in (depth, id) order, so the
		// first interior row X tries to lock is the one with the smaller id:
		// T holds THAT one (parking X mid-lockSubtree) and A2 targets the
		// other, which X has not touched yet.
		ordered := []*org.OrgNode{inner1, inner2}
		slices.SortFunc(ordered, func(x, y *org.OrgNode) int {
			return strings.Compare(x.ID, y.ID)
		})
		held := holdNodeRowLockTx(t, db, ctx, ordered[0].ID)
		addTarget := ordered[1].ID

		var (
			wg        sync.WaitGroup
			deleteErr error
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			deleteErr = tree.Delete(ctx, a.ID, true)
		}()
		waitForPgLockWaiters(t, db, 1, label+": Delete parked mid-lockSubtree on the held interior descendant")

		userID := fmt.Sprintf("u-add-race-%d", round)
		membership, addErr := members.Add(ctx, userID, addTarget)
		if addErr != nil {
			t.Fatalf("%s: Members().Add(%s, %s) = %v, want success (the interior descendant is still live while the cascade is parked)",
				label, userID, addTarget, addErr)
		}
		if err := held.Commit().Error; err != nil {
			t.Fatalf("%s: committing the held-descendant transaction: %v", label, err)
		}
		wg.Wait()

		if deleteErr != nil {
			if !apperr.HasCode(deleteErr, org.ErrNodeHasMembers.Code) {
				t.Fatalf("%s: Delete = %v, want the coded org.node_has_members refusal once the guard sees the committed membership", label, deleteErr)
			}
			// The tree must still be intact: the refusal rolled everything back.
			if _, err := tree.Get(ctx, a.ID); err != nil {
				t.Fatalf("%s: Delete refused with %v but node A is not live afterward: %v", label, deleteErr, err)
			}
		}

		// The invariant that matters either way: the membership Add created
		// must not be bound to a row the cascade removed. If the cascade won
		// the race, the Add committed into its sweep window and this Get
		// fails -- the dangling membership.
		if _, err := members.Get(ctx, userID); err != nil {
			t.Fatalf("%s: the membership Add reported success but Get failed: %v", label, err)
		}
		if _, err := tree.Get(ctx, membership.NodeID); err != nil {
			t.Fatalf("%s: membership %s is bound to node %s, which is no longer visible (%v) -- dangling membership created by Add racing the cascade",
				label, membership.ID, membership.NodeID, err)
		}
	}
}
