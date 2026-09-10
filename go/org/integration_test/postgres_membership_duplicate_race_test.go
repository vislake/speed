//go:build integration

package org_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds the deterministic PostgreSQL proof behind
// MemberService.ensure's doc comment ("The insert race: the database is
// the backstop, and the recovery must leave the failed transaction
// behind", membership.go): the recovery for a lost insert race must NOT
// re-read the winning membership on the SAME transaction that just
// failed the insert with a unique violation. SQLite tolerates a failed
// statement inside an open transaction; PostgreSQL does not -- a
// unique-violation error aborts the whole transaction, so a follow-up
// read on that transaction dies with SQLSTATE 25P02 ("current transaction
// is aborted, commands ignored until end of transaction block") in exactly
// the concurrent-duplicate case the branch exists to absorb: two
// concurrent ensures of the same (tenant, user) both pass the byUser
// pre-check, the partial unique index admits one insert, and the loser --
// which must answer exactly as a sequential duplicate would, the coded
// org.membership_exists from Add -- would otherwise fail with an internal
// error instead.
//
// # Orchestration
//
// A wall-clock race cannot force the loser reliably onto the duplicate
// branch: the loser's own byUser pre-check would simply find the winner's
// committed row whenever the two calls did not genuinely overlap. The
// orchestration reuses the row-lock barrier machinery of
// postgres_delete_race_test.go with TWO held node locks, one per racing
// Add and each on a DIFFERENT node (both memberships bind the same
// (tenant, user) seat; the unique index arbitrates regardless of which
// node each side targets):
//
//   - T holds a write lock on node A (holdNodeRowLockTx) and another on
//     node B. Two Add calls for the same user -- one targeting A, one
//     targeting B -- each park at their own ensure's lockLiveNode, with
//     each one's byUser pre-check already completed against a memberships
//     table neither has written yet. waitForPgLockWaiters(2) is the
//     barrier that both calls are parked, pre-insert.
//
//   - Each waiter queues on its OWN holder, which is what makes the
//     pg_locks barrier see both: a second transaction parking on the same
//     held row as a first waiter blocks on the row's tuple lock instead
//     (wait_event 'tuple'), which pg_locks never counts -- the reason this
//     orchestration needs two separately held nodes, and the reason
//     postgres_delete_race_test.go always parks at most one operation on
//     each held row.
//
//   - T commits both holds; whichever Add then acquires its node's lock
//     first inserts and commits, and the other's insert collides with the
//     committed row and fails on the unique index -- the duplicate branch,
//     deterministically, in either order: the loser's insert waits on the
//     winner's in-flight row if they overlap, and raises the unique
//     violation once the winner commits either way. Which side wins is a
//     coin flip no assertion depends on.
//
// The loser's insert ends its transaction the moment it loses the race
// and the winner is re-read on a fresh session -- a recovery that re-read
// on the aborted transaction would fail with 25P02 and make Add report
// org.internal_error -- so Add answers the coded org.membership_exists
// and exactly one membership row exists. Three rounds, each on a fresh
// tenant, mirroring the delete-race file's own repetition.
func TestMemberService_ConcurrentAddSameUser_LoserAnswersMembershipExists_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree := org.NewTreeService(db)
	members := org.NewMemberService(db, tree)

	const rounds = 3
	for round := 0; round < rounds; round++ {
		label := fmt.Sprintf("round %d", round)
		tenant := pkgcore.TenantID(fmt.Sprintf("tenant-dup-%d", round))
		ctx := tenantCtx(tenant)

		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("%s: CreateRoot: %v", label, err)
		}
		nodeA, err := tree.CreateChild(ctx, root.ID, "node-a", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(a): %v", label, err)
		}
		nodeB, err := tree.CreateChild(ctx, root.ID, "node-b", "group")
		if err != nil {
			t.Fatalf("%s: CreateChild(b): %v", label, err)
		}

		// T holds both target nodes' row locks, so both racing Adds are
		// guaranteed to park at their own lockLiveNode -- each one's byUser
		// pre-check done, neither insert issued -- before the race below is
		// released.
		heldA := holdNodeRowLockTx(t, db, ctx, nodeA.ID)
		heldB := holdNodeRowLockTx(t, db, ctx, nodeB.ID)

		userID := fmt.Sprintf("u-dup-%d", round)
		var (
			wg      sync.WaitGroup
			addErrs = make([]error, 2)
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, addErrs[0] = members.Add(ctx, userID, nodeA.ID)
		}()
		go func() {
			defer wg.Done()
			_, addErrs[1] = members.Add(ctx, userID, nodeB.ID)
		}()
		waitForPgLockWaiters(t, db, 2, label+": both Add calls parked at their lockLiveNode behind the held node locks")
		if err := heldA.Commit().Error; err != nil {
			t.Fatalf("%s: committing the held node-A transaction: %v", label, err)
		}
		if err := heldB.Commit().Error; err != nil {
			t.Fatalf("%s: committing the held node-B transaction: %v", label, err)
		}
		wg.Wait()

		// Exactly one of the two racing Adds created the membership; the
		// other lost the unique-index race and must answer the same coded
		// org.membership_exists a sequential duplicate would. A recovery
		// that re-read on the aborted transaction would fail with SQLSTATE
		// 25P02 and make Add report org.internal_error right here.
		creators, codedExists := 0, 0
		for i, err := range addErrs {
			switch {
			case err == nil:
				creators++
			case apperr.HasCode(err, org.ErrMembershipExists.Code):
				codedExists++
			default:
				t.Fatalf("%s: concurrent Add %d = %v, want success or the coded org.membership_exists, never an internal error",
					label, i, err)
			}
		}
		if creators != 1 || codedExists != 1 {
			t.Fatalf("%s: two concurrent Adds of one membership = %d creator(s) / %d coded-exists, want exactly 1 of each",
				label, creators, codedExists)
		}

		// One seat per person per tenant: exactly one live membership row
		// must exist afterward, whatever the split above said.
		if _, err := members.Get(ctx, userID); err != nil {
			t.Fatalf("%s: Get after the duplicate race = %v, want the single winning membership", label, err)
		}
	}
}
