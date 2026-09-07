package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestComplianceSweepVsErasure_ConcurrentRemovalOfTheSameRow_ConvergesClean
// pins the P2-9 race tolerance both of notes' destructive participant
// callbacks now carry (see internal/notes/retention_participant.go's
// hardDeleteSaysGone): when a retention sweep and an on-demand erasure
// converge on the SAME rows -- a soft-deleted note past the retention
// cutoff is exactly what both target -- whichever removes a row first
// makes the other's Repository.HardDelete, issued after its own candidate
// list was already gathered, answer dbkit's record-not-found. That answer
// must be treated as "the data is gone" (convergence), never surfaced as
// compliance's ErrErasurePartialFailure or ErrSweepPartialFailure, which
// is what a partial failure of a real operation means -- and what the
// participant's callbacks reported before this fix, whenever the two
// orchestrators raced.
//
// The race is driven for real, in both directions, over real notes rows
// and through the wired compliance module buildServer returns. Each
// direction interleaves the two actors deterministically around the only
// observable phase boundary the loser's callback exposes: the candidate
// LIST precedes its first HardDelete, and the first row-count drop can
// only happen after that list completed. The test therefore waits until
// the goroutine's own deletes have begun (physical count < total -- its
// list is done, so every remaining row is one it will still try to
// delete), then removes all remaining rows itself in one transaction on
// the second connection, standing in for the other orchestrator's
// concurrent pass. Whatever the goroutine deletes next then answers
// record-not-found -- pre-fix, an error; post-fix, an already-removed row
// that converges clean. (The one interleaving that could hide the pre-fix
// failure -- the goroutine finishing every remaining delete between the
// poll's observation and the bulk transaction's commit -- would need
// hundreds of per-row SQLite commit transactions to complete inside the
// poll interval's milliseconds, which the real database makes
// effectively impossible; post-fix the assertions hold under every
// interleaving, which is the property the fix actually ships.)
func TestComplianceSweepVsErasure_ConcurrentRemovalOfTheSameRow_ConvergesClean(t *testing.T) {
	srv, cfg, complianceModule := buildTestServer(t)

	acmeToken := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "compliance-race")
	db := openSecondDB(t, cfg)

	// total notes per direction: large enough that the orchestrator's
	// per-row delete phase spans far longer than the test's poll interval
	// and its one bulk transaction, small enough to keep the suite fast.
	const total = 250
	past := time.Now().Add(-45 * 24 * time.Hour)

	// backdatedSoftDeleteCreatorNotes creates total notes by creator in
	// tenant-acme, each soft-deleted and backdated 45 days -- past the
	// 30-day default retention window -- so every row is simultaneously
	// sweep-eligible (soft-deleted past cutoff) and erasure-eligible
	// (owned by the subject).
	backdatedSoftDeleteCreatorNotes := func(creator string) {
		for i := 0; i < total; i++ {
			id := createNoteAsByCreator(t, srv, acmeToken, creator, "race note "+strconv.Itoa(i))
			softDeleteAndBackdate(t, db, "tenant-acme", id, past)
		}
	}

	// creatorNoteRowCount counts the physical (soft-deleted or not) rows of
	// one creator in tenant-acme -- the race's progress signal, read from
	// the test's own second connection.
	creatorNoteRowCount := func(creator string) int64 {
		t.Helper()
		ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
		var count int64
		err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
			return tx.Unscoped().Model(&notes.Note{}).Where("creator_user_id = ?", creator).Count(&count).Error
		})
		if err != nil {
			t.Fatalf("count rows of creator %s: %v", creator, err)
		}
		return count
	}

	// removeAllCreatorRows physically deletes every remaining row of one
	// creator in one transaction on the second connection -- the test's
	// stand-in for the OTHER orchestrator's concurrent pass, started once
	// the goroutine's own deletes have begun (the same test-only reach
	// softDeleteAndBackdate documents: a second-connection statement the
	// product has no API for, in this case standing in for the concurrent
	// orchestrator's per-row HardDeletes).
	removeAllCreatorRows := func(creator string) {
		t.Helper()
		ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
		err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
			return tx.Unscoped().Where("creator_user_id = ?", creator).Delete(&notes.Note{}).Error
		})
		if err != nil {
			t.Fatalf("bulk-remove rows of creator %s: %v", creator, err)
		}
	}

	// waitForDeletePhaseBegin blocks until the goroutine's own deletes have
	// reduced the creator's physical row count below total -- the signal
	// that its candidate list completed (a row count can only drop through
	// its own HardDeletes) and that every row it still holds is one it
	// will still try to delete.
	waitForDeletePhaseBegin := func(creator string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for creatorNoteRowCount(creator) >= total {
			if time.Now().After(deadline) {
				t.Fatalf("the orchestrator's deletes never began within 30s (rows still %d of %d)", creatorNoteRowCount(creator), total)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// Direction A -- an on-demand erasure (the goroutine) racing a
	// concurrent sweep-shaped removal of the same rows (the test's bulk
	// delete): the erasure's HardDelete of an already-removed row must not
	// surface ErrErasurePartialFailure.
	t.Run("erasure racing a concurrent sweep-shaped removal converges clean", func(t *testing.T) {
		const creator = demoNotesCreatorUserID
		backdatedSoftDeleteCreatorNotes(creator)

		type outcome struct {
			result compliance.ErasureResult
			err    error
		}
		ch := make(chan outcome, 1)
		// The ctx must carry the subject's own tenant: Erase erases within
		// the tenant ctx is scoped to -- the SubjectRef may only echo it
		// back (go/compliance's Erase tenant gate).
		ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
		go func() {
			result, err := complianceModule.Erasure().Erase(ctx, pkgcore.SubjectRef{
				TenantID:  "tenant-acme",
				SubjectID: creator,
			}, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "compliance-race-test", DisplayName: "Compliance race test"})
			ch <- outcome{result: result, err: err}
		}()

		waitForDeletePhaseBegin(creator)
		removeAllCreatorRows(creator)

		got := <-ch
		if got.err != nil {
			t.Fatalf("Erase racing the concurrent removal: error = %v, want nil (an already-removed row is convergence, not a partial failure)", got.err)
		}
		if got.result.HasErrors() {
			t.Fatalf("Erase errors = %v, want none", got.result.Errors)
		}
		if remaining := creatorNoteRowCount(creator); remaining != 0 {
			t.Fatalf("%d of the subject's rows still physically present after the raced erasure, want 0", remaining)
		}

		// Re-running the same erasure converges with (0, nil): nothing the
		// race left behind, and nothing the fix left half-done.
		again, err := complianceModule.Erasure().Erase(ctx, pkgcore.SubjectRef{
			TenantID:  "tenant-acme",
			SubjectID: creator,
		}, pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "compliance-race-test", DisplayName: "Compliance race test"})
		if err != nil {
			t.Fatalf("second Erase: %v", err)
		}
		if again.HasErrors() {
			t.Fatalf("second Erase errors = %v, want none", again.Errors)
		}
	})

	// Direction B -- a retention sweep (the goroutine) racing a concurrent
	// erasure-shaped removal of the same rows (the test's bulk delete): the
	// sweep's HardDelete of an already-removed row must not surface
	// ErrSweepPartialFailure.
	t.Run("sweep racing a concurrent erasure-shaped removal converges clean", func(t *testing.T) {
		const creator = complianceOtherCreatorUserID
		backdatedSoftDeleteCreatorNotes(creator)

		type outcome struct {
			result compliance.SweepResult
			err    error
		}
		ch := make(chan outcome, 1)
		go func() {
			result, err := complianceModule.Retention().SweepTenant(context.Background(), "tenant-acme")
			ch <- outcome{result: result, err: err}
		}()

		waitForDeletePhaseBegin(creator)
		removeAllCreatorRows(creator)

		got := <-ch
		if got.err != nil {
			t.Fatalf("SweepTenant racing the concurrent removal: error = %v, want nil (an already-removed row is convergence, not a partial failure)", got.err)
		}
		if got.result.HasErrors() {
			t.Fatalf("SweepTenant errors = %v, want none", got.result.Errors)
		}
		if remaining := creatorNoteRowCount(creator); remaining != 0 {
			t.Fatalf("%d sweep-eligible rows still physically present after the raced sweep, want 0", remaining)
		}

		// Re-running the same sweep converges with (0, nil).
		again, err := complianceModule.Retention().SweepTenant(context.Background(), "tenant-acme")
		if err != nil {
			t.Fatalf("second SweepTenant: %v", err)
		}
		if again.HasErrors() {
			t.Fatalf("second SweepTenant errors = %v, want none", again.Errors)
		}
	})
}
