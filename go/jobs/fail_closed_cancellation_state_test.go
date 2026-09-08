// This file lives in package jobs_test — the external test package,
// distinct from the internal package jobs — for the identical mechanical
// reason queue_conformance_test.go documents: it must import
// go/jobs/queuetest, which itself imports go/jobs, and an internal test
// file importing a package that imports jobs back is an import cycle Go's
// toolchain refuses.
package jobs_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queuetest"
)

// TestStandaloneQueue_FailsClosedOnUnreadableCancellationState is the
// standalone deployment mode's leg of the queuetest injectable-fault tier:
// it runs queuetest.AssertFailsClosedOnUnreadableCancellationState —
// the suite proving a Queue whose cancellation-state read fails fails
// closed on its reporting surfaces — against StandaloneQueue with a REAL
// injected read failure. StandaloneQueue keeps every byte of a Job's state
// in one jobs-table row, so its cancellation-state read is that row read
// and no second source exists to fail independently: the leg's injection
// therefore breaks the row read itself, by renaming the jobs table out from
// under the running queue (the store.go jobsTable name reproduced verbatim,
// the identical drift-loud discipline marker_read_fail_closed_test.go's
// marker-key reproduction applies). Every read the queue makes — Get,
// DeadLetterJobs, and the dispatcher's claim polls alike — answers "no such
// table" for the outage, and the rename back repairs it: the fail-closed
// baseline the finding this tier closes records (standalone already failed
// closed; it was asynq that diverged) is verified here against the queue's
// genuine behaviour, not asserted by inspection.
//
// The queue keeps running through the outage on purpose: the dispatcher
// and writer-heartbeat loops log their per-tick read errors and continue
// (worker.go's dispatchOnce and heartbeat), exactly as they do for any
// transient database hiccup, so the checks observe the reporting surfaces
// mid-outage the way a caller would. The sabotage is a whole-queue fault
// (there is no per-Job second read to break), so the adapter ignores the id
// its methods receive — queuetest.CancellationStateFault is id-addressable
// because asynq's marker is; a leg whose read is global documents that it
// is.
func TestStandaloneQueue_FailsClosedOnUnreadableCancellationState(t *testing.T) {
	queuetest.AssertFailsClosedOnUnreadableCancellationState(t, func() queuetest.FaultRunnable {
		db := dbtest.NewSQLite(t)
		q := jobs.NewStandaloneQueue(db,
			jobs.WithPollInterval(15*time.Millisecond),
			jobs.WithBackoff(20*time.Millisecond, 200*time.Millisecond),
		)
		return &standaloneFaultQueue{StandaloneQueue: q, db: db}
	})
}

// standaloneFaultQueue is the standalone leg's test-side adapter: the
// concrete queue embedded (so its RegisterHandler/Start/Close/DeadLetterJobs
// promote unchanged) plus the *gorm.DB handle the test itself created (the
// queue's own db field is private and must stay so), implementing
// queuetest.CancellationStateFault by renaming the jobs table out from
// under the queue and back.
type standaloneFaultQueue struct {
	*jobs.StandaloneQueue
	db *gorm.DB
}

// jobsTableForFault is store.go's jobsTable literal ("jobs"),
// reproduced verbatim for the rename — exactly how
// marker_read_fail_closed_test.go reproduces queue/asynq's cancelMarkerKey.
// If the constant ever drifts, the sabotage stops failing the read and the
// checks time out or fail loudly rather than passing silently.
const jobsTableForFault = "jobs"

// jobsTableSabotageAlias is where the table lives during the outage; a name
// no schema code ever creates, so the rename can never collide.
const jobsTableSabotageAlias = "jobs_fault_tier_sabotaged"

// Sabotage implements queuetest.CancellationStateFault: renaming the jobs
// table makes every state read answer "no such table" until Repair renames
// it back — a real read failure on the real store the queue reads through.
// The id is unused: StandaloneQueue's cancellation state lives in the one
// row, so the read this breaks is the row read itself (whole-queue scope).
func (f *standaloneFaultQueue) Sabotage(ctx context.Context, _ jobs.JobID) error {
	if err := f.db.WithContext(ctx).Exec("ALTER TABLE " + jobsTableForFault + " RENAME TO " + jobsTableSabotageAlias).Error; err != nil {
		return err
	}
	return nil
}

// Repair implements queuetest.CancellationStateFault: renaming the table
// back restores every read, with the rows — and the cancelled Job's
// status — exactly as the sabotage left them.
func (f *standaloneFaultQueue) Repair(ctx context.Context, _ jobs.JobID) error {
	if err := f.db.WithContext(ctx).Exec("ALTER TABLE " + jobsTableSabotageAlias + " RENAME TO " + jobsTableForFault).Error; err != nil {
		return err
	}
	return nil
}
